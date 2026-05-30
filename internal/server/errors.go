package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/1mb-dev/shim/internal/translate"
)

// Anthropic error-type aliases for the client-side errors the server raises
// directly. The canonical, complete taxonomy lives in translate (ErrType*);
// the upstream-error re-classification (authentication/rate-limit/api) now lives
// in the OpenAI-dialect translator's FromUpstreamError, so the server only needs
// these two.
const (
	errInvalidRequest = translate.ErrTypeInvalidRequest
	errAPI            = translate.ErrTypeAPI
)

// writeError serialises an Anthropic-shaped error and writes it to w with
// the given HTTP status. The error event is logged at error level; the
// logger's redaction layer scrubs the request body before emission.
func writeError(w http.ResponseWriter, log *slog.Logger, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, writeErr := w.Write(translate.AnthropicErrorJSON(typ, msg))

	if log != nil {
		log.Error("request failed",
			slog.Int("status", status),
			slog.String("error_type", typ),
			slog.String("message", msg),
		)
		if writeErr != nil {
			log.Error("error-response write failed", slog.String("error", writeErr.Error()))
		}
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
