package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler(called *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*called++
		JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

func TestRequireUserRejectsBadHeaders(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		userID string
	}{
		{"заголовка нет", ""},
		{"только пробелы", "   "},
		{"перевод строки — подмена логов", "user-1\nfake=admin"},
		{"кавычка — попытка сломать разбор", `user"1`},
		{"слишком длинный", string(make([]byte, 65))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			called := 0
			req := httptest.NewRequest(http.MethodGet, "/api/checkout", nil)
			if tc.userID != "" {
				req.Header.Set(HeaderUserID, tc.userID)
			}

			rec := httptest.NewRecorder()
			RequireUser(okHandler(&called)).ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("статус: получено %d, ожидалось %d", rec.Code, http.StatusUnauthorized)
			}
			if called != 0 {
				t.Errorf("обработчик вызван %d раз, ожидалось 0", called)
			}
		})
	}
}

func TestRequireUserPutsUserInContext(t *testing.T) {
	t.Parallel()

	var seen string
	handler := RequireUser(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = UserIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/checkout", nil)
	req.Header.Set(HeaderUserID, "user-1")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "user-1" {
		t.Errorf("user_id в контексте: получено %q, ожидалось %q", seen, "user-1")
	}
}

func TestRateLimitStopsBurst(t *testing.T) {
	t.Parallel()

	const burst = 3
	called := 0
	// rps крошечный, чтобы за время теста запас не успел пополниться.
	handler := RequireUser(RateLimit(0.001, burst)(okHandler(&called)))

	statuses := make([]int, 0, burst+2)
	for i := 0; i < burst+2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/products/p-1/queue", nil)
		req.Header.Set(HeaderUserID, "user-1")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		statuses = append(statuses, rec.Code)
	}

	for i := 0; i < burst; i++ {
		if statuses[i] != http.StatusOK {
			t.Errorf("запрос %d: получено %d, ожидалось %d", i+1, statuses[i], http.StatusOK)
		}
	}
	for i := burst; i < len(statuses); i++ {
		if statuses[i] != http.StatusTooManyRequests {
			t.Errorf("запрос %d: получено %d, ожидалось %d", i+1, statuses[i], http.StatusTooManyRequests)
		}
	}
	if called != burst {
		t.Errorf("обработчик вызван %d раз, ожидалось %d", called, burst)
	}
}

// Лимит одного покупателя не должен мешать другому.
func TestRateLimitIsPerUser(t *testing.T) {
	t.Parallel()

	called := 0
	handler := RequireUser(RateLimit(0.001, 1)(okHandler(&called)))

	request := func(userID string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/products/p-1/queue", nil)
		req.Header.Set(HeaderUserID, userID)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := request("user-1"); got != http.StatusOK {
		t.Fatalf("первый запрос user-1: получено %d", got)
	}
	if got := request("user-1"); got != http.StatusTooManyRequests {
		t.Fatalf("второй запрос user-1: получено %d, ожидалось 429", got)
	}
	if got := request("user-2"); got != http.StatusOK {
		t.Errorf("запрос user-2: получено %d, ожидалось 200 — лимит протёк между пользователями", got)
	}
}

func TestRecovererTurnsPanicIntoJSON(t *testing.T) {
	t.Parallel()

	handler := Recoverer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("что-то пошло не так")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/products", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("статус: получено %d, ожидалось %d", rec.Code, http.StatusInternalServerError)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type: получено %q", got)
	}
}

func TestRequestIDRejectsUnsafeClientValue(t *testing.T) {
	t.Parallel()

	var seen string
	handler := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set(HeaderRequestID, "id\r\nX-Injected: 1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if seen == "id\r\nX-Injected: 1" {
		t.Fatal("небезопасный X-Request-Id принят как есть")
	}
	if seen == "" {
		t.Fatal("подменный идентификатор не сгенерирован")
	}
	if rec.Header().Get("X-Injected") != "" {
		t.Error("в ответ попал внедрённый заголовок")
	}
}
