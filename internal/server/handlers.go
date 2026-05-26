package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/1mb-dev/shim/internal/measure"
	"github.com/1mb-dev/shim/internal/tokens"
	"github.com/1mb-dev/shim/internal/translate"
)

// maxStopSequences is the OpenAI-imposed cap on stop[] entries. Requests
// over this are truncated at the server boundary with a warn log rather
// than forwarded to a 400 that looks like a shim bug.
const maxStopSequences = 4

// handleHealth — GET /health → {"status":"ok"}.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	start := time.Now()
	defer func() { s.measure.RecordLatency("/health", time.Since(start)) }()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// logModelRewrite emits a breadcrumb when the adapter rewrites the
// requested model name + records the rewrite event for /v1/metrics. Stage
// 0 thesis-2: never silently forward modified traffic.
func (s *Server) logModelRewrite(requested, resolved string) {
	if requested == "" || requested == resolved {
		return
	}
	s.log.Info("model rewritten",
		slog.String("requested", requested),
		slog.String("resolved", resolved),
		slog.String("adapter", s.adapter.Name()),
	)
	s.measure.RecordRewriteEvent(measure.RewriteModel)
}

// approxInputTokens returns shim's chars/4 approximation of the request's
// input prompt — system blocks + every message's content, space-joined.
// Used both by /v1/messages/count_tokens (user-facing) and by the
// measurement collector (delta vs. upstream prompt_tokens).
func approxInputTokens(req *translate.AnthropicRequest) int {
	var parts []string
	if len(req.System) > 0 {
		parts = append(parts, string(req.System))
	}
	for _, m := range req.Messages {
		parts = append(parts, string(m.Content))
	}
	return tokens.Approximate(strings.Join(parts, " "))
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

	// OpenAI-compatible upstreams reject stop arrays larger than 4 with a
	// 400 that looks like a shim bug; cap loudly per thesis-2.
	if n := len(req.StopSequences); n > maxStopSequences {
		s.log.Warn("stop_sequences truncated",
			slog.Int("from", n),
			slog.Int("to", maxStopSequences),
		)
		req.StopSequences = req.StopSequences[:maxStopSequences]
		s.measure.RecordRewriteEvent(measure.RewriteStopSequences)
	}

	if containsThinkingBlock(req.Messages) {
		writeError(w, s.log, http.StatusNotImplemented, errInvalidRequest,
			"extended thinking not yet supported")
		return
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
		s.writeUpstreamError(w, upstream.StatusCode, normalised, err)
		return
	}

	var openaiResp translate.OpenAIResponse
	if err := json.Unmarshal(normalised, &openaiResp); err != nil {
		writeError(w, s.log, http.StatusBadGateway, errAPI,
			"upstream returned malformed JSON: "+err.Error())
		return
	}

	s.measure.RecordTokenDelta("/v1/messages",
		approxInputTokens(&req),
		openaiResp.Usage.PromptTokens,
		openaiResp.Usage.CompletionTokens,
	)

	anthropicResp, err := translate.OpenAIToAnthropic(&openaiResp, req.Model)
	if err != nil {
		writeError(w, s.log, http.StatusInternalServerError, errAPI,
			"translation back: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(anthropicResp); err != nil {
		// Headers already committed; can't change status. Log for visibility.
		s.log.Error("response encode failed", slog.String("path", "/v1/messages"), slog.String("error", err.Error()))
	}
}

// handleCountTokens — POST /v1/messages/count_tokens → {"input_tokens": N}.
// Stage 0 uses chars/4 approximation; the README discloses this adjacent
// to the usage.*_tokens docs.
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
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

	n := approxInputTokens(&req)
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
// Anthropic-shaped error class. The upstream body is NOT echoed (it can
// contain prompt content or other sensitive material).
func (s *Server) writeUpstreamError(w http.ResponseWriter, upstreamStatus int, _ []byte, err error) {
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
