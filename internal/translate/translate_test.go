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

func TestAnthropicToOpenAI_ThinkingRejected(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: mustJSON([]AnthropicBlock{
				{Type: "thinking", Text: "..."},
			})},
		},
	}
	_, err := AnthropicToOpenAI(req)
	if err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("expected thinking-not-supported error, got %v", err)
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
