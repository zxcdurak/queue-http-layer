package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const advisoryLockKey int64 = 8_531_204

const createSchemaMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    text        PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
)`

// Migrate применяет непринятые миграции из fsys в лексическом порядке имён.
// Каждая идёт в своей транзакции вместе с отметкой в schema_migrations.
// Пустые файлы пропускаются и не отмечаются применёнными.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("получение соединения для миграций: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("блокировка миграций: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey); err != nil {
			slog.Error("не удалось снять блокировку миграций", "error", err)
		}
	}()

	if _, err := conn.Exec(ctx, createSchemaMigrations); err != nil {
		return fmt.Errorf("создание таблицы schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	names, err := migrationNames(fsys)
	if err != nil {
		return err
	}

	for _, name := range names {
		if applied[name] {
			continue
		}

		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("чтение миграции %s: %w", name, err)
		}
		if strings.TrimSpace(string(body)) == "" {
			slog.Warn("миграция пустая, пропускаем", "migration", name)
			continue
		}

		if err := applyOne(ctx, conn, name, string(body)); err != nil {
			return err
		}
		slog.Info("миграция применена", "migration", name)
	}

	return nil
}

func applyOne(ctx context.Context, conn *pgxpool.Conn, name, body string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("старт транзакции для %s: %w", name, err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.Error("откат транзакции миграции", "migration", name, "error", err)
		}
	}()

	if _, err := tx.Exec(ctx, body); err != nil {
		return fmt.Errorf("выполнение миграции %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", name); err != nil {
		return fmt.Errorf("отметка миграции %s: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("коммит миграции %s: %w", name, err)
	}
	return nil
}

func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("чтение schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("разбор строки schema_migrations: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("обход schema_migrations: %w", err)
	}
	return applied, nil
}

func migrationNames(fsys fs.FS) ([]string, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("поиск файлов миграций: %w", err)
	}
	sort.Strings(names)
	return names, nil
}
