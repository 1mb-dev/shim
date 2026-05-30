package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/1mb-dev/shim/internal/adapter"
	"github.com/1mb-dev/shim/internal/measure"
	"github.com/1mb-dev/shim/internal/tokens"
	"github.com/1mb-dev/shim/internal/translate"
)

// upstreamErrStatus maps a Translator error to an HTTP status: a
// structurally-valid upstream response shim couldn't convert back to
// Anthropic shape is a 500 (ErrBackTranslation — shim's mapping fell short);
// a malformed or otherwise unusable upstream body is a 502 (gateway problem).
func upstreamErrStatus(err error) int {
	if errors.Is(err, translate.ErrBackTranslation) {
		return http.StatusInternalServerError
	}
	return http.StatusBadGateway
}

// copyForwardedHeaders passes a fixed allowlist of upstream response headers
// through to the client: tracing (request-id) + rate-limit/backoff
// (retry-after, anthropic-ratelimit-*). It is dialect-agnostic — each upstream
// sets what it sets, so headers a given provider doesn't emit are simply not
// forwarded. Content-framing and hop-by-hop headers (Content-Length,
// Transfer-Encoding, Connection, Content-Type, Content-Encoding) are NEVER
// forwarded; shim owns those.
func copyForwardedHeaders(dst, src http.Header) {
	for k, vals := range src {
		if !forwardResponseHeader(http.CanonicalHeaderKey(k)) {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func forwardResponseHeader(canonicalKey string) bool {
	switch canonicalKey {
	case "Request-Id", "Retry-After":
		return true
	}
	return strings.HasPrefix(canonicalKey, "Anthropic-Ratelimit-")
}

// recordStopCap emits the loud-fail (warn log + rewrite metric) when the
// translator capped stop sequences for its dialect. dropped==0 is a no-op, so
// passthrough (which never caps) records nothing. Keeps the modification
// observable (thesis-2) while the cap itself lives in the OpenAI translator.
func (s *Server) recordStopCap(req *translate.AnthropicRequest, dropped int) {
	if dropped == 0 {
		return
	}
	s.log.Warn("stop_sequences truncated",
		slog.Int("from", len(req.StopSequences)),
		slog.Int("to", len(req.StopSequences)-dropped),
	)
	s.measure.RecordRewriteEvent(measure.RewriteStopSequences)
}

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

// handleMetricsPrometheus — GET /metrics → measure.Snapshot in Prometheus text
// exposition format (the scrapeable sibling of /v1/metrics' human JSON). No auth:
// matches /health + /v1/metrics' loopback-only trust model.
//
// Deliberately does NOT self-record: /metrics is a scrape target hit every ~15s,
// so recording it would let monitoring traffic dominate requests_seen and turn
// its own latency reservoir into a measure of string-building time, polluting
// the very signal it reports. (The pre-existing /health + /v1/metrics handlers
// DO self-record — same issue, deferred to P2 to avoid changing pre-existing
// behaviour + tests inside this endpoint's commit.)
func (s *Server) handleMetricsPrometheus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := w.Write(renderPrometheus(s.measure.Snapshot())); err != nil {
		s.log.Error("response write failed",
			slog.String("path", "/metrics"),
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
// JSON string ("text") or an array of content blocks. Returns ("", false)
// for null or unparseable input. Counts text and thinking blocks (Stage
// 2.6c — thinking carries prompt-derived content when echoed back). Other
// block types (image, tool_use, tool_result) are skipped — their JSON
// bodies would otherwise count brackets and keys.
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
			switch b.Type {
			case "text":
				if sb.Len() > 0 {
					sb.WriteByte(' ')
				}
				sb.WriteString(b.Text)
			case "thinking":
				// Stage 2.6c — thinking text is real prompt-derived
				// content; counting keeps shim_total honest vs.
				// upstream_prompt_total when clients echo thinking back.
				if sb.Len() > 0 {
					sb.WriteByte(' ')
				}
				sb.WriteString(b.Thinking)
			}
		}
		return sb.String(), true
	}
	return "", false
}

// handleMessages — POST /v1/messages.
//
// Flow: read+cap body → parse → cap stop_sequences → branch on stream →
// translate to OpenAI → adapter.BuildRequest → client.Do →
// adapter.NormalizeResponse → translate back to Anthropic → write JSON.
//
// Every loud-fail path emits an Anthropic-shaped error and logs the event
// with redacted attrs. Stage 2.6c removed the assistant-side thinking-
// block 501 (now translates to reasoning_content); user-side thinking
// still rejected at translate.go per the Anthropic spec.
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

	if req.Stream {
		s.handleMessagesStream(w, r, &req, body)
		return
	}

	t := s.adapter.Translator()
	mappedModel := s.adapter.MapModel(req.Model)
	s.logModelRewrite(req.Model, mappedModel)

	openaiBody, stopCapped, err := t.ToUpstream(&req, body, mappedModel)
	if err != nil {
		writeError(w, s.log, http.StatusBadRequest, errInvalidRequest,
			"translation: "+err.Error())
		return
	}
	s.recordStopCap(&req, stopCapped)

	httpReq, err := s.adapter.BuildRequest(adapter.WithInboundHeaders(r.Context(), r.Header), openaiBody)
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
		// Forward rate-limit/tracing headers on errors too — a 429's
		// Retry-After / anthropic-ratelimit-* is exactly when a client needs them.
		copyForwardedHeaders(w.Header(), upstream.Header)
		// A 2xx that NormalizeResponse still rejected is a body/transport read
		// failure, not an upstream error response — always a gateway failure,
		// dialect-independent (FromUpstreamError keys on status and would
		// mis-render a 2xx, e.g. identity would forward 200 + an empty body).
		if upstream.StatusCode >= 200 && upstream.StatusCode < 300 {
			writeError(w, s.log, http.StatusBadGateway, errAPI,
				"read upstream response: "+err.Error())
			return
		}
		s.writeUpstreamError(w, "/v1/messages", mappedModel, upstream.StatusCode, normalised)
		return
	}

	respBody, usage, err := t.FromUpstream(normalised, req.Model)
	if err != nil {
		writeError(w, s.log, upstreamErrStatus(err), errAPI, err.Error())
		return
	}

	// Record AFTER back-translation succeeds so a failed translate doesn't
	// credit shim_total for a request the client never saw a 200 for.
	// Collector additionally no-ops on zero Usage (some upstreams omit it).
	s.measure.RecordTokenDelta("/v1/messages",
		inputTokens(&req),
		usage.InputTokens,
		usage.OutputTokens,
	)

	copyForwardedHeaders(w.Header(), upstream.Header)
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBody); err != nil {
		// Headers already committed; can't change status. Log for visibility.
		s.log.Error("response write failed", slog.String("path", "/v1/messages"), slog.String("error", err.Error()))
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

// writeUpstreamError renders a non-2xx upstream response for the client via the
// adapter's Translator (FromUpstreamError) — the dialect owns whether the status
// and body pass through verbatim (identity: native Anthropic, already correct)
// or are re-classified into a fresh Anthropic envelope (OpenAI dialect: the
// upstream body is OpenAI-shaped, may carry prompt content, and is dropped).
//
// The server owns the surrounding concerns. It records the upstream error for
// /v1/metrics and logs a single diagnostic `upstream error` line carrying both
// the upstream status and the client-facing status plus a capped body_preview —
// operator-facing only, never echoed to the client (thesis-1: honest boundary
// visibility; see README "Errors and debugging"). endpoint buckets the metric;
// resolvedModel (upstream name after Adapter.MapModel) joins the line to the
// prior "model rewritten" breadcrumb without timestamp triangulation.
func (s *Server) writeUpstreamError(w http.ResponseWriter, endpoint, resolvedModel string, upstreamStatus int, body []byte) {
	s.measure.RecordUpstreamError(endpoint, upstreamStatus)

	clientStatus, clientBody := s.adapter.Translator().FromUpstreamError(upstreamStatus, body)

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
		slog.Int("status", clientStatus),
		slog.String("resolved_model", resolvedModel),
		slog.String("body_preview", string(preview)),
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(clientStatus)
	if _, err := w.Write(clientBody); err != nil {
		s.log.Error("error-response write failed",
			slog.String("endpoint", endpoint),
			slog.String("error", err.Error()),
		)
	}
}
