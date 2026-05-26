package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/1mb-dev/shim/internal/translate"
)

// writeSSE emits the ordered Anthropic SSE events to w using the documented
// wire format: `event: <name>\ndata: <json>\n\n`. Flushes after each event so
// clients see the sequence in order. Returns the first write error (if any)
// so the caller can log; the connection cannot be recovered mid-stream.
func writeSSE(w http.ResponseWriter, events []translate.SSEEvent) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("ResponseWriter does not support Flush")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	for _, ev := range events {
		data, err := json.Marshal(ev.Data)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", ev.Name, err)
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, data); err != nil {
			return err
		}
		flusher.Flush()
	}
	return nil
}

// handleMessagesStream handles a stream:true request. Buffer-then-restream:
// drive the upstream as a non-streaming call (Stage 0 MVP), then emit the
// canonical Anthropic SSE event sequence in one burst.
//
// Errors discovered BEFORE the SSE stream starts go out as Anthropic-shaped
// JSON via writeError; errors mid-stream are logged and the connection
// dropped (we cannot retroactively change response status).
func (s *Server) handleMessagesStream(w http.ResponseWriter, r *http.Request, req *translate.AnthropicRequest) {
	openaiReq, err := translate.AnthropicToOpenAI(req)
	if err != nil {
		writeError(w, s.log, http.StatusBadRequest, errInvalidRequest,
			"translation: "+err.Error())
		return
	}
	openaiReq.Model = s.adapter.MapModel(req.Model)
	s.logModelRewrite(req.Model, openaiReq.Model)
	openaiReq.Stream = false // MVP: buffer-then-restream

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

	events, err := translate.ToAnthropicSSE(&openaiResp, req.Model)
	if err != nil {
		writeError(w, s.log, http.StatusInternalServerError, errAPI,
			"build SSE: "+err.Error())
		return
	}

	// Record AFTER SSE build succeeds so a failed translate doesn't credit
	// shim_total for a request that never produced a streamable event.
	// Collector additionally no-ops on zero Usage (some upstreams omit it).
	s.measure.RecordTokenDelta("/v1/messages",
		inputTokens(req),
		openaiResp.Usage.PromptTokens,
		openaiResp.Usage.CompletionTokens,
	)

	if err := writeSSE(w, events); err != nil {
		// Connection already in SSE mode; can only log.
		s.log.Error("sse write failed",
			slog.String("error", err.Error()),
		)
	}
}
