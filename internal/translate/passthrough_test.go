package translate

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIdentityStreamChunks_VerbatimAndUsage(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(stream))}

	next, usage, err := identity{}.StreamChunks(resp, "claude-x")
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	events := 0
	for {
		chunk, ok, err := next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got.WriteString(string(chunk))
		events++
	}
	if got.String() != stream {
		t.Errorf("stream not byte-identical:\n got: %q\nwant: %q", got.String(), stream)
	}
	if events != 4 {
		t.Errorf("event count = %d, want 4", events)
	}
	// input from message_start, output from message_delta.
	if usage.InputTokens != 7 || usage.OutputTokens != 4 {
		t.Errorf("usage = %+v, want {input:7, output:4}", *usage)
	}
}

// Anthropic content/thinking events can exceed bufio's 4 KB default and the
// 64 KB reader buffer; bufio.Reader.ReadBytes accumulates past the buffer, so
// the event must survive intact.
func TestIdentityStreamChunks_LargeEvent(t *testing.T) {
	big := strings.Repeat("x", 200*1024)
	stream := "event: content_block_delta\ndata: {\"text\":\"" + big + "\"}\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(stream))}

	next, _, err := identity{}.StreamChunks(resp, "x")
	if err != nil {
		t.Fatal(err)
	}
	chunk, ok, err := next()
	if err != nil || !ok {
		t.Fatalf("first event: ok=%v err=%v", ok, err)
	}
	if string(chunk) != stream {
		t.Errorf("large event not preserved (got %d bytes, want %d)", len(chunk), len(stream))
	}
	if _, ok, _ := next(); ok {
		t.Error("expected end of stream after the single event")
	}
}

// TestIdentityStreamChunks_Incremental proves the forward is NOT
// buffer-then-restream: next() returns event 1 while event 2 is still
// unwritten. If StreamChunks buffered the whole stream it would block forever
// on the first call (no EOF yet).
func TestIdentityStreamChunks_Incremental(t *testing.T) {
	pr, pw := io.Pipe()
	resp := &http.Response{Body: pr}
	next, _, err := identity{}.StreamChunks(resp, "x")
	if err != nil {
		t.Fatal(err)
	}
	const ev1 = "event: message_start\ndata: {}\n\n"
	const ev2 = "event: message_stop\ndata: {}\n\n"

	go func() { _, _ = pw.Write([]byte(ev1)) }() // event 2 deliberately withheld
	got1, ok, err := next()
	if err != nil || !ok || string(got1) != ev1 {
		t.Fatalf("event 1 not delivered before event 2 written: ok=%v err=%v got=%q", ok, err, got1)
	}

	go func() { _, _ = pw.Write([]byte(ev2)); pw.Close() }()
	got2, ok, _ := next()
	if !ok || string(got2) != ev2 {
		t.Errorf("event 2 = %q", got2)
	}
	if _, ok, _ := next(); ok {
		t.Error("expected end of stream")
	}
}

// A final event without a trailing blank line (stream cut at EOF) must still be
// yielded, not dropped.
func TestIdentityStreamChunks_NoTrailingBlankLine(t *testing.T) {
	stream := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(stream))}
	next, _, err := identity{}.StreamChunks(resp, "x")
	if err != nil {
		t.Fatal(err)
	}
	chunk, ok, _ := next()
	if !ok || string(chunk) != stream {
		t.Errorf("trailing event dropped: ok=%v chunk=%q", ok, chunk)
	}
}
