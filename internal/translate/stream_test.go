package translate

import (
	"encoding/json"
	"testing"
)

func TestToAnthropicSSE_TextOnly(t *testing.T) {
	resp := &OpenAIResponse{
		ID: "chat-1", Model: "deepseek-chat",
		Choices: []OpenAIChoice{{
			Message: OpenAIMessage{
				Role:    "assistant",
				Content: mustJSON("hello world"),
			},
			FinishReason: "stop",
		}},
		Usage: OpenAIUsage{PromptTokens: 5, CompletionTokens: 2},
	}
	events, err := ToAnthropicSSE(resp, "claude-3-5")
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if len(events) != len(wantOrder) {
		t.Fatalf("event count = %d, want %d (%v)", len(events), len(wantOrder),
			eventNames(events))
	}
	for i, want := range wantOrder {
		if events[i].Name != want {
			t.Errorf("events[%d] = %q, want %q", i, events[i].Name, want)
		}
	}

	// message_start carries the original Anthropic model name.
	ms := events[0].Data.(sseMessageStart)
	if ms.Message.Model != "claude-3-5" {
		t.Errorf("message_start model = %q", ms.Message.Model)
	}
	if ms.Message.ID != "chat-1" {
		t.Errorf("message_start id = %q", ms.Message.ID)
	}
	if ms.Message.Usage.InputTokens != 5 {
		t.Errorf("message_start usage.input_tokens = %d", ms.Message.Usage.InputTokens)
	}

	// content_block_delta carries the text body.
	cbd := events[2].Data.(sseContentBlockDelta)
	if cbd.Delta.Type != "text_delta" || cbd.Delta.Text != "hello world" {
		t.Errorf("delta = %+v", cbd.Delta)
	}

	// message_delta carries the final stop_reason and output_tokens.
	md := events[4].Data.(sseMessageDelta)
	if md.Delta.StopReason != "end_turn" {
		t.Errorf("message_delta stop_reason = %q", md.Delta.StopReason)
	}
	if md.Usage.OutputTokens != 2 {
		t.Errorf("message_delta output_tokens = %d", md.Usage.OutputTokens)
	}
}

func TestToAnthropicSSE_ToolCall(t *testing.T) {
	resp := &OpenAIResponse{
		ID: "chat-2", Model: "x",
		Choices: []OpenAIChoice{{
			Message: OpenAIMessage{
				Role: "assistant",
				ToolCalls: []OpenAIToolCall{{
					ID: "call_99", Type: "function",
					Function: OpenAIToolCallBody{Name: "search", Arguments: `{"q":"x"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
		Usage: OpenAIUsage{PromptTokens: 7, CompletionTokens: 3},
	}
	events, err := ToAnthropicSSE(resp, "claude")
	if err != nil {
		t.Fatal(err)
	}
	// Tool-use ⇒ one block: start, delta(input_json_delta), stop.
	// Plus message_start/message_delta/message_stop bookends.
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	got := eventNames(events)
	if !sameStrings(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	cbs := events[1].Data.(sseContentBlockStart)
	if cbs.ContentBlock.Type != "tool_use" || cbs.ContentBlock.ID != "call_99" {
		t.Errorf("content_block_start = %+v", cbs.ContentBlock)
	}
	cbd := events[2].Data.(sseContentBlockDelta)
	if cbd.Delta.Type != "input_json_delta" || cbd.Delta.PartialJSON != `{"q":"x"}` {
		t.Errorf("delta = %+v", cbd.Delta)
	}
	md := events[4].Data.(sseMessageDelta)
	if md.Delta.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", md.Delta.StopReason)
	}
}

// TestToAnthropicSSE_Thinking — Stage 2.6c. Reasoning content emits a
// thinking content block in the SSE stream: content_block_start (empty
// thinking + empty sig), thinking_delta (text), signature_delta (constant
// sig), content_block_stop. Block ordering: thinking precedes text.
func TestToAnthropicSSE_Thinking(t *testing.T) {
	resp := &OpenAIResponse{
		ID: "chat-3", Model: "deepseek-v4-pro",
		Choices: []OpenAIChoice{{
			Message: OpenAIMessage{
				Role:             "assistant",
				Content:          json.RawMessage(`"final answer"`),
				ReasoningContent: "let me think...",
			},
			FinishReason: "stop",
		}},
		Usage: OpenAIUsage{PromptTokens: 7, CompletionTokens: 3},
	}
	events, err := ToAnthropicSSE(resp, "claude")
	if err != nil {
		t.Fatal(err)
	}
	// Thinking block (3 events: start, thinking_delta, signature_delta) +
	// content_block_stop. Then text block (3 events: start, delta, stop).
	// Bookends: message_start, message_delta, message_stop. = 11 events.
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	got := eventNames(events)
	if !sameStrings(got, want) {
		t.Fatalf("events = %v\nwant %v", got, want)
	}
	// Thinking block first (ordering invariant).
	cbs0 := events[1].Data.(sseContentBlockStart)
	if cbs0.ContentBlock.Type != "thinking" {
		t.Errorf("block[0] type = %q, want thinking", cbs0.ContentBlock.Type)
	}
	td := events[2].Data.(sseContentBlockDelta)
	if td.Delta.Type != "thinking_delta" || td.Delta.Thinking != "let me think..." {
		t.Errorf("thinking_delta = %+v", td.Delta)
	}
	sd := events[3].Data.(sseContentBlockDelta)
	if sd.Delta.Type != "signature_delta" || sd.Delta.Signature != "shim-passthrough-v1" {
		t.Errorf("signature_delta = %+v", sd.Delta)
	}
}

func TestToAnthropicSSE_EmptyContent(t *testing.T) {
	// Empty assistant content collapses to a single empty text block per
	// messageToBlocks; the SSE sequence must still produce the canonical 6
	// events.
	resp := &OpenAIResponse{
		Choices: []OpenAIChoice{{
			Message:      OpenAIMessage{Role: "assistant"},
			FinishReason: "stop",
		}},
	}
	events, err := ToAnthropicSSE(resp, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 6 {
		t.Fatalf("event count = %d, want 6", len(events))
	}
	cbd := events[2].Data.(sseContentBlockDelta)
	if cbd.Delta.Text != "" {
		t.Errorf("empty text should be empty, got %q", cbd.Delta.Text)
	}
}

func TestToAnthropicSSE_NoChoices(t *testing.T) {
	if _, err := ToAnthropicSSE(&OpenAIResponse{}, "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestToAnthropicSSE_Nil(t *testing.T) {
	if _, err := ToAnthropicSSE(nil, "x"); err == nil {
		t.Fatal("expected error")
	}
}

// TestSSEEventJSON asserts an event marshals as valid JSON (so the server's
// wire writer can format it as `event: NAME\ndata: <JSON>\n\n`).
func TestSSEEventJSON(t *testing.T) {
	resp := &OpenAIResponse{
		ID: "x", Choices: []OpenAIChoice{{
			Message:      OpenAIMessage{Role: "assistant", Content: mustJSON("hi")},
			FinishReason: "stop",
		}},
		Usage: OpenAIUsage{PromptTokens: 1, CompletionTokens: 1},
	}
	events, _ := ToAnthropicSSE(resp, "x")
	for _, e := range events {
		raw, err := json.Marshal(e.Data)
		if err != nil {
			t.Errorf("event %q marshal failed: %v", e.Name, err)
		}
		var any map[string]any
		if err := json.Unmarshal(raw, &any); err != nil {
			t.Errorf("event %q unmarshal failed: %v", e.Name, err)
		}
		if any["type"] != e.Name {
			t.Errorf("event %q has type=%v in JSON, mismatch", e.Name, any["type"])
		}
	}
}

func eventNames(events []SSEEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Name
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
