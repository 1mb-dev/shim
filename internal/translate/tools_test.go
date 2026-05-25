package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- wireTools (tools[]) ---

func TestWireTools_Empty(t *testing.T) {
	in := &AnthropicRequest{}
	out := &OpenAIRequest{}
	if err := wireTools(in, out); err != nil {
		t.Fatal(err)
	}
	if out.Tools != nil {
		t.Errorf("expected nil tools, got %v", out.Tools)
	}
}

func TestWireTools_Translation(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	in := &AnthropicRequest{
		Tools: []AnthropicTool{
			{Name: "search", Description: "search the web", InputSchema: schema},
			{Name: "weather", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	out := &OpenAIRequest{}
	if err := wireTools(in, out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("tools len = %d", len(out.Tools))
	}
	if out.Tools[0].Type != "function" || out.Tools[0].Function.Name != "search" {
		t.Errorf("tools[0] = %+v", out.Tools[0])
	}
	if out.Tools[0].Function.Description != "search the web" {
		t.Errorf("tools[0] description lost")
	}
	if string(out.Tools[0].Function.Parameters) != string(schema) {
		t.Errorf("tools[0] schema = %s", out.Tools[0].Function.Parameters)
	}
	if out.Tools[1].Function.Description != "" {
		t.Errorf("tools[1] description should be empty")
	}
}

func TestWireTools_MissingName(t *testing.T) {
	in := &AnthropicRequest{
		Tools: []AnthropicTool{
			{Name: "", InputSchema: json.RawMessage(`{}`)},
		},
	}
	if err := wireTools(in, &OpenAIRequest{}); err == nil {
		t.Fatal("expected error for missing tool name")
	}
}

// --- tool_choice matrix ---

func TestMapToolChoice(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"auto", `{"type":"auto"}`, `"auto"`},
		{"any", `{"type":"any"}`, `"required"`},
		{"none", `{"type":"none"}`, `"none"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mapToolChoice([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestMapToolChoice_ToolByName(t *testing.T) {
	got, err := mapToolChoice([]byte(`{"type":"tool","name":"search"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Type     string            `json:"type"`
		Function map[string]string `json:"function"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != "function" {
		t.Errorf("type = %q", decoded.Type)
	}
	if decoded.Function["name"] != "search" {
		t.Errorf("function.name = %q", decoded.Function["name"])
	}
}

func TestMapToolChoice_ToolMissingName(t *testing.T) {
	if _, err := mapToolChoice([]byte(`{"type":"tool"}`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestMapToolChoice_UnknownType(t *testing.T) {
	if _, err := mapToolChoice([]byte(`{"type":"wizard"}`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestMapToolChoice_Malformed(t *testing.T) {
	if _, err := mapToolChoice([]byte(`not-json`)); err == nil {
		t.Fatal("expected error")
	}
}

// TestToolChoiceMatrixMutationSurvival pins the OpenAI mapping for each
// Anthropic tool_choice variant. Swap any RHS and a test above will fail.
func TestToolChoiceMatrixMutationSurvival(t *testing.T) {
	// Map intent: (anthropic type) -> (openai serialised form)
	want := map[string]string{
		"auto": `"auto"`,
		"any":  `"required"`,
		"none": `"none"`,
	}
	for in, expected := range want {
		got, err := mapToolChoice([]byte(`{"type":"` + in + `"}`))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if string(got) != expected {
			t.Errorf("matrix drift: %s → %s, want %s", in, got, expected)
		}
	}
	// Tool-by-name is asserted separately because its serialised shape is
	// a JSON object, not a primitive.
}

// --- toolUseToOpenAI ---

func TestToolUseToOpenAI_Happy(t *testing.T) {
	b := AnthropicBlock{
		Type:  "tool_use",
		ID:    "toolu_01ABC",
		Name:  "search",
		Input: json.RawMessage(`{"q":"hello"}`),
	}
	tc, err := toolUseToOpenAI(b)
	if err != nil {
		t.Fatal(err)
	}
	if tc.ID != "toolu_01ABC" {
		t.Errorf("id = %q", tc.ID)
	}
	if tc.Type != "function" {
		t.Errorf("type = %q", tc.Type)
	}
	if tc.Function.Name != "search" {
		t.Errorf("function.name = %q", tc.Function.Name)
	}
	if tc.Function.Arguments != `{"q":"hello"}` {
		t.Errorf("function.arguments = %q", tc.Function.Arguments)
	}
}

func TestToolUseToOpenAI_EmptyInputBecomesObject(t *testing.T) {
	b := AnthropicBlock{Type: "tool_use", ID: "x", Name: "y"}
	tc, err := toolUseToOpenAI(b)
	if err != nil {
		t.Fatal(err)
	}
	if tc.Function.Arguments != "{}" {
		t.Errorf("empty input should serialise as {}, got %q", tc.Function.Arguments)
	}
}

func TestToolUseToOpenAI_MissingIDOrName(t *testing.T) {
	for _, b := range []AnthropicBlock{
		{Type: "tool_use", Name: "x"},
		{Type: "tool_use", ID: "x"},
	} {
		if _, err := toolUseToOpenAI(b); err == nil {
			t.Errorf("expected error for %+v", b)
		}
	}
}

// --- toolResultToOpenAI ---

func TestToolResultToOpenAI_StringContent(t *testing.T) {
	b := AnthropicBlock{
		Type:        "tool_result",
		ToolUseID:   "toolu_01",
		ToolContent: json.RawMessage(`"the answer is 42"`),
	}
	m, err := toolResultToOpenAI(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.Role != "tool" {
		t.Errorf("role = %q", m.Role)
	}
	if m.ToolCallID != "toolu_01" {
		t.Errorf("tool_call_id = %q", m.ToolCallID)
	}
	var content string
	_ = json.Unmarshal(m.Content, &content)
	if content != "the answer is 42" {
		t.Errorf("content = %q", content)
	}
}

func TestToolResultToOpenAI_ArrayContent(t *testing.T) {
	b := AnthropicBlock{
		Type:      "tool_result",
		ToolUseID: "toolu_01",
		ToolContent: mustJSON([]AnthropicBlock{
			{Type: "text", Text: "part1 "},
			{Type: "text", Text: "part2"},
		}),
	}
	m, err := toolResultToOpenAI(b)
	if err != nil {
		t.Fatal(err)
	}
	var content string
	_ = json.Unmarshal(m.Content, &content)
	if content != "part1 part2" {
		t.Errorf("content = %q", content)
	}
}

func TestToolResultToOpenAI_ArrayWithImagePlaceholder(t *testing.T) {
	b := AnthropicBlock{
		Type:      "tool_result",
		ToolUseID: "toolu_01",
		ToolContent: mustJSON([]AnthropicBlock{
			{Type: "text", Text: "see: "},
			{Type: "image", Source: &AnthropicImageSource{Type: "url", URL: "x"}},
		}),
	}
	m, err := toolResultToOpenAI(b)
	if err != nil {
		t.Fatal(err)
	}
	var content string
	_ = json.Unmarshal(m.Content, &content)
	if !strings.Contains(content, "image elided") {
		t.Errorf("expected image placeholder, got %q", content)
	}
}

func TestToolResultToOpenAI_DisallowedBlockType(t *testing.T) {
	b := AnthropicBlock{
		Type:      "tool_result",
		ToolUseID: "toolu_01",
		ToolContent: mustJSON([]AnthropicBlock{
			{Type: "tool_use", ID: "x", Name: "y"},
		}),
	}
	if _, err := toolResultToOpenAI(b); err == nil {
		t.Fatal("expected error for tool_use inside tool_result")
	}
}

func TestToolResultToOpenAI_MissingID(t *testing.T) {
	b := AnthropicBlock{Type: "tool_result", ToolContent: json.RawMessage(`"x"`)}
	if _, err := toolResultToOpenAI(b); err == nil {
		t.Fatal("expected error")
	}
}

func TestToolResultToOpenAI_EmptyContent(t *testing.T) {
	b := AnthropicBlock{Type: "tool_result", ToolUseID: "toolu_01"}
	m, err := toolResultToOpenAI(b)
	if err != nil {
		t.Fatal(err)
	}
	var content string
	_ = json.Unmarshal(m.Content, &content)
	if content != "" {
		t.Errorf("empty content should yield empty string, got %q", content)
	}
}

// --- toolCallToAnthropic ---

func TestToolCallToAnthropic_Happy(t *testing.T) {
	tc := OpenAIToolCall{
		ID:   "call_01",
		Type: "function",
		Function: OpenAIToolCallBody{
			Name:      "search",
			Arguments: `{"q":"hello"}`,
		},
	}
	b, err := toolCallToAnthropic(tc)
	if err != nil {
		t.Fatal(err)
	}
	if b.Type != "tool_use" {
		t.Errorf("type = %q", b.Type)
	}
	if b.ID != "call_01" || b.Name != "search" {
		t.Errorf("id/name = %q/%q", b.ID, b.Name)
	}
	if string(b.Input) != `{"q":"hello"}` {
		t.Errorf("input = %s", b.Input)
	}
}

func TestToolCallToAnthropic_EmptyArgs(t *testing.T) {
	tc := OpenAIToolCall{
		ID: "x", Function: OpenAIToolCallBody{Name: "y"},
	}
	b, err := toolCallToAnthropic(tc)
	if err != nil {
		t.Fatal(err)
	}
	if string(b.Input) != "{}" {
		t.Errorf("empty args should yield {}, got %s", b.Input)
	}
}

func TestToolCallToAnthropic_MissingIDOrName(t *testing.T) {
	for _, tc := range []OpenAIToolCall{
		{ID: "x"},
		{Function: OpenAIToolCallBody{Name: "y"}},
	} {
		if _, err := toolCallToAnthropic(tc); err == nil {
			t.Errorf("expected error for %+v", tc)
		}
	}
}

func TestToolCallToAnthropic_InvalidArgsJSON(t *testing.T) {
	tc := OpenAIToolCall{
		ID:       "x",
		Function: OpenAIToolCallBody{Name: "y", Arguments: "not-json"},
	}
	if _, err := toolCallToAnthropic(tc); err == nil {
		t.Fatal("expected error for invalid args JSON")
	}
}

// --- Roundtrip: tool_use ↔ openai tool_call ↔ tool_use ---

func TestRoundtrip_ToolUse(t *testing.T) {
	original := AnthropicBlock{
		Type:  "tool_use",
		ID:    "toolu_RT",
		Name:  "calc",
		Input: json.RawMessage(`{"op":"add","a":1,"b":2}`),
	}
	tc, err := toolUseToOpenAI(original)
	if err != nil {
		t.Fatal(err)
	}
	back, err := toolCallToAnthropic(tc)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != original.ID || back.Name != original.Name {
		t.Errorf("id/name lost: %q/%q", back.ID, back.Name)
	}
	// Compare as JSON to avoid byte-level whitespace differences.
	var origIn, backIn any
	_ = json.Unmarshal(original.Input, &origIn)
	_ = json.Unmarshal(back.Input, &backIn)
	if !jsonEqual(t, origIn, backIn) {
		t.Errorf("input drift:\n original: %s\n back:     %s", original.Input, back.Input)
	}
}

// --- Roundtrip via full request/response: user tool_result -> openai tool message ---

func TestRoundtrip_UserToolResult(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "user", Content: mustJSON([]AnthropicBlock{
				{Type: "tool_result", ToolUseID: "toolu_RT", ToolContent: mustJSON("result body")},
			})},
		},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("expected 1 OpenAI message, got %d", len(out.Messages))
	}
	if out.Messages[0].Role != "tool" {
		t.Errorf("role = %q, want tool", out.Messages[0].Role)
	}
	if out.Messages[0].ToolCallID != "toolu_RT" {
		t.Errorf("tool_call_id = %q", out.Messages[0].ToolCallID)
	}
}

// --- End-to-end response: assistant w/ tool_call returns tool_use block ---

func TestOpenAIToAnthropic_ToolCallResponse(t *testing.T) {
	resp := &OpenAIResponse{
		Choices: []OpenAIChoice{{
			Message: OpenAIMessage{
				Role: "assistant",
				ToolCalls: []OpenAIToolCall{{
					ID: "call_01", Type: "function",
					Function: OpenAIToolCallBody{Name: "search", Arguments: `{"q":"hello"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
	out, err := OpenAIToAnthropic(resp, "claude-3-5")
	if err != nil {
		t.Fatal(err)
	}
	if out.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", out.StopReason)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "tool_use" {
		t.Fatalf("content = %+v", out.Content)
	}
	if out.Content[0].ID != "call_01" || out.Content[0].Name != "search" {
		t.Errorf("tool_use id/name = %q/%q", out.Content[0].ID, out.Content[0].Name)
	}
}

// --- Request-side: assistant message carrying a tool_use block ---

func TestAnthropicToOpenAI_AssistantToolUseBlock(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: mustJSON([]AnthropicBlock{
				{Type: "text", Text: "let me check"},
				{Type: "tool_use", ID: "toolu_01", Name: "search", Input: mustJSON(map[string]string{"q": "hello"})},
			})},
		},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages len = %d", len(out.Messages))
	}
	m := out.Messages[0]
	if m.Role != "assistant" {
		t.Errorf("role = %q", m.Role)
	}
	if len(m.ToolCalls) != 1 {
		t.Fatalf("tool_calls len = %d", len(m.ToolCalls))
	}
	if m.ToolCalls[0].ID != "toolu_01" || m.ToolCalls[0].Function.Name != "search" {
		t.Errorf("tool_call = %+v", m.ToolCalls[0])
	}
}

// --- Edge cases for coverage on the real branches ---

func TestAnthropicToOpenAI_NullSystem(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		System:   json.RawMessage(`null`),
		Messages: []AnthropicMessage{{Role: "user", Content: mustJSON("hi")}},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Errorf("null system should produce no system message, got %d total", len(out.Messages))
	}
}

func TestAnthropicToOpenAI_NullMessageContent(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`null`)},
		},
	}
	out, err := AnthropicToOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(out.Messages))
	}
}

func TestAnthropicToOpenAI_NonStringNonArrayContent(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`42`)},
		},
	}
	if _, err := AnthropicToOpenAI(req); err == nil {
		t.Fatal("expected error for numeric content")
	}
}

func TestAnthropicToOpenAI_UnknownUserBlockType(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "user", Content: mustJSON([]AnthropicBlock{
				{Type: "wizard", Text: "x"},
			})},
		},
	}
	if _, err := AnthropicToOpenAI(req); err == nil {
		t.Fatal("expected error for unknown block type")
	}
}

func TestAnthropicToOpenAI_UnknownAssistantBlockType(t *testing.T) {
	req := &AnthropicRequest{
		Model: "x", MaxTokens: 1,
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: mustJSON([]AnthropicBlock{
				{Type: "wizard"},
			})},
		},
	}
	if _, err := AnthropicToOpenAI(req); err == nil {
		t.Fatal("expected error for unknown assistant block type")
	}
}

// --- helpers ---

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
