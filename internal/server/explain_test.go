package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/1mb-dev/shim/internal/translate"
)

// TestExplain_Translated: the OpenAI dialect reshapes the request, so transport
// is "translated" and the model rewrite + stop-sequence cap are reported. Also
// proves explain NEVER calls the upstream (the stub's upstream fails the test if
// hit).
func TestExplain_Translated(t *testing.T) {
	s := newStub()
	defer s.close()
	s.mu.Lock()
	s.upstreamHandler = func(w http.ResponseWriter, _ *http.Request) {
		t.Error("explain must NOT call the upstream")
		w.WriteHeader(http.StatusInternalServerError)
	}
	s.mu.Unlock()
	srv, _ := newTestServer(t, s)

	body := `{"model":"claude-opus-4-7","max_tokens":10,"stop_sequences":["a","b","c","d","e","f"],"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages/explain", body)
	out := bodyOf(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}

	var exp explainResponse
	if err := json.Unmarshal([]byte(out), &exp); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if exp.Transport != "translated" {
		t.Errorf("transport = %q, want translated", exp.Transport)
	}
	if exp.Adapter != s.name {
		t.Errorf("adapter = %q, want %q", exp.Adapter, s.name)
	}
	if len(exp.Mutations) != 2 {
		t.Fatalf("mutations = %+v, want 2 (model_rewrite + stop_sequences_capped)", exp.Mutations)
	}
	// Mutation details via string match (avoids any-typed JSON round-trip wrangling).
	for _, want := range []string{
		`"type":"model_rewrite"`, `"from":"claude-opus-4-7"`, `"to":"stub-model"`,
		`"type":"stop_sequences_capped"`, `"from":6`, `"to":4`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("explain body missing %q\n%s", want, out)
		}
	}
	if !strings.Contains(string(exp.UpstreamRequest), `"model":"stub-model"`) {
		t.Errorf("upstream_request should carry the mapped model: %s", exp.UpstreamRequest)
	}
}

// TestExplain_Passthrough: identity forwards verbatim, so transport is
// "passthrough", NO mutations are reported (even though the stub's MapModel
// would rewrite — identity ignores it, and explain reports what actually
// reaches the wire), and upstream_request is byte-identical to the inbound body
// (incl. the unmodeled top_k field).
func TestExplain_Passthrough(t *testing.T) {
	s := newStub()
	defer s.close()
	s.translator = translate.Identity()
	srv, _ := newTestServer(t, s)

	body := `{"model":"x","max_tokens":10,"top_k":5,"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages/explain", body)
	out := bodyOf(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}

	var exp explainResponse
	if err := json.Unmarshal([]byte(out), &exp); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if exp.Transport != "passthrough" {
		t.Errorf("transport = %q, want passthrough", exp.Transport)
	}
	if len(exp.Mutations) != 0 {
		t.Errorf("passthrough must report no mutations, got %+v", exp.Mutations)
	}
	if string(exp.UpstreamRequest) != body {
		t.Errorf("upstream_request not verbatim:\n got: %s\nwant: %s", exp.UpstreamRequest, body)
	}
}

func TestExplain_MalformedJSON(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	resp := doPOST(t, srv, "/v1/messages/explain", `{not json`)
	out := bodyOf(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(out, "invalid_request_error") {
		t.Errorf("want invalid_request_error envelope: %s", out)
	}
}
