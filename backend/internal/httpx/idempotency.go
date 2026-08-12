package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// HeaderIdempotencyKey — ключ намерения, который клиент генерирует один раз на
// действие.
const HeaderIdempotencyKey = "Idempotency-Key"

// Коды ошибок идемпотентности.
const (
	CodeIdempotencyMismatch = "idempotency_key_mismatch"
	CodeRequestInProgress   = "request_in_progress"
)

const (
	maxStoredBody = 32 << 10

	inFlightAttempts = 10
	inFlightBackoff  = 100 * time.Millisecond
)

// IdempotencyStore хранит результаты обработанных запросов в PostgreSQL.
type IdempotencyStore struct {
	pool *pgxpool.Pool
}

// NewIdempotencyStore создаёт хранилище поверх пула соединений.
func NewIdempotencyStore(pool *pgxpool.Pool) *IdempotencyStore {
	return &IdempotencyStore{pool: pool}
}

// Middleware делает обработчик идемпотентным по заголовку Idempotency-Key.
// Ставится после RequireUser: ключ уникален в пределах пользователя.
func (s *IdempotencyStore) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(HeaderIdempotencyKey)
		if key == "" || !isMutating(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if !isSafeRequestID(key) {
			Error(w, http.StatusBadRequest, CodeBadRequest, "Некорректный Idempotency-Key")
			return
		}

		userID := UserIDFrom(r.Context())
		if userID == "" {
			slog.ErrorContext(r.Context(), "идемпотентность включена без RequireUser", "path", r.URL.Path)
			Error(w, http.StatusInternalServerError, CodeInternal, "Что-то пошло не так. Попробуйте ещё раз")
			return
		}

		hash, ok := hashRequest(w, r)
		if !ok {
			return
		}

		s.serve(w, r, next, userID, key, hash)
	})
}

func (s *IdempotencyStore) serve(
	w http.ResponseWriter, r *http.Request, next http.Handler,
	userID, key, hash string,
) {
	ctx := r.Context()

	claimed, err := s.claim(ctx, userID, key, hash)
	if err != nil {
		slog.ErrorContext(ctx, "не удалось занять ключ идемпотентности", "error", err)
		Error(w, http.StatusInternalServerError, CodeInternal, "Что-то пошло не так. Попробуйте ещё раз")
		return
	}

	if !claimed {
		s.replay(ctx, w, userID, key, hash)
		return
	}

	rec := newResponseRecorder(w, true)
	next.ServeHTTP(rec, r)

	// 5xx не запоминаем: повтор с тем же ключом должен действительно повториться.
	if rec.status >= http.StatusInternalServerError {
		if err := s.release(ctx, userID, key); err != nil {
			slog.ErrorContext(ctx, "не удалось освободить ключ идемпотентности", "error", err)
		}
		return
	}

	if err := s.complete(ctx, userID, key, rec.status, rec.body.Bytes()); err != nil {
		slog.ErrorContext(ctx, "не удалось сохранить результат идемпотентного запроса", "error", err)
	}
}

func (s *IdempotencyStore) replay(ctx context.Context, w http.ResponseWriter, userID, key, hash string) {
	for attempt := 0; attempt < inFlightAttempts; attempt++ {
		stored, err := s.fetch(ctx, userID, key)
		if err != nil {
			slog.ErrorContext(ctx, "не удалось прочитать ключ идемпотентности", "error", err)
			Error(w, http.StatusInternalServerError, CodeInternal, "Что-то пошло не так. Попробуйте ещё раз")
			return
		}

		if stored.requestHash != hash {
			Error(w, http.StatusConflict, CodeIdempotencyMismatch,
				"Этот Idempotency-Key уже использован для другого запроса")
			return
		}

		if stored.statusCode != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(*stored.statusCode)
			if _, err := w.Write(stored.body); err != nil {
				slog.ErrorContext(ctx, "не удалось повторить сохранённый ответ", "error", err)
			}
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(inFlightBackoff):
		}
	}

	Error(w, http.StatusConflict, CodeRequestInProgress, "Запрос уже обрабатывается. Попробуйте ещё раз")
}

type storedResponse struct {
	requestHash string
	statusCode  *int
	body        []byte
}

func (s *IdempotencyStore) claim(ctx context.Context, userID, key, hash string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO idempotency_keys (user_id, idempotency_key, request_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, idempotency_key) DO NOTHING`,
		userID, key, hash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *IdempotencyStore) fetch(ctx context.Context, userID, key string) (storedResponse, error) {
	var stored storedResponse
	err := s.pool.QueryRow(ctx, `
		SELECT request_hash, status_code, response_body
		FROM idempotency_keys
		WHERE user_id = $1 AND idempotency_key = $2`,
		userID, key,
	).Scan(&stored.requestHash, &stored.statusCode, &stored.body)

	if errors.Is(err, pgx.ErrNoRows) {
		return storedResponse{}, nil
	}
	return stored, err
}

func (s *IdempotencyStore) complete(ctx context.Context, userID, key string, status int, body []byte) error {
	if len(body) > maxStoredBody {
		body = body[:maxStoredBody]
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE idempotency_keys
		SET status_code = $3, response_body = $4, completed_at = now()
		WHERE user_id = $1 AND idempotency_key = $2`,
		userID, key, status, body)
	return err
}

func (s *IdempotencyStore) release(ctx context.Context, userID, key string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE user_id = $1 AND idempotency_key = $2 AND status_code IS NULL`,
		userID, key)
	return err
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func hashRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		Error(w, http.StatusBadRequest, CodeBadRequest, "Не удалось прочитать тело запроса")
		return "", false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	sum := sha256.New()
	sum.Write([]byte(r.Method))
	sum.Write([]byte{0})
	sum.Write([]byte(r.URL.Path))
	sum.Write([]byte{0})
	sum.Write(body)

	return hex.EncodeToString(sum.Sum(nil)), true
}
