package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/1mb-dev/shim/internal/measure"
	"github.com/1mb-dev/shim/internal/tokens"
	"github.com/1mb-dev/shim/internal/translate"
)

// maxStopSequences is the OpenAI-imposed cap on stop[] entries. Requests
// over this are truncated at the server boundary with a warn log rather
// than forwarded to a 400 that looks like a shim bug.
const maxStopSequences = 4

// upstreamBodyLogBytes caps the bytes of upstream-error body recorded in
// the `upstream_error` log line. 1024 covers the typical OpenAI-style
// `{"error":{"type":"...","message":"..."}}` envelope with room for echoed
// prompt fragments; truncates pathologically large 5xx HTML pages.
// Operator-facing only — body never echoed to the client. See README
// "Errors and debugging" for the upstream-echo disclosure.
const upstreamBodyLogBytes = 1024

// handleHealth — GET /health → {"status":"ok"}.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	start := time.Now()
	s.measure.RecordRequestSeen("/health")
	defer func() { s.measure.RecordLatency("/health", time.Since(start)) }()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleMetrics — GET /v1/metrics → measure.Snapshot as JSON. No auth;
// matches /health's loopback-only trust model. The README's "Measurement"
// section documents the wire shape and the no-auth implication.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	start := time.Now()
	s.measure.RecordRequestSeen("/v1/metrics")
	defer func() { s.measure.RecordLatency("/v1/metrics", time.Since(start)) }()

	snap := s.measure.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		s.log.Error("response encode failed",
			slog.String("path", "/v1/metrics"),
			slog.String("error", err.Error()),
		)
	}
}

// logModelRewrite emits a breadcrumb when the adapter rewrites the
// requested model name + records the rewrite event for /v1/metrics.
// Thesis-2: never silently forward modified traffic. Empty requested →
// non-empty resolved (i.e., client sent no model, shim picked one) IS a
// rewrite — substitution happened, it deserves the breadcrumb.
func (s *Server) logModelRewrite(requested, resolved string) {
	if requested == resolved {
		return
	}
	s.log.Info("model rewritten",
		slog.String("requested", requested),
		slog.String("resolved", resolved),
		slog.String("adapter", s.adapter.Name()),
	)
	s.measure.RecordRewriteEvent(measure.RewriteModel)
}

// inputTokens returns the cl100k_base BPE token count for the request's
// input prompt — extracts text from system + message content blocks, then
// runs tokens.Count. Used by /v1/messages/count_tokens and the measurement
// collector (delta vs. upstream prompt_tokens).
//
// cl100k is OpenAI's GPT-4 tokenizer; the upstream may use a different
// scheme (DeepSeek's tokenizer is not published). The result is therefore
// "approximate to the upstream" but exact under cl100k. README's
// Measurement section discloses this.
//
// Naive stringification of json.RawMessage would feed brackets/quotes/keys
// to the tokenizer as if they were prompt characters, structurally
// inflating shim_total. extractText unmarshals the actual text fields.
// Tool-call argument JSON is currently NOT counted; known under-count for
// tool-heavy requests.
func inputTokens(req *translate.AnthropicRequest) int {
	var sb strings.Builder
	if s, ok := extractText(req.System); ok {
		sb.WriteString(s)
	}
	for _, m := range req.Messages {
		if s, ok := extractText(m.Content); ok {
			if sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(s)
		}
	}
	return tokens.Count(sb.String())
}

// extractText pulls prompt text out of a json.RawMessage that may be a
// JSON string ("text") or an array of content blocks
// ([{"type":"text","text":"..."}, ...]). Returns ("", false) for null or
// unparseable input. Non-text blocks (image, tool_use, tool_result) are
// skipped — their JSON bodies would otherwise count brackets and keys.
func extractText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", false
		}
		return s, true
	}
	if raw[0] == '[' {
		var blocks []translate.AnthropicBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return "", false
		}
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				if sb.Len() > 0 {
					sb.WriteByte(' ')
				}
				sb.WriteString(b.Text)
			}
		}
		return sb.String(), true
	}
	return "", false
}

// handleMessages — POST /v1/messages.
//
// Flow: read+cap body → parse → reject streaming/thinking → translate to
// OpenAI → adapter.BuildRequest → client.Do → adapter.NormalizeResponse →
// translate back to Anthropic → write JSON.
//
// Every loud-fail path emits an Anthropic-shaped error and logs the event
// with redacted attrs.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.measure.RecordRequestSeen("/v1/messages")
	defer func() { s.measure.RecordLatency("/v1/messages", time.Since(start)) }()
	body, err := s.readBody(r, w)
	if err != nil {
		// readBody already wrote the response.
		return
	}

	var req translate.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, s.log, http.StatusBadRequest, errInvalidRequest,
			"malformed JSON body: "+err.Error())
		return
	}

	if containsThinkingBlock(req.Messages) {
		writeError(w, s.log, http.StatusNotImplemented, errInvalidRequest,
			"extended thinking not yet supported")
		return
	}

	// Stage 2.6b: 501 on request-level thinking=enabled. Different layer
	// from containsThinkingBlock above (which gates message-level thinking
	// content blocks). The counter is the L2-demand telemetry that decides
	// whether Stage 2.6c fires — see todos/shim-stage2.6b-plan.md.
	if req.Thinking != nil && req.Thinking.Type == "enabled" {
		s.measure.RecordThinkingEnabledSeen()
		writeError(w, s.log, http.StatusNotImplemented, errInvalidRequest,
			"extended thinking not yet supported by shim's translator (planned for Stage 2.6c)")
		return
	}

	// OpenAI-compatible upstreams reject stop arrays larger than 4 with a
	// 400 that looks like a shim bug; cap loudly per thesis-2. Run AFTER
	// the thinking-block gate so a request rejected at 501 doesn't leave a
	// rewrite counter increment that never reached the wire.
	if n := len(req.StopSequences); n > maxStopSequences {
		s.log.Warn("stop_sequences truncated",
			slog.Int("from", n),
			slog.Int("to", maxStopSequences),
		)
		req.StopSequences = req.StopSequences[:maxStopSequences]
		s.measure.RecordRewriteEvent(measure.RewriteStopSequences)
	}

	if req.Stream {
		s.handleMessagesStream(w, r, &req)
		return
	}

	openaiReq, err := translate.AnthropicToOpenAI(&req)
	if err != nil {
		writeError(w, s.log, http.StatusBadRequest, errInvalidRequest,
			"translation: "+err.Error())
		return
	}
	openaiReq.Model = s.adapter.MapModel(req.Model)
	s.logModelRewrite(req.Model, openaiReq.Model)
	if req.Thinking == nil {
		s.measure.RecordRewriteEvent(measure.RewriteThinkingDisabled)
	}

	openaiBody, err := json.Marshal(openaiReq)
	if err != nil {
		writeError(w, s.log, http.StatusInternalServerError, errAPI,
			"failed to encode upstream request")
		return
	}

	httpReq, err := s.adapter.BuildRequest(r.Context(), openaiBody)
	if err != nil {
		writeError(w, s.log, http.StatusInternalServerError, errAPI,
			"build upstream request: "+err.Error())
		return
	}

	upstream, err := s.client.Do(httpReq)
	if err != nil {
		writeError(w, s.log, http.StatusBadGateway, errAPI,
			"upstream unreachable: "+err.Error())
		return
	}

	normalised, err := s.adapter.NormalizeResponse(upstream)
	if err != nil {
		s.writeUpstreamError(w, "/v1/messages", openaiReq.Model, upstream.StatusCode, normalised, err)
		return
	}

	var openaiResp translate.OpenAIResponse
	if err := json.Unmarshal(normalised, &openaiResp); err != nil {
		writeError(w, s.log, http.StatusBadGateway, errAPI,
			"upstream returned malformed JSON: "+err.Error())
		return
	}

	anthropicResp, err := translate.OpenAIToAnthropic(&openaiResp, req.Model)
	if err != nil {
		writeError(w, s.log, http.StatusInternalServerError, errAPI,
			"translation back: "+err.Error())
		return
	}

	// Record AFTER back-translation succeeds so a failed translate doesn't
	// credit shim_total for a request the client never saw a 200 for.
	// Collector additionally no-ops on zero Usage (some upstreams omit it).
	s.measure.RecordTokenDelta("/v1/messages",
		inputTokens(&req),
		openaiResp.Usage.PromptTokens,
		openaiResp.Usage.CompletionTokens,
	)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(anthropicResp); err != nil {
		// Headers already committed; can't change status. Log for visibility.
		s.log.Error("response encode failed", slog.String("path", "/v1/messages"), slog.String("error", err.Error()))
	}
}

// handleCountTokens — POST /v1/messages/count_tokens → {"input_tokens": N}.
// N is the cl100k_base BPE count; see inputTokens for the cross-tokenizer
// caveat (DeepSeek's actual tokenizer is not published).
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.measure.RecordRequestSeen("/v1/messages/count_tokens")
	defer func() { s.measure.RecordLatency("/v1/messages/count_tokens", time.Since(start)) }()

	body, err := s.readBody(r, w)
	if err != nil {
		return
	}

	var req translate.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, s.log, http.StatusBadRequest, errInvalidRequest,
			"malformed JSON body: "+err.Error())
		return
	}

	n := inputTokens(&req)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]int{"input_tokens": n}); err != nil {
		s.log.Error("response encode failed", slog.String("path", "/v1/messages/count_tokens"), slog.String("error", err.Error()))
	}
}

// readBody reads and caps the request body, converting MaxBytesReader
// errors into Anthropic-shaped 413s. On error it has already written the
// response; callers should return.
func (s *Server) readBody(r *http.Request, w http.ResponseWriter) ([]byte, error) {
	limit := s.cfg.MaxRequestBytes
	if limit <= 0 {
		limit = 1 << 20 // 1 MiB safety default
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if isMaxBytesError(err) {
			writeError(w, s.log, http.StatusRequestEntityTooLarge, errInvalidRequest,
				fmt.Sprintf("request body exceeds MAX_REQUEST_BYTES=%d", limit))
		} else {
			writeError(w, s.log, http.StatusBadRequest, errInvalidRequest,
				"read body: "+err.Error())
		}
		return nil, err
	}
	return body, nil
}

// writeUpstreamError translates a non-2xx upstream response into the right
// Anthropic-shaped error class. The upstream body is NOT echoed to the
// client (it can contain prompt content or other sensitive material), but
// it IS logged as `body_preview` on the `upstream_error` line for operator
// diagnosis — thesis-1: the proxy's reason to exist is honest boundary
// visibility. See README "Errors and debugging" for the upstream-echo
// disclosure. endpoint is the shim-side endpoint that initiated the
// upstream call (e.g. "/v1/messages") — used to bucket the error in
// /v1/metrics' upstream_errors aggregate. resolvedModel is the upstream
// model name after Adapter.MapModel; carried on the log line so a failure
// is joinable to the prior "model rewritten" breadcrumb without timestamp
// triangulation.
func (s *Server) writeUpstreamError(w http.ResponseWriter, endpoint, resolvedModel string, upstreamStatus int, body []byte, err error) {
	s.measure.RecordUpstreamError(endpoint, upstreamStatus)

	preview := body
	if len(preview) > upstreamBodyLogBytes {
		preview = preview[:upstreamBodyLogBytes]
		// Byte truncation may have cut a multi-byte UTF-8 rune; walk back
		// at most 3 bytes (max continuation length) so the tail is
		// rune-clean. Invalid bytes ELSEWHERE in the body stay as-is —
		// slog emits U+FFFD for them, which is the honest signal that the
		// upstream sent non-UTF-8 content.
		for i := 0; i < 3 && len(preview) > 0; i++ {
			if r, _ := utf8.DecodeLastRune(preview); r != utf8.RuneError {
				break
			}
			preview = preview[:len(preview)-1]
		}
	}
	s.log.Error("upstream error",
		slog.String("endpoint", endpoint),
		slog.String("adapter", s.adapter.Name()),
		slog.Int("upstream_status", upstreamStatus),
		slog.String("resolved_model", resolvedModel),
		slog.String("body_preview", string(preview)),
	)

	switch {
	case upstreamStatus == http.StatusUnauthorized || upstreamStatus == http.StatusForbidden:
		writeError(w, s.log, http.StatusUnauthorized, errAuthentication,
			fmt.Sprintf("upstream rejected the API key (status %d)", upstreamStatus))
	case upstreamStatus == http.StatusTooManyRequests:
		writeError(w, s.log, http.StatusTooManyRequests, errRateLimit,
			"upstream rate limited")
	case upstreamStatus >= 500:
		writeError(w, s.log, http.StatusBadGateway, errAPI,
			fmt.Sprintf("upstream unavailable (status %d)", upstreamStatus))
	default:
		writeError(w, s.log, http.StatusBadGateway, errAPI,
			"upstream error: "+err.Error())
	}
}

func containsThinkingBlock(msgs []translate.AnthropicMessage) bool {
	for _, m := range msgs {
		if len(m.Content) == 0 || m.Content[0] != '[' {
			continue
		}
		// Cheap scan rather than parsing the full structure twice — looks
		// for a top-level "thinking" type token. Conservative: false
		// negatives are acceptable (the translator will reject downstream),
		// false positives would block legitimate traffic and are rejected.
		var blocks []translate.AnthropicBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "thinking" {
				return true
			}
		}
	}
	return false
}
