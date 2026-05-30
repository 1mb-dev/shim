package server

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/1mb-dev/shim/internal/adapter"
	"github.com/1mb-dev/shim/internal/translate"
)

// streamSSE writes wire-ready Anthropic SSE chunks from next to w, flushing
// after each so clients see events in order. Sets event-stream headers and a
// 200 before the first chunk. Returns the first iterator/write error; the
// connection cannot be recovered mid-stream. The handler owns the writer; the
// translator owns chunk production, so dialect stays out of the server.
func streamSSE(w http.ResponseWriter, next func() ([]byte, bool, error)) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("ResponseWriter does not support Flush")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	for {
		chunk, ok, err := next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if _, err := w.Write(chunk); err != nil {
			return err
		}
		flusher.Flush()
	}
}

// handleMessagesStream handles a stream:true request via the adapter's
// Translator. For OpenAI-dialect upstreams this is buffer-then-restream (the
// translator drives the upstream non-streaming and synthesizes the canonical
// SSE sequence); for anthropic-passthrough it is true byte-passthrough. The
// handler is dialect-free: it gates on HTTP status, then writes the chunks the
// translator yields.
//
// Errors discovered BEFORE the SSE stream starts go out as Anthropic-shaped
// JSON via writeError; errors mid-stream are logged and the connection
// dropped (we cannot retroactively change response status).
func (s *Server) handleMessagesStream(w http.ResponseWriter, r *http.Request, req *translate.AnthropicRequest, raw []byte) {
	t := s.adapter.Translator()
	mappedModel := s.adapter.MapModel(req.Model)
	s.logModelRewrite(req.Model, mappedModel)

	openaiBody, stopCapped, err := t.ToUpstream(req, raw, mappedModel)
	if err != nil {
		writeError(w, s.log, http.StatusBadRequest, errInvalidRequest,
			"translation: "+err.Error())
		return
	}
	s.recordStopCap(req, stopCapped)

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
	defer upstream.Body.Close()

	// Transport-level status gate. The stream path skips NormalizeResponse
	// (Fork 2-a: a native-Anthropic passthrough cannot normalize a live SSE
	// stream into bytes), so upstream non-2xx is handled here — behaviour-
	// equivalent to NormalizeResponse's error path on the non-stream path.
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		body, _ := io.ReadAll(upstream.Body)
		copyForwardedHeaders(w.Header(), upstream.Header)
		s.writeUpstreamError(w, "/v1/messages", mappedModel, upstream.StatusCode, body)
		return
	}

	next, usage, err := t.StreamChunks(upstream, req.Model)
	if err != nil {
		writeError(w, s.log, upstreamErrStatus(err), errAPI, err.Error())
		return
	}

	// Record AFTER the iterator is built so a failed translate doesn't credit
	// shim_total. Deferred so it fires once the stream drains: for true
	// passthrough, usage is only known after the final message_delta is read.
	// Collector no-ops on zero Usage; the nil guard defends the interface
	// contract (usage non-nil on success) against a future translator.
	defer func() {
		if usage != nil {
			s.measure.RecordTokenDelta("/v1/messages", inputTokens(req), usage.InputTokens, usage.OutputTokens)
		}
	}()

	copyForwardedHeaders(w.Header(), upstream.Header)
	if err := streamSSE(w, next); err != nil {
		// Connection already in SSE mode (or flush unsupported); can only log.
		// Covers both a client write failure and an upstream read/iterator error.
		s.log.Error("sse stream failed", slog.String("error", err.Error()))
	}
}
