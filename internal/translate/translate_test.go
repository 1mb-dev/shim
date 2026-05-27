package translate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// --- AnthropicToOpenAI ---

func TestAnthropicToOpenAI_SystemString(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-3-5",
		MaxTokens: 100,
		System:    mustJSON("you are helpful"),
		Messages: []AnthropicMessage{
			{Role: "user", Content: mustJSON("hi")},
		},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(out.Messages))
	}
	if out.Messages[0].Role != "system" {
		t.Errorf("first role = %q, want system", out.Messages[0].Role)
	}
	var sysContent string
	_ = json.Unmarshal(out.Messages[0].Content, &sysContent)
	if sysContent != "you are helpful" {
		t.Errorf("system content = %q", sysContent)
	}
}

func TestAnthropicToOpenAI_SystemBlocks(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-3-5",
		MaxTokens: 100,
		System: mustJSON([]AnthropicBlock{
			{Type: "text", Text: "you are helpful"},
			{Type: "text", Text: "be terse"},
		}),
		Messages: []AnthropicMessage{
			{Role: "user", Content: mustJSON("hi")},
		},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	var sysContent string
	_ = json.Unmarshal(out.Messages[0].Content, &sysContent)
	if sysContent != "you are helpful\nbe terse" {
		t.Errorf("system concat = %q", sysContent)
	}
}

func TestAnthropicToOpenAI_SystemEmpty(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-3-5",
		MaxTokens: 100,
		Messages: []AnthropicMessage{
			{Role: "user", Content: mustJSON("hi")},
		},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Errorf("expected single user message, got %v", out.Messages)
	}
}

func TestAnthropicToOpenAI_SystemNonTextBlockRejected(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-3-5",
		MaxTokens: 100,
		System: mustJSON([]AnthropicBlock{
			{Type: "image", Source: &AnthropicImageSource{Type: "url", URL: "x"}},
		}),
		Messages: []AnthropicMessage{{Role: "user", Content: mustJSON("hi")}},
	}
	_, err := AnthropicToOpenAI(req)
	if err == nil {
		t.Fatal("expected error for image-in-system")
	}
}

func TestAnthropicToOpenAI_UserStringContent(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "x",
		MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "user", Content: mustJSON("hello")},
		},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	var c string
	_ = json.Unmarshal(out.Messages[0].Content, &c)
	if c != "hello" {
		t.Errorf("content = %q", c)
	}
}

func TestAnthropicToOpenAI_UserTextBlock(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "x",
		MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "user", Content: mustJSON([]AnthropicBlock{
				{Type: "text", Text: "hello"},
			})},
		},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	var c string
	_ = json.Unmarshal(out.Messages[0].Content, &c)
	if c != "hello" {
		t.Errorf("single-text-block should collapse to string, got %q", c)
	}
}

func TestAnthropicToOpenAI_UserMixedTextImage(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "x",
		MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "user", Content: mustJSON([]AnthropicBlock{
				{Type: "text", Text: "describe this"},
				{Type: "image", Source: &AnthropicImageSource{
					Type: "base64", MediaType: "image/png", Data: "iVBORw0KGgo=",
				}},
			})},
		},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	var parts []OpenAIContentPart
	if err := json.Unmarshal(out.Messages[0].Content, &parts); err != nil {
		t.Fatalf("expected array content for multi-part: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts len = %d", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "describe this" {
		t.Errorf("parts[0] = %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Fatalf("parts[1] = %+v", parts[1])
	}
	want := "data:image/png;base64,iVBORw0KGgo="
	if parts[1].ImageURL.URL != want {
		t.Errorf("data URL = %q, want %q", parts[1].ImageURL.URL, want)
	}
}

func TestAnthropicToOpenAI_ImageURL(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "user", Content: mustJSON([]AnthropicBlock{
				{Type: "image", Source: &AnthropicImageSource{
					Type: "url", URL: "https://example.com/cat.png",
				}},
			})},
		},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	var parts []OpenAIContentPart
	_ = json.Unmarshal(out.Messages[0].Content, &parts)
	if parts[0].ImageURL.URL != "https://example.com/cat.png" {
		t.Errorf("url passthrough = %q", parts[0].ImageURL.URL)
	}
}

func TestAnthropicToOpenAI_ImageBadSource(t *testing.T) {
	cases := []struct {
		name string
		src  *AnthropicImageSource
	}{
		{"nil source", nil},
		{"base64 missing data", &AnthropicImageSource{Type: "base64", MediaType: "image/png"}},
		{"base64 missing media_type", &AnthropicImageSource{Type: "base64", Data: "x"}},
		{"url missing url", &AnthropicImageSource{Type: "url"}},
		{"unknown type", &AnthropicImageSource{Type: "blob"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &AnthropicRequest{
				Model: "x", MaxTokens: 1,
				Messages: []AnthropicMessage{
					{Role: "user", Content: mustJSON([]AnthropicBlock{
						{Type: "image", Source: tc.src},
					})},
				},
			}
			if _, err := AnthropicToOpenAI(req); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestAnthropicToOpenAI_AssistantText(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: mustJSON([]AnthropicBlock{
				{Type: "text", Text: "ok"},
				{Type: "text", Text: "got it"},
			})},
		},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	var c string
	_ = json.Unmarshal(out.Messages[0].Content, &c)
	if c != "ok\ngot it" {
		t.Errorf("assistant text join = %q", c)
	}
}

func TestAnthropicToOpenAI_StopSequences(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		StopSequences: []string{"\nUser:", "STOP"},
		Messages:      []AnthropicMessage{{Role: "user", Content: mustJSON("hi")}},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.Stop, []string{"\nUser:", "STOP"}) {
		t.Errorf("stop = %v", out.Stop)
	}
}

func TestAnthropicToOpenAI_TemperatureTopPPassthrough(t *testing.T) {
	tp := 0.7
	tt := 0.3
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Temperature: &tt, TopP: &tp,
		Messages: []AnthropicMessage{{Role: "user", Content: mustJSON("hi")}},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	if out.Temperature == nil || *out.Temperature != 0.3 {
		t.Errorf("temperature = %v", out.Temperature)
	}
	if out.TopP == nil || *out.TopP != 0.7 {
		t.Errorf("top_p = %v", out.TopP)
	}
}

// TestAnthropicToOpenAI_AssistantThinkingRoundtrips — Stage 2.6c lifted
// the assistant-side thinking 501. Thinking blocks now populate
// reasoning_content on the outbound OpenAI message; signature is
// discarded (DeepSeek doesn't accept it and shim doesn't verify on
// roundtrip — constant-string posture documented in README).
func TestAnthropicToOpenAI_AssistantThinkingRoundtrips(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: mustJSON([]AnthropicBlock{
				{Type: "thinking", Thinking: "deliberating...", Signature: "anything"},
				{Type: "text", Text: "final answer"},
			})},
		},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("expected 1 outbound message, got %d", len(out.Messages))
	}
	m := out.Messages[0]
	if m.ReasoningContent != "deliberating..." {
		t.Errorf("reasoning_content = %q, want %q", m.ReasoningContent, "deliberating...")
	}
	if !strings.Contains(string(m.Content), "final answer") {
		t.Errorf("content missing text part: %s", m.Content)
	}
}

// TestAnthropicToOpenAI_UserThinkingStillRejected — only assistant turns
// produce thinking per Anthropic's spec. User-side thinking blocks still
// loud-fail; lifting the assistant-side gate does not cascade.
func TestAnthropicToOpenAI_UserThinkingStillRejected(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "user", Content: mustJSON([]AnthropicBlock{
				{Type: "thinking", Thinking: "..."},
			})},
		},
	}
	_, err := AnthropicToOpenAI(req)
	if err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("expected user-side thinking error, got %v", err)
	}
}

// TestOpenAIToAnthropic_ReasoningContentBecomesThinkingBlock — response
// side. DeepSeek's reasoning_content becomes an Anthropic thinking block
// with the constant signature; block ordering is thinking-first (Anthropic
// spec: thinking precedes tool_use in assistant turns).
func TestOpenAIToAnthropic_ReasoningContentBecomesThinkingBlock(t *testing.T) {
	resp := &OpenAIResponse{
		ID:    "chatcmpl-x",
		Model: "deepseek-v4-pro",
		Choices: []OpenAIChoice{{
			Index: 0,
			Message: OpenAIMessage{
				Role:             "assistant",
				Content:          mustJSON("the answer"),
				ReasoningContent: "let me think...",
			},
			FinishReason: "stop",
		}},
	}
	out, err := OpenAIToAnthropic(resp, "claude-opus-4-7")
	if err != nil {
		t.Fatalf("OpenAIToAnthropic: %v", err)
	}
	if len(out.Content) != 2 {
		t.Fatalf("expected 2 blocks (thinking + text), got %d", len(out.Content))
	}
	if out.Content[0].Type != "thinking" {
		t.Errorf("block[0].type = %q, want thinking (ordering: thinking precedes text)", out.Content[0].Type)
	}
	if out.Content[0].Thinking != "let me think..." {
		t.Errorf("block[0].thinking = %q", out.Content[0].Thinking)
	}
	if out.Content[0].Signature != "shim-passthrough-v1" {
		t.Errorf("block[0].signature = %q, want constant sig", out.Content[0].Signature)
	}
	if out.Content[1].Type != "text" || out.Content[1].Text != "the answer" {
		t.Errorf("block[1] = %+v, want text/the answer", out.Content[1])
	}
}

// TestOpenAIToAnthropic_EmptyReasoningContentNoBlock — defensive: a
// response without reasoning content should NOT emit an empty thinking
// block. Zero-content thinking blocks are noise and violate the
// signature-implies-content invariant.
func TestOpenAIToAnthropic_EmptyReasoningContentNoBlock(t *testing.T) {
	resp := &OpenAIResponse{
		ID: "chatcmpl-x", Model: "x",
		Choices: []OpenAIChoice{{
			Index:        0,
			Message:      OpenAIMessage{Role: "assistant", Content: mustJSON("hi")},
			FinishReason: "stop",
		}},
	}
	out, err := OpenAIToAnthropic(resp, "claude-x")
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range out.Content {
		if b.Type == "thinking" {
			t.Errorf("block[%d] is thinking but reasoning_content was empty", i)
		}
	}
}

// TestOpenAIToAnthropic_ReasoningWithToolCallOrdering — Anthropic's spec
// says thinking precedes tool_use in assistant turns. With both present,
// shim emits thinking first.
func TestOpenAIToAnthropic_ReasoningWithToolCallOrdering(t *testing.T) {
	resp := &OpenAIResponse{
		ID: "chatcmpl-x", Model: "x",
		Choices: []OpenAIChoice{{
			Index: 0,
			Message: OpenAIMessage{
				Role:             "assistant",
				ReasoningContent: "deciding to use tool",
				ToolCalls: []OpenAIToolCall{{
					ID: "call_1", Type: "function",
					Function: OpenAIToolCallBody{Name: "get_weather", Arguments: `{"city":"SF"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
	out, err := OpenAIToAnthropic(resp, "claude-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 2 {
		t.Fatalf("expected 2 blocks (thinking + tool_use), got %d: %+v", len(out.Content), out.Content)
	}
	if out.Content[0].Type != "thinking" {
		t.Errorf("block[0] type = %q, want thinking (ordering invariant)", out.Content[0].Type)
	}
	if out.Content[1].Type != "tool_use" {
		t.Errorf("block[1] type = %q, want tool_use", out.Content[1].Type)
	}
}

// TestAnthropicToOpenAI_MultipleThinkingBlocksConcatenate — rare but the
// spec allows it. Shim concatenates with newline so the upstream sees one
// reasoning_content string.
func TestAnthropicToOpenAI_MultipleThinkingBlocksConcatenate(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: mustJSON([]AnthropicBlock{
				{Type: "thinking", Thinking: "first"},
				{Type: "thinking", Thinking: "second"},
				{Type: "text", Text: "done"},
			})},
		},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	want := "first\nsecond"
	if out.Messages[0].ReasoningContent != want {
		t.Errorf("reasoning_content = %q, want %q", out.Messages[0].ReasoningContent, want)
	}
}

func TestAnthropicToOpenAI_NilRequest(t *testing.T) {
	if _, err := AnthropicToOpenAI(nil); err == nil {
		t.Fatal("expected error for nil request")
	}
}

func TestAnthropicToOpenAI_UnknownRole(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Messages: []AnthropicMessage{{Role: "wizard", Content: mustJSON("hi")}},
	}
	if _, err := AnthropicToOpenAI(req); err == nil {
		t.Fatal("expected error for unknown role")
	}
}

// --- OpenAIToAnthropic ---

func TestOpenAIToAnthropic_TextResponse(t *testing.T) {
	resp := &OpenAIResponse{
		ID:    "chatcmpl-abc",
		Model: "deepseek-chat",
		Choices: []OpenAIChoice{
			{
				Index: 0,
				Message: OpenAIMessage{
					Role:    "assistant",
					Content: mustJSON("here you go"),
				},
				FinishReason: "stop",
			},
		},
		Usage: OpenAIUsage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17},
	}
	out, err := OpenAIToAnthropic(resp, "claude-3-5")
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != "chatcmpl-abc" {
		t.Errorf("id = %q", out.ID)
	}
	if out.Type != "message" || out.Role != "assistant" {
		t.Errorf("type/role = %q/%q", out.Type, out.Role)
	}
	if out.Model != "claude-3-5" {
		t.Errorf("model = %q (should echo original Anthropic model)", out.Model)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "text" || out.Content[0].Text != "here you go" {
		t.Errorf("content = %+v", out.Content)
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", out.StopReason)
	}
	if out.Usage.InputTokens != 12 || out.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

func TestOpenAIToAnthropic_EmptyContent(t *testing.T) {
	resp := &OpenAIResponse{
		Choices: []OpenAIChoice{
			{Message: OpenAIMessage{Role: "assistant"}, FinishReason: "stop"},
		},
	}
	out, err := OpenAIToAnthropic(resp, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "text" || out.Content[0].Text != "" {
		t.Errorf("empty content should emit one empty text block, got %+v", out.Content)
	}
}

func TestOpenAIToAnthropic_AssistantArrayContent(t *testing.T) {
	resp := &OpenAIResponse{
		Choices: []OpenAIChoice{
			{
				Message: OpenAIMessage{
					Role: "assistant",
					Content: mustJSON([]OpenAIContentPart{
						{Type: "text", Text: "first "},
						{Type: "text", Text: "second"},
					}),
				},
				FinishReason: "stop",
			},
		},
	}
	out, err := OpenAIToAnthropic(resp, "x")
	if err != nil {
		t.Fatal(err)
	}
	if out.Content[0].Text != "first second" {
		t.Errorf("text concat = %q", out.Content[0].Text)
	}
}

func TestOpenAIToAnthropic_NoChoices(t *testing.T) {
	if _, err := OpenAIToAnthropic(&OpenAIResponse{}, "x"); err == nil {
		t.Fatal("expected error for empty choices")
	}
}

func TestOpenAIToAnthropic_NilResponse(t *testing.T) {
	if _, err := OpenAIToAnthropic(nil, "x"); err == nil {
		t.Fatal("expected error for nil response")
	}
}

// --- finish_reason matrix ---

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"content_filter", "stop_sequence"},
		{"", "end_turn"},             // default
		{"unrecognised", "end_turn"}, // default
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := mapFinishReason(tc.in); got != tc.want {
				t.Errorf("mapFinishReason(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFinishReasonMatrixMutationSurvival is the one-shot mutation check
// Linus signed off on. Conceptually: swap any value in finishReasonMap and
// at least one assertion above must fail. We approximate this here by
// proving the map is exactly the expected matrix — any addition, removal,
// or value swap will break this test.
func TestFinishReasonMatrixMutationSurvival(t *testing.T) {
	want := map[string]string{
		"stop":           "end_turn",
		"length":         "max_tokens",
		"tool_calls":     "tool_use",
		"content_filter": "stop_sequence",
	}
	if !reflect.DeepEqual(finishReasonMap, want) {
		t.Errorf("finishReasonMap drift:\n got: %#v\nwant: %#v", finishReasonMap, want)
	}
	if defaultStopReason != "end_turn" {
		t.Errorf("defaultStopReason = %q, want end_turn", defaultStopReason)
	}
}

// --- usage mapping ---

func TestUsageMapping(t *testing.T) {
	resp := &OpenAIResponse{
		Choices: []OpenAIChoice{{Message: OpenAIMessage{Role: "assistant"}, FinishReason: "stop"}},
		Usage:   OpenAIUsage{PromptTokens: 1000, CompletionTokens: 50, TotalTokens: 1050},
	}
	out, _ := OpenAIToAnthropic(resp, "x")
	if out.Usage.InputTokens != 1000 || out.Usage.OutputTokens != 50 {
		t.Errorf("usage mapping wrong: %+v", out.Usage)
	}
}

// --- internal helper coverage ---

func TestTrimJSON(t *testing.T) {
	tests := map[string]struct {
		in   string
		want string
	}{
		"no whitespace":      {`"hi"`, `"hi"`},
		"leading":            {`   "hi"`, `"hi"`},
		"trailing":           {`"hi"   `, `"hi"`},
		"both ends":          {" \n\"hi\"\t ", `"hi"`},
		"pure whitespace":    {" \n\t ", ``},
		"empty":              {``, ``},
		"internal preserved": {`"a b"`, `"a b"`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := string(trimJSON(json.RawMessage(tc.in)))
			if got != tc.want {
				t.Errorf("trimJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFlattenAssistantContent_MixedParts(t *testing.T) {
	// Upstreams shouldn't return image_url in assistant content for Stage 0,
	// but if they do we keep the text parts and drop the rest rather than
	// erroring — the alternative would silently corrupt a working response.
	raw := json.RawMessage(`[{"type":"text","text":"hello "},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"world"}]`)
	got, err := flattenAssistantContent(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestFlattenAssistantContent_UnrecognisedShape(t *testing.T) {
	raw := json.RawMessage(`{"unexpected":"object"}`)
	if _, err := flattenAssistantContent(raw); err == nil {
		t.Error("expected error for object shape, got nil")
	}
}
