// Package db — подключение к PostgreSQL и применение миграций.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	connectAttempts = 15
	connectBackoff  = time.Second

	maxConns = 10
	minConns = 2
)

// Connect открывает пул и убеждается, что база отвечает. Закрыть пул — забота
// вызывающего.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("разбор DATABASE_URL: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = minConns
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("создание пула соединений: %w", err)
	}

	if err := waitReady(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Postgres в compose успевает ответить pg_isready раньше, чем начинает
// принимать соединения.
func waitReady(ctx context.Context, pool *pgxpool.Pool) error {
	var lastErr error
	for attempt := 1; attempt <= connectAttempts; attempt++ {
		lastErr = pool.Ping(ctx)
		if lastErr == nil {
			return nil
		}

		slog.Warn("база пока не отвечает, повторяем",
			"attempt", attempt, "of", connectAttempts, "error", lastErr)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(connectBackoff):
		}
	}
	return fmt.Errorf("база не ответила за %d попыток: %w", connectAttempts, lastErr)
}
