package translate

import "encoding/json"

// Anthropic error "type" taxonomy — the canonical wire strings for the inner
// `error.type` of the Anthropic error envelope. Single source of truth, shared
// by the server (client-side errors) and the OpenAI-dialect translator's
// upstream-error re-wrap (FromUpstreamError). The identity translator never
// uses them: it forwards the native-Anthropic error body verbatim.
const (
	ErrTypeInvalidRequest = "invalid_request_error"
	ErrTypeAuthentication = "authentication_error"
	ErrTypePermission     = "permission_error"
	ErrTypeRateLimit      = "rate_limit_error"
	ErrTypeAPI            = "api_error"
	ErrTypeOverloaded     = "overloaded_error"
)

// anthropicError mirrors the documented Anthropic error envelope:
//
//	{"type":"error","error":{"type":"invalid_request_error","message":"..."}}
type anthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// AnthropicErrorJSON marshals an Anthropic-shaped error envelope. errType is one
// of the ErrType* constants; message is human-readable. Marshalling two strings
// cannot fail in practice — the fallback is defensive only, never reached.
func AnthropicErrorJSON(errType, message string) []byte {
	var e anthropicError
	e.Type = "error"
	e.Error.Type = errType
	e.Error.Message = message
	b, err := json.Marshal(e)
	if err != nil {
		return []byte(`{"type":"error","error":{"type":"api_error","message":"error serialization failed"}}`)
	}
	return b
}
