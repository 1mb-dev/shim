package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// anthropicError mirrors the documented Anthropic error shape:
//
//	{"type":"error","error":{"type":"invalid_request_error","message":"..."}}
type anthropicError struct {
	Type  string             `json:"type"`
	Error anthropicErrorBody `json:"error"`
}

type anthropicErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Anthropic error type taxonomy used by Stage 0.
const (
	errInvalidRequest = "invalid_request_error"
	errAuthentication = "authentication_error"
	errPermission     = "permission_error"
	errRateLimit      = "rate_limit_error"
	errAPI            = "api_error"
	errOverloaded     = "overloaded_error"
)

// writeError serialises an Anthropic-shaped error and writes it to w with
// the given HTTP status. The error event is logged at error level; the
// logger's redaction layer scrubs the request body before emission.
func writeError(w http.ResponseWriter, log *slog.Logger, status int, typ, msg string) {
	body := anthropicError{
		Type:  "error",
		Error: anthropicErrorBody{Type: typ, Message: msg},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)

	if log != nil {
		log.Error("request failed",
			slog.Int("status", status),
			slog.String("error_type", typ),
			slog.String("message", msg),
		)
	}
}

// isMaxBytesError returns true when err comes from http.MaxBytesReader's
// over-size trip. We rewrap as an Anthropic-shaped 413 so callers don't see
// stdlib EOF.
func isMaxBytesError(err error) bool {
	if err == nil {
		return false
	}
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}
