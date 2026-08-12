// Команда api поднимает HTTP-сервис пользовательской очереди.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shndo1337/avito-queue/backend/internal/catalog"
	"github.com/shndo1337/avito-queue/backend/internal/checkout"
	"github.com/shndo1337/avito-queue/backend/internal/config"
	"github.com/shndo1337/avito-queue/backend/internal/db"
	"github.com/shndo1337/avito-queue/backend/internal/httpx"
	"github.com/shndo1337/avito-queue/backend/internal/queue"
	"github.com/shndo1337/avito-queue/backend/internal/waitlist"
	"github.com/shndo1337/avito-queue/backend/internal/worker"
	"github.com/shndo1337/avito-queue/backend/migrations"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second
	healthPingTimeout = 2 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("сервис остановлен с ошибкой", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("конфигурация: %w", err)
	}
	setupLogger(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("подключение к базе: %w", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		return fmt.Errorf("миграции: %w", err)
	}

	queueSvc := queue.NewService(pool, cfg.GrantTTL)
	subscriptions := waitlist.NewRepository(pool)

	go worker.NewExpirer(queueSvc, cfg.ExpirerInterval, slog.Default()).Run(ctx)
	go worker.NewNotifier(subscriptions, cfg.ExpirerInterval, slog.Default()).Run(ctx)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           newRouter(&cfg, pool, queueSvc, subscriptions),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("сервис запущен", "addr", cfg.ListenAddr, "grant_ttl", cfg.GrantTTL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("http-сервер: %w", err)
		}
		return nil
	case <-ctx.Done():
		slog.Info("получен сигнал остановки, доигрываем активные запросы")
	}

	// Контекст остановки отдельный: ctx уже отменён сигналом.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("остановка http-сервера: %w", err)
	}
	slog.Info("сервис остановлен штатно")
	return nil
}

func newRouter(
	cfg *config.Config,
	pool *pgxpool.Pool,
	queueSvc *queue.Service,
	subscriptions *waitlist.Repository,
) http.Handler {
	idempotency := httpx.NewIdempotencyStore(pool)

	protected := http.NewServeMux()
	queue.NewHandler(queueSvc).RegisterRoutes(protected)
	checkout.NewHandler(queueSvc).RegisterRoutes(protected)
	waitlist.NewHandler(subscriptions).RegisterRoutes(protected)

	guarded := httpx.Chain(protected,
		httpx.RequireUser,
		httpx.RateLimit(cfg.RateLimitRPS, cfg.RateLimitBurst),
		idempotency.Middleware,
	)

	root := http.NewServeMux()
	root.HandleFunc("GET /health", health(pool))

	catalog.NewHandler(catalog.NewRepository(pool)).RegisterRoutes(root)

	// Граница защиты сценария: всё, что меняет очередь или создаёт заказ.
	root.Handle("/api/products/{id}/queue", guarded)
	root.Handle("/api/products/{id}/queue/status", guarded)
	root.Handle("/api/products/{id}/waitlist", guarded)
	root.Handle("POST /api/checkout", guarded)

	// Метрики про сервис, а не про пользователя.
	root.Handle("GET /api/queue/metrics", protected)

	// Иначе ServeMux отвечает на неизвестный путь простым текстом, и фронт
	// спотыкается при разборе тела как JSON.
	root.HandleFunc("/", notFound)

	return httpx.Chain(root,
		httpx.RequestID,
		httpx.Recoverer,
		httpx.Logger,
		httpx.CORS(cfg.CORSOrigins),
	)
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, "Метод не найден")
}

func health(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthPingTimeout)
		defer cancel()

		if err := pool.Ping(ctx); err != nil {
			slog.WarnContext(ctx, "health: база не отвечает", "error", err)
			httpx.Error(w, http.StatusServiceUnavailable, httpx.CodeInternal, "База данных недоступна")
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func setupLogger(level slog.Level) {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}
