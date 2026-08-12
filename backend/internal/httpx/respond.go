// Package httpx — формат ответов, middleware и идемпотентность.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// Коды ошибок из openapi.yaml.
const (
	CodeBadRequest    = "bad_request"
	CodeUnauthorized  = "unauthorized"
	CodeNotFound      = "not_found"
	CodeQueueDisabled = "queue_disabled"
	CodeRateLimited   = "rate_limited"
	CodeInternal      = "internal_error"

	CodeNoActiveGrant  = "no_active_grant"
	CodeGrantExpired   = "grant_expired"
	CodeGrantConsumed  = "grant_consumed"
	CodeGrantForbidden = "grant_forbidden"
)

const maxBodyBytes = 64 << 10

// ErrorBody — схема Error из контракта.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// JSON пишет v с указанным статусом. nil отправляет только статус.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("не удалось записать тело ответа", "error", err)
	}
}

// Error отвечает ошибкой в формате схемы Error.
func Error(w http.ResponseWriter, status int, code, message string) {
	JSON(w, status, ErrorBody{Code: code, Message: message})
}

// DecodeJSON читает тело запроса в dst. false означает, что ответ клиенту уже
// отправлен.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		Error(w, http.StatusBadRequest, CodeBadRequest, decodeMessage(err))
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		Error(w, http.StatusBadRequest, CodeBadRequest, "Тело запроса должно содержать один JSON-объект")
		return false
	}

	return true
}

func decodeMessage(err error) string {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		return "Тело запроса слишком большое"
	}
	if errors.Is(err, io.EOF) {
		return "Тело запроса пустое"
	}
	return "Некорректный JSON в теле запроса"
}
