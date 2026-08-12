// Package checkout — оформление покупки по праву из очереди.
package checkout

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/shndo1337/avito-queue/backend/internal/httpx"
	"github.com/shndo1337/avito-queue/backend/internal/queue"
)

// GrantConsumer гасит право на покупку.
type GrantConsumer interface {
	ConsumeGrant(ctx context.Context, userID, productID, token string) (orderID string, err error)
}

// Handler обслуживает POST /api/checkout.
type Handler struct {
	grants GrantConsumer
}

// NewHandler создаёт обработчик чекаута.
func NewHandler(grants GrantConsumer) *Handler {
	return &Handler{grants: grants}
}

// RegisterRoutes вешает чекаут на mux. Маршрут должен быть закрыт RequireUser.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/checkout", h.Create)
}

type createRequest struct {
	ProductID  string `json:"product_id"`
	GrantToken string `json:"grant_token"`
}

// Order — схема Order из openapi.yaml.
type Order struct {
	OrderID   string `json:"order_id"`
	ProductID string `json:"product_id"`
	Status    string `json:"status"`
}

// Create оформляет заказ по праву на покупку.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := httpx.UserIDFrom(ctx)

	var req createRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.ProductID == "" || req.GrantToken == "" {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest,
			"Нужно передать product_id и grant_token")
		return
	}

	orderID, err := h.grants.ConsumeGrant(ctx, userID, req.ProductID, req.GrantToken)
	if err != nil {
		h.respondGrantError(ctx, w, err)
		return
	}

	httpx.JSON(w, http.StatusOK, Order{
		OrderID:   orderID,
		ProductID: req.ProductID,
		Status:    "created",
	})
}

func (h *Handler) respondGrantError(ctx context.Context, w http.ResponseWriter, err error) {
	status, code, message, known := queue.HTTPError(err)
	if !known {
		slog.ErrorContext(ctx, "не удалось погасить право на покупку", "error", err)
	}
	httpx.Error(w, status, code, message)
}
