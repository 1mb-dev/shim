package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// parseErrEnvelope decodes an Anthropic error envelope for assertions.
func parseErrEnvelope(t *testing.T, b []byte) (typ, msg string) {
	t.Helper()
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("body not an Anthropic error envelope: %v\nbody: %s", err, b)
	}
	if e.Type != "error" {
		t.Errorf("envelope type = %q, want \"error\"", e.Type)
	}
	return e.Error.Type, e.Error.Message
}

func TestAnthropicErrorJSON(t *testing.T) {
	typ, msg := parseErrEnvelope(t, AnthropicErrorJSON(ErrTypeRateLimit, "slow down"))
	if typ != ErrTypeRateLimit || msg != "slow down" {
		t.Errorf("AnthropicErrorJSON = (%q,%q), want (rate_limit_error, slow down)", typ, msg)
	}
}

// TestAnthropicOpenAIFromUpstreamError pins the OpenAI dialect's status
// re-classification + Anthropic envelope. The upstream body is always dropped
// (OpenAI-shaped, may carry prompt content) — asserted via a sentinel that must
// never appear in the client-facing body.
func TestAnthropicOpenAIFromUpstreamError(t *testing.T) {
	const leak = "SECRET-PROMPT-LEAK"
	cases := []struct {
		name        string
		upstream    int
		wantStatus  int
		wantType    string
		wantMsgPart string
	}{
		{"401 auth", 401, 401, ErrTypeAuthentication, "rejected the API key"},
		{"403 auth", 403, 401, ErrTypeAuthentication, "rejected the API key"},
		{"429 rate", 429, 429, ErrTypeRateLimit, "rate limited"},
		{"500 server", 500, 502, ErrTypeAPI, "upstream unavailable"},
		{"503 server", 503, 502, ErrTypeAPI, "upstream unavailable"},
		{"400 default", 400, 502, ErrTypeAPI, "status 400"},
		{"418 default", 418, 502, ErrTypeAPI, "status 418"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := anthropicOpenAI{}.FromUpstreamError(c.upstream, []byte(leak))
			if status != c.wantStatus {
				t.Errorf("status = %d, want %d", status, c.wantStatus)
			}
			typ, msg := parseErrEnvelope(t, body)
			if typ != c.wantType {
				t.Errorf("error type = %q, want %q", typ, c.wantType)
			}
			if !strings.Contains(msg, c.wantMsgPart) {
				t.Errorf("message %q missing %q", msg, c.wantMsgPart)
			}
			if strings.Contains(string(body), leak) {
				t.Errorf("upstream body leaked into client error: %s", body)
			}
		})
	}
}

// TestIdentityFromUpstreamError: native-Anthropic errors pass through verbatim —
// status unchanged, body byte-identical (it is already Anthropic-shaped).
func TestIdentityFromUpstreamError(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	for _, status := range []int{400, 401, 429, 500, 529} {
		gotStatus, gotBody := identity{}.FromUpstreamError(status, body)
		if gotStatus != status {
			t.Errorf("status = %d, want %d (verbatim)", gotStatus, status)
		}
		if string(gotBody) != string(body) {
			t.Errorf("body not verbatim: got %s", gotBody)
		}
	}
}
