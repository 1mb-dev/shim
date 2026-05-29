package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// sseReadBuffer sizes the SSE reader generously — Anthropic thinking/content
// events can exceed bufio's 4 KB default. bufio.Reader grows past this as
// needed (unlike bufio.Scanner, which hard-caps), so large events never break
// the stream.
//
// The per-event buffer is unbounded by design: this is safe because the
// upstream is a configured, authenticated endpoint (trusted under shim's
// loopback threat model). A hostile upstream could send a never-terminated
// "event" and grow the buffer until OOM — if shim ever fronts an untrusted
// upstream, cap the accumulation in readSSEEvent.
const sseReadBuffer = 64 * 1024

// StreamChunks forwards a native-Anthropic SSE response verbatim, one event at
// a time, while sniffing token usage from the message_start / message_delta
// events for honest measurement (thesis-1). It is pull-based (a closure, not a
// goroutine): the server's write loop drives it and owns cancellation via the
// request context, so a client disconnect simply stops the pulls — nothing
// leaks. usage is populated as events arrive and is final by the time next
// reports ok=false.
func (identity) StreamChunks(resp *http.Response, _ string) (func() ([]byte, bool, error), *AnthropicUsage, error) {
	r := bufio.NewReaderSize(resp.Body, sseReadBuffer)
	usage := &AnthropicUsage{}
	next := func() ([]byte, bool, error) {
		event, err := readSSEEvent(r)
		if err != nil && err != io.EOF {
			return nil, false, err
		}
		if len(event) == 0 {
			return nil, false, nil // clean end of stream
		}
		sniffSSEUsage(event, usage)
		return event, true, nil
	}
	return next, usage, nil
}

// readSSEEvent reads one SSE event from r — every byte up to and including the
// terminating blank line — preserving them verbatim so the client sees the
// upstream's exact framing. Returns any trailing bytes with io.EOF at stream
// end.
func readSSEEvent(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		line, err := r.ReadBytes('\n')
		buf = append(buf, line...)
		if err != nil {
			return buf, err
		}
		if isBlankLine(line) && len(buf) > len(line) {
			return buf, nil
		}
	}
}

func isBlankLine(line []byte) bool {
	return bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n"))
}

// sseUsageProbe captures usage from both Anthropic streaming shapes:
// message_start carries message.usage.input_tokens; message_delta carries
// usage.output_tokens.
type sseUsageProbe struct {
	Message struct {
		Usage AnthropicUsage `json:"usage"`
	} `json:"message"`
	Usage AnthropicUsage `json:"usage"`
}

// sniffSSEUsage best-effort folds any usage numbers in the event's data payload
// into u. Non-JSON or usage-free events are ignored — sniffing must never break
// the verbatim forward.
func sniffSSEUsage(event []byte, u *AnthropicUsage) {
	data := sseDataPayload(event)
	if len(data) == 0 {
		return
	}
	var p sseUsageProbe
	if json.Unmarshal(data, &p) != nil {
		return
	}
	if v := max(p.Message.Usage.InputTokens, p.Usage.InputTokens); v > 0 {
		u.InputTokens = v
	}
	if v := max(p.Message.Usage.OutputTokens, p.Usage.OutputTokens); v > 0 {
		u.OutputTokens = v
	}
}

// sseDataPayload returns the JSON after the first "data:" line of an SSE event.
func sseDataPayload(event []byte) []byte {
	for _, line := range bytes.Split(event, []byte("\n")) {
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			return bytes.TrimSpace(rest)
		}
	}
	return nil
}
