package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Translator error categories. The server maps these to HTTP status:
// ErrBackTranslation (a structurally-valid upstream response shim can't
// convert) is a 500; a malformed upstream body is a 502 (gateway problem).
var (
	ErrUpstreamMalformed = errors.New("upstream returned malformed JSON")
	ErrBackTranslation   = errors.New("back-translation failed")
)

// maxStopSequences is the OpenAI cap on stop[] entries. The cap lives in the
// OpenAI-dialect translator (not the server handler) so passthrough is
// unaffected.
const maxStopSequences = 4

// Translator converts between the Anthropic Messages wire format and one
// upstream transport dialect. shim supports two dialects: OpenAI
// ChatCompletions (DeepSeek, OpenAI — see AnthropicOpenAI) and identity
// (anthropic-passthrough — defined in the passthrough adapter).
//
// Implementations are pure for request/response (no client I/O, no config,
// no state). StreamChunks reads the upstream response body (input) but never
// touches the client ResponseWriter — the server handler owns the writer and
// the flush loop. This keeps dialect knowledge out of the handler entirely.
type Translator interface {
	// ToUpstream builds the upstream request body. req is the parsed request;
	// raw is the original inbound bytes (the identity translator forwards them
	// verbatim, so client fields shim doesn't model survive). mappedModel is
	// the upstream model name (from Adapter.MapModel). stopCapped reports how
	// many stop sequences the dialect dropped, so the server emits the
	// loud-fail metric/log (thesis-2) while the translator stays pure. The
	// implementation owns the upstream `stream` flag for its dialect.
	ToUpstream(req *AnthropicRequest, raw []byte, mappedModel string) (body []byte, stopCapped int, err error)

	// FromUpstream converts a buffered (non-streaming) upstream response body
	// into the Anthropic-shaped response BYTES the client should receive, plus
	// normalized usage so the caller records the token delta without knowing
	// the dialect. Returning bytes (not a struct) lets the identity translator
	// forward the upstream body verbatim — lossless passthrough, no field-drop
	// from re-encoding through shim's response struct.
	FromUpstream(body []byte, originalModel string) (respBody []byte, usage AnthropicUsage, err error)

	// StreamChunks returns a pull iterator over wire-ready Anthropic SSE event
	// bytes (`event: <name>\ndata: <json>\n\n`). The caller writes + flushes
	// each chunk. usage is populated by the time next reports ok=false. The
	// implementation owns reading resp.Body; the caller has already gated on a
	// 2xx upstream status.
	StreamChunks(resp *http.Response, originalModel string) (next func() ([]byte, bool, error), usage *AnthropicUsage, err error)
}

// AnthropicOpenAI returns the Translator for OpenAI-ChatCompletions-dialect
// upstreams. It wraps the pure AnthropicToOpenAI / OpenAIToAnthropic /
// ToAnthropicSSE functions. Streaming is buffer-then-restream: the upstream
// is driven non-streaming and the canonical SSE sequence is synthesized from
// the complete response (true per-token streaming for translating providers
// is out of v1.0 scope).
func AnthropicOpenAI() Translator { return anthropicOpenAI{} }

type anthropicOpenAI struct{}

func (anthropicOpenAI) ToUpstream(req *AnthropicRequest, _ []byte, mappedModel string) ([]byte, int, error) {
	o, err := AnthropicToOpenAI(req)
	if err != nil {
		return nil, 0, err
	}
	o.Model = mappedModel
	o.Stream = false // MVP: buffer-then-restream
	// OpenAI rejects >maxStopSequences stop strings with a 400 that looks like
	// a shim bug. Cap here (a dialect concern) instead of in the dialect-
	// agnostic handler, so passthrough is unaffected; report the drop so the
	// server emits the loud-fail metric/log.
	stopCapped := 0
	if n := len(o.Stop); n > maxStopSequences {
		stopCapped = n - maxStopSequences
		o.Stop = o.Stop[:maxStopSequences]
	}
	// Marshal cannot fail here: every json.RawMessage in o originates from a
	// successfully-unmarshaled inbound request, so the only realistic error
	// source is the client's request shape (already surfaced by
	// AnthropicToOpenAI above) — the server treats a ToUpstream error as a 400.
	b, err := json.Marshal(o)
	if err != nil {
		return nil, 0, err
	}
	return b, stopCapped, nil
}

func (anthropicOpenAI) FromUpstream(body []byte, originalModel string) ([]byte, AnthropicUsage, error) {
	o, err := decodeOpenAIResponse(body)
	if err != nil {
		return nil, AnthropicUsage{}, err
	}
	resp, err := OpenAIToAnthropic(o, originalModel)
	if err != nil {
		return nil, AnthropicUsage{}, fmt.Errorf("%w: %w", ErrBackTranslation, err)
	}
	respBody, err := json.Marshal(resp)
	if err != nil {
		// Marshalling shim's own response struct: unreachable in practice.
		return nil, AnthropicUsage{}, fmt.Errorf("%w: encode response: %w", ErrBackTranslation, err)
	}
	return respBody, openAIUsage(o), nil
}

func (anthropicOpenAI) StreamChunks(resp *http.Response, originalModel string) (func() ([]byte, bool, error), *AnthropicUsage, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read upstream: %w", err)
	}
	o, err := decodeOpenAIResponse(body)
	if err != nil {
		return nil, nil, err
	}
	events, err := ToAnthropicSSE(o, originalModel)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrBackTranslation, err)
	}
	// Buffered upstream: usage is known up front.
	usage := openAIUsage(o)
	i := 0
	next := func() ([]byte, bool, error) {
		if i >= len(events) {
			return nil, false, nil
		}
		b, err := marshalSSE(events[i])
		i++
		if err != nil {
			return nil, false, err
		}
		return b, true, nil
	}
	return next, &usage, nil
}

// Identity returns the passthrough Translator for a native-Anthropic upstream:
// the request is forwarded verbatim and the response body is returned
// unchanged, with no dialect conversion. Used by the anthropic-passthrough
// adapter. Forwarding bytes verbatim is the point — shim must not drop
// Anthropic response fields it doesn't model.
func Identity() Translator { return identity{} }

type identity struct{}

// ToUpstream forwards the client's request BYTES verbatim — fields shim does
// not model (metadata, top_k, service_tier, …) survive, which re-marshalling
// the parsed struct would silently drop. No stop-sequence cap: that's an
// OpenAI-dialect concern, not Anthropic's. mappedModel is unused (the adapter's
// MapModel is identity); req is unused (raw is authoritative).
func (identity) ToUpstream(_ *AnthropicRequest, raw []byte, _ string) ([]byte, int, error) {
	return raw, 0, nil
}

// FromUpstream returns the upstream body verbatim (lossless) and sniffs usage
// for measurement without re-encoding through shim's response struct.
func (identity) FromUpstream(body []byte, _ string) ([]byte, AnthropicUsage, error) {
	var probe struct {
		Usage AnthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, AnthropicUsage{}, fmt.Errorf("%w: %w", ErrUpstreamMalformed, err)
	}
	return body, probe.Usage, nil
}

func decodeOpenAIResponse(body []byte) (*OpenAIResponse, error) {
	var o OpenAIResponse
	if err := json.Unmarshal(body, &o); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUpstreamMalformed, err)
	}
	return &o, nil
}

func openAIUsage(o *OpenAIResponse) AnthropicUsage {
	return AnthropicUsage{
		InputTokens:  o.Usage.PromptTokens,
		OutputTokens: o.Usage.CompletionTokens,
	}
}

// marshalSSE renders one SSE event to its wire bytes:
// `event: <name>\ndata: <json>\n\n`.
func marshalSSE(ev SSEEvent) ([]byte, error) {
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", ev.Name, err)
	}
	return fmt.Appendf(nil, "event: %s\ndata: %s\n\n", ev.Name, data), nil
}
