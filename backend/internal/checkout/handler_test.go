package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shndo1337/avito-queue/backend/internal/httpx"
	"github.com/shndo1337/avito-queue/backend/internal/queue"
)

type stubConsumer struct {
	orderID string
	err     error

	gotUserID    string
	gotProductID string
	gotToken     string
	calls        int
}

func (s *stubConsumer) ConsumeGrant(_ context.Context, userID, productID, token string) (string, error) {
	s.calls++
	s.gotUserID, s.gotProductID, s.gotToken = userID, productID, token
	return s.orderID, s.err
}

// Прогоняем через RequireUser: без него в контексте нет пользователя.
func do(t *testing.T, consumer GrantConsumer, userID, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api/checkout", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set(httpx.HeaderUserID, userID)
	}

	rec := httptest.NewRecorder()
	httpx.RequireUser(http.HandlerFunc(NewHandler(consumer).Create)).ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) httpx.ErrorBody {
	t.Helper()

	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не разобрался как Error: %v (тело: %s)", err, rec.Body.String())
	}
	return body
}

// Без активного права покупка не оформляется ни при каких условиях.
func TestCreateRejectsEveryInvalidGrant(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		consumer error
		wantCode string
	}{
		{"права никогда не было", queue.ErrNoActiveGrant, httpx.CodeNoActiveGrant},
		{"право истекло", queue.ErrGrantExpired, httpx.CodeGrantExpired},
		{"право уже использовано", queue.ErrGrantConsumed, httpx.CodeGrantConsumed},
		{"право чужое", queue.ErrGrantForbidden, httpx.CodeGrantForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := do(t, &stubConsumer{err: tc.consumer}, "user-1",
				`{"product_id":"p-1","grant_token":"11111111-1111-1111-1111-111111111111"}`)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("статус: получено %d, ожидалось %d", rec.Code, http.StatusForbidden)
			}
			if got := decodeError(t, rec).Code; got != tc.wantCode {
				t.Errorf("код ошибки: получено %q, ожидалось %q", got, tc.wantCode)
			}
		})
	}
}

// Если потерять product_id, токен от одного товара сработает для другого.
func TestCreatePassesProductIDToCore(t *testing.T) {
	t.Parallel()

	consumer := &stubConsumer{orderID: "order-1"}
	rec := do(t, consumer, "user-42",
		`{"product_id":"p-7","grant_token":"22222222-2222-2222-2222-222222222222"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("статус: получено %d, ожидалось %d (тело: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if consumer.gotProductID != "p-7" {
		t.Errorf("product_id: получено %q, ожидалось %q", consumer.gotProductID, "p-7")
	}
	if consumer.gotUserID != "user-42" {
		t.Errorf("user_id: получено %q, ожидалось %q", consumer.gotUserID, "user-42")
	}
	if consumer.gotToken != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("grant_token: получено %q", consumer.gotToken)
	}

	var order Order
	if err := json.Unmarshal(rec.Body.Bytes(), &order); err != nil {
		t.Fatalf("ответ не разобрался как Order: %v", err)
	}
	if order.OrderID != "order-1" || order.ProductID != "p-7" || order.Status != "created" {
		t.Errorf("заказ: получено %+v", order)
	}
}

func TestCreateRequiresUser(t *testing.T) {
	t.Parallel()

	consumer := &stubConsumer{orderID: "order-1"}
	rec := do(t, consumer, "",
		`{"product_id":"p-1","grant_token":"33333333-3333-3333-3333-333333333333"}`)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("статус: получено %d, ожидалось %d", rec.Code, http.StatusUnauthorized)
	}
	if consumer.calls != 0 {
		t.Errorf("ядро вызвано %d раз, ожидалось 0", consumer.calls)
	}
}

func TestCreateRejectsMalformedBody(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"пустое тело", ``},
		{"не json", `не json`},
		{"без product_id", `{"grant_token":"44444444-4444-4444-4444-444444444444"}`},
		{"без grant_token", `{"product_id":"p-1"}`},
		{"лишнее поле", `{"product_id":"p-1","grant_token":"t","user_id":"admin"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			consumer := &stubConsumer{orderID: "order-1"}
			rec := do(t, consumer, "user-1", tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("статус: получено %d, ожидалось %d", rec.Code, http.StatusBadRequest)
			}
			if consumer.calls != 0 {
				t.Errorf("ядро вызвано %d раз на некорректном теле, ожидалось 0", consumer.calls)
			}
		})
	}
}

// Сбой базы не должен маскироваться под 403, а его текст — уходить наружу.
func TestCreateHidesUnexpectedErrors(t *testing.T) {
	t.Parallel()

	rec := do(t, &stubConsumer{err: errors.New("connection refused: 10.0.0.5:5432")}, "user-1",
		`{"product_id":"p-1","grant_token":"55555555-5555-5555-5555-555555555555"}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("статус: получено %d, ожидалось %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Errorf("во внешний ответ утекли подробности ошибки: %s", rec.Body.String())
	}
}
