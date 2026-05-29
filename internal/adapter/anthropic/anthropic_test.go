package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/1mb-dev/shim/internal/adapter"
	"github.com/1mb-dev/shim/internal/translate"
)

func unmarshalReq(t *testing.T, body []byte) *translate.AnthropicRequest {
	t.Helper()
	var req translate.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	return &req
}

func testAdapter(t *testing.T, baseURL, key string) adapter.Adapter {
	t.Helper()
	a, err := New(ConfigureOpts{BaseURL: baseURL, APIKey: key})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestMapModel_Identity(t *testing.T) {
	a := testAdapter(t, "https://api.anthropic.com", "k")
	for _, m := range []string{"claude-opus-4-7", "claude-3-5-sonnet-20240620", "", "anything"} {
		if got := a.MapModel(m); got != m {
			t.Errorf("MapModel(%q) = %q, want identity", m, got)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := testAdapter(t, "https://api.anthropic.com", "k").Validate(); err != nil {
		t.Errorf("configured adapter should validate: %v", err)
	}
	if err := testAdapter(t, "https://api.anthropic.com", "").Validate(); err == nil {
		t.Error("missing API key should fail Validate")
	}
}

func TestNew_DefaultBaseURL(t *testing.T) {
	a := testAdapter(t, "", "k")
	req, err := a.BuildRequest(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(req.URL.String(), DefaultBaseURL+"/v1/messages") {
		t.Errorf("default base URL not applied: %s", req.URL)
	}
}

func TestBuildRequest_AuthAndPath(t *testing.T) {
	a := testAdapter(t, "https://up.example/", "secret-key")
	req, err := a.BuildRequest(context.Background(), []byte(`{"model":"claude-opus-4-7"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.String() != "https://up.example/v1/messages" {
		t.Errorf("url = %s, want .../v1/messages (no double slash)", req.URL)
	}
	if req.Header.Get("x-api-key") != "secret-key" {
		t.Errorf("x-api-key not set: %q", req.Header.Get("x-api-key"))
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("must not set Authorization (Anthropic uses x-api-key)")
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content-type = %q", req.Header.Get("Content-Type"))
	}
}

func TestBuildRequest_ForwardsClientVersionAndBeta(t *testing.T) {
	a := testAdapter(t, "https://up.example", "k")
	in := http.Header{}
	in.Set("anthropic-version", "2099-01-01")
	in.Set("anthropic-beta", "tools-2024,foo")
	ctx := adapter.WithInboundHeaders(context.Background(), in)

	req, err := a.BuildRequest(ctx, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("anthropic-version"); got != "2099-01-01" {
		t.Errorf("anthropic-version = %q, want client's verbatim", got)
	}
	if got := req.Header.Get("anthropic-beta"); got != "tools-2024,foo" {
		t.Errorf("anthropic-beta = %q, want client's verbatim", got)
	}
}

func TestBuildRequest_InjectsDefaultVersionWhenAbsent(t *testing.T) {
	a := testAdapter(t, "https://up.example", "k")
	// No inbound headers in context.
	req, err := a.BuildRequest(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("anthropic-version"); got != DefaultAnthropicVersion {
		t.Errorf("anthropic-version = %q, want injected default %q", got, DefaultAnthropicVersion)
	}
	if req.Header.Get("anthropic-beta") != "" {
		t.Error("anthropic-beta must not be set when client omitted it")
	}
}

func TestNormalizeResponse(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"type":"message","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer okSrv.Close()
	a := testAdapter(t, "x", "k")

	resp, _ := http.Get(okSrv.URL)
	body, err := a.NormalizeResponse(resp)
	if err != nil {
		t.Fatalf("2xx should not error: %v", err)
	}
	if !strings.Contains(string(body), `"input_tokens":3`) {
		t.Errorf("body not returned verbatim: %s", body)
	}

	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"overloaded"}`))
	}))
	defer errSrv.Close()
	resp2, _ := http.Get(errSrv.URL)
	body2, err := a.NormalizeResponse(resp2)
	if err == nil {
		t.Error("non-2xx should error")
	}
	if !strings.Contains(string(body2), "overloaded") {
		t.Errorf("error body lost: %s", body2)
	}
}

// TestPassthroughRoundtrip exercises the adapter + identity translator end to
// end against a fake Anthropic upstream: the request body and response body
// must survive verbatim, and usage must be sniffed for measurement.
func TestPassthroughRoundtrip(t *testing.T) {
	const upstreamResp = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5,"output_tokens":3,"cache_read_input_tokens":2}}`
	var gotReqBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotReqBody = string(b)
		_, _ = w.Write([]byte(upstreamResp))
	}))
	defer srv.Close()

	a := testAdapter(t, srv.URL, "k")
	tr := a.Translator()

	// reqBody carries fields shim's AnthropicRequest struct does NOT model
	// (metadata, top_k) plus >4 stop_sequences. All must survive verbatim:
	// passthrough forwards raw bytes and must not cap (cap is OpenAI-only).
	reqBody := []byte(`{"model":"claude-opus-4-7","max_tokens":10,"metadata":{"user_id":"u1"},"top_k":40,"stop_sequences":["a","b","c","d","e"],"messages":[{"role":"user","content":"hi"}]}`)
	upstreamBody, stopCapped, err := tr.ToUpstream(unmarshalReq(t, reqBody), reqBody, "claude-opus-4-7")
	if err != nil {
		t.Fatal(err)
	}
	if stopCapped != 0 {
		t.Errorf("passthrough must not cap stop_sequences, got stopCapped=%d", stopCapped)
	}
	httpReq, err := a.BuildRequest(context.Background(), upstreamBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	normalised, err := a.NormalizeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	respBody, usage, err := tr.FromUpstream(normalised, "claude-opus-4-7")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(gotReqBody, `"model":"claude-opus-4-7"`) {
		t.Errorf("request not forwarded verbatim: %s", gotReqBody)
	}
	// The whole reason for raw-byte forwarding: unmodeled request fields and
	// uncapped stop_sequences must reach the upstream untouched.
	for _, want := range []string{`"metadata":{"user_id":"u1"}`, `"top_k":40`, `"e"`} {
		if !strings.Contains(gotReqBody, want) {
			t.Errorf("request lost %s (not verbatim): %s", want, gotReqBody)
		}
	}
	if string(respBody) != upstreamResp {
		t.Errorf("response not verbatim (lossless passthrough failed):\n got: %s\nwant: %s", respBody, upstreamResp)
	}
	if usage.InputTokens != 5 || usage.OutputTokens != 3 {
		t.Errorf("usage sniff = %+v, want {5,3}", usage)
	}
	// The unmodeled cache_read_input_tokens field must survive verbatim — the
	// whole reason for byte-passthrough over struct re-encode.
	if !strings.Contains(string(respBody), `"cache_read_input_tokens":2`) {
		t.Error("unmodeled field dropped — byte-passthrough fidelity broken")
	}
}

// TestPassthroughStreamRoundtrip: a stream:true request is forwarded with the
// flag intact, and the upstream SSE stream is returned to the client
// byte-identical with usage sniffed from message_start/message_delta.
func TestPassthroughStreamRoundtrip(t *testing.T) {
	const sseResp = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseResp))
	}))
	defer srv.Close()

	a := testAdapter(t, srv.URL, "k")
	tr := a.Translator()

	reqBody := []byte(`{"model":"claude-opus-4-7","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	upstreamBody, _, err := tr.ToUpstream(unmarshalReq(t, reqBody), reqBody, "claude-opus-4-7")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(upstreamBody), `"stream":true`) {
		t.Errorf("stream flag not forwarded verbatim: %s", upstreamBody)
	}
	httpReq, err := a.BuildRequest(context.Background(), upstreamBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	next, usage, err := tr.StreamChunks(resp, "claude-opus-4-7")
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for {
		c, ok, err := next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got.WriteString(string(c))
	}
	if got.String() != sseResp {
		t.Errorf("SSE not byte-identical:\n got: %q\nwant: %q", got.String(), sseResp)
	}
	if usage.InputTokens != 9 || usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want {input:9, output:5}", *usage)
	}
}
