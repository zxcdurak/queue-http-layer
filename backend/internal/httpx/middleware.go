package httpx

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

// Заголовки, по которым сервис узнаёт запрос и пользователя.
const (
	HeaderRequestID = "X-Request-Id"
	HeaderUserID    = "X-User-Id"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyUserID
)

var userIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Chain оборачивает обработчик набором middleware. Первый в списке — внешний.
func Chain(h http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// RequestIDFrom возвращает идентификатор запроса.
func RequestIDFrom(ctx context.Context) string {
	id, ok := ctx.Value(ctxKeyRequestID).(string)
	if !ok {
		return ""
	}
	return id
}

// UserIDFrom возвращает пользователя, проверенного RequireUser.
func UserIDFrom(ctx context.Context) string {
	id, ok := ctx.Value(ctxKeyUserID).(string)
	if !ok {
		return ""
	}
	return id
}

// RequestID проставляет запросу идентификатор и возвращает его клиенту.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !isSafeRequestID(id) {
			id = uuid.NewString()
		}

		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

// Recoverer превращает панику в 500 с телом в формате Error.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}

			slog.ErrorContext(r.Context(), "паника в обработчике",
				"panic", rec,
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", RequestIDFrom(r.Context()),
				"stack", string(debug.Stack()),
			)
			Error(w, http.StatusInternalServerError, CodeInternal, "Что-то пошло не так. Попробуйте ещё раз")
		}()

		next.ServeHTTP(w, r)
	})
}

// Logger пишет одну строку на запрос.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := newResponseRecorder(w, false)

		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if rec.status >= http.StatusInternalServerError {
			level = slog.LevelError
		}

		slog.LogAttrs(r.Context(), level, "запрос обработан",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int("bytes", rec.written),
			slog.Duration("took", time.Since(started)),
			slog.String("request_id", RequestIDFrom(r.Context())),
			slog.String("user_id", UserIDFrom(r.Context())),
		)
	})
}

// CORS разрешает запросы фронта с дев-сервера Vite.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[strings.TrimSpace(o)] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && allowed[origin] {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", strings.Join([]string{
					"Content-Type", HeaderUserID, HeaderRequestID, HeaderIdempotencyKey,
				}, ", "))
				h.Set("Access-Control-Expose-Headers", HeaderRequestID)
				h.Set("Access-Control-Max-Age", "600")
				h.Add("Vary", "Origin")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireUser пропускает дальше только запросы с корректным X-User-Id.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := strings.TrimSpace(r.Header.Get(HeaderUserID))
		if !userIDPattern.MatchString(userID) {
			Error(w, http.StatusUnauthorized, CodeUnauthorized, "Не удалось определить пользователя")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyUserID, userID)))
	})
}

// RateLimit ограничивает частоту запросов одного пользователя. Ставится после
// RequireUser: лимит бьёт по X-User-Id.
func RateLimit(rps float64, burst int) func(http.Handler) http.Handler {
	limiters := newVisitorLimits(rate.Limit(rps), burst)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiters.allow(UserIDFrom(r.Context())) {
				Error(w, http.StatusTooManyRequests, CodeRateLimited,
					"Слишком много запросов. Подождите пару секунд")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

const (
	visitorTTL = 3 * time.Minute
	sweepEvery = time.Minute
)

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type visitorLimits struct {
	mu        sync.Mutex
	visitors  map[string]*visitor
	lastSweep time.Time
	rps       rate.Limit
	burst     int
}

func newVisitorLimits(rps rate.Limit, burst int) *visitorLimits {
	return &visitorLimits{
		visitors:  make(map[string]*visitor),
		lastSweep: time.Now(),
		rps:       rps,
		burst:     burst,
	}
}

func (v *visitorLimits) allow(userID string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	now := time.Now()
	v.sweepLocked(now)

	entry, ok := v.visitors[userID]
	if !ok {
		entry = &visitor{limiter: rate.NewLimiter(v.rps, v.burst)}
		v.visitors[userID] = entry
	}
	entry.lastSeen = now

	return entry.limiter.Allow()
}

func (v *visitorLimits) sweepLocked(now time.Time) {
	if now.Sub(v.lastSweep) < sweepEvery {
		return
	}
	v.lastSweep = now

	for id, entry := range v.visitors {
		if now.Sub(entry.lastSeen) > visitorTTL {
			delete(v.visitors, id)
		}
	}
}

type responseRecorder struct {
	http.ResponseWriter
	status      int
	written     int
	body        *bytes.Buffer
	wroteHeader bool
}

func newResponseRecorder(w http.ResponseWriter, captureBody bool) *responseRecorder {
	rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
	if captureBody {
		rec.body = &bytes.Buffer{}
	}
	return rec
}

func (rec *responseRecorder) WriteHeader(status int) {
	if rec.wroteHeader {
		return
	}
	rec.wroteHeader = true
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *responseRecorder) Write(b []byte) (int, error) {
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	if rec.body != nil && rec.body.Len() < maxStoredBody {
		rec.body.Write(b)
	}

	n, err := rec.ResponseWriter.Write(b)
	rec.written += n
	return n, err
}

// Flush пробрасывает сброс буфера дальше по цепочке.
func (rec *responseRecorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func isSafeRequestID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		isAllowed := c == '-' || c == '_' ||
			(c >= '0' && c <= '9') ||
			(c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z')
		if !isAllowed {
			return false
		}
	}
	return true
}
