// Package translate converts Anthropic Messages API requests/responses to
// and from OpenAI ChatCompletions shape. Pure: no I/O, no upstream calls.
// Adapter-specific quirks live in internal/adapter/<provider>.
package translate

import "encoding/json"

// --- Anthropic Messages API shapes ---

// AnthropicRequest mirrors the documented Anthropic Messages request body.
// Fields irrelevant to Stage 0 are intentionally omitted (e.g. metadata,
// service_tier) — they round-trip via json.Unmarshal's ignored-field path.
type AnthropicRequest struct {
	Model         string                   `json:"model"`
	Messages      []AnthropicMessage       `json:"messages"`
	System        json.RawMessage          `json:"system,omitempty"` // string | content[]
	MaxTokens     int                      `json:"max_tokens"`
	Stream        bool                     `json:"stream,omitempty"`
	Temperature   *float64                 `json:"temperature,omitempty"`
	TopP          *float64                 `json:"top_p,omitempty"`
	StopSequences []string                 `json:"stop_sequences,omitempty"`
	Tools         []AnthropicTool          `json:"tools,omitempty"`
	ToolChoice    json.RawMessage          `json:"tool_choice,omitempty"`
	Thinking      *AnthropicThinkingConfig `json:"thinking,omitempty"`
}

// AnthropicThinkingConfig is the extended-thinking request param. Stage
// 2.6b only inspects Type ("enabled"|"disabled") — budget_tokens, display,
// and other fields get json-unmarshal-silently-dropped. Stage 2.6c adds
// them when there's a code path that uses them.
type AnthropicThinkingConfig struct {
	Type string `json:"type"`
}

// AnthropicMessage holds a role and content. Content is either a string or
// an array of typed blocks — we keep RawMessage for the boundary and parse
// in helpers.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// AnthropicBlock is a tagged union of content block types: text, image,
// tool_use, tool_result, and thinking (Stage 0 rejects thinking loudly).
type AnthropicBlock struct {
	Type string `json:"type"`

	// type: "text"
	Text string `json:"text,omitempty"`

	// type: "image"
	Source *AnthropicImageSource `json:"source,omitempty"`

	// type: "tool_use"
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type: "tool_result"
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Content on tool_result is either a string or a content[] of text/image
	// blocks. We keep RawMessage for the boundary.
	ToolContent json.RawMessage `json:"content,omitempty"`
	IsError     bool            `json:"is_error,omitempty"`
}

// AnthropicImageSource carries either base64 data with a media_type or a
// URL. Anthropic supports both; OpenAI takes a single URL form (data: for
// base64).
type AnthropicImageSource struct {
	Type      string `json:"type"`       // "base64" | "url"
	MediaType string `json:"media_type"` // e.g. "image/png"
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// AnthropicTool is the tool declaration shape on the request side. Schema
// stays a RawMessage so we don't validate JSON Schema here.
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// AnthropicResponse is what we send back to Claude Code.
type AnthropicResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"` // always "message"
	Role         string           `json:"role"` // always "assistant"
	Model        string           `json:"model"`
	Content      []AnthropicBlock `json:"content"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence,omitempty"`
	Usage        AnthropicUsage   `json:"usage"`
}

// AnthropicUsage mirrors Anthropic's usage shape.
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// --- OpenAI ChatCompletions shapes ---

// OpenAIRequest is the body we send to {adapter.BaseURL}/chat/completions.
type OpenAIRequest struct {
	Model       string                  `json:"model"`
	Messages    []OpenAIMessage         `json:"messages"`
	MaxTokens   int                     `json:"max_tokens,omitempty"`
	Temperature *float64                `json:"temperature,omitempty"`
	TopP        *float64                `json:"top_p,omitempty"`
	Stop        []string                `json:"stop,omitempty"`
	Tools       []OpenAITool            `json:"tools,omitempty"`
	ToolChoice  json.RawMessage         `json:"tool_choice,omitempty"`
	Stream      bool                    `json:"stream,omitempty"`
	Thinking    *DeepSeekThinkingConfig `json:"thinking,omitempty"`
}

// DeepSeekThinkingConfig is the thinking-mode control param on DeepSeek's
// OpenAI-format endpoint. Stage 2.6b only ever emits {Type: "disabled"} —
// reasoning_effort and other fields land in 2.6c when there's a code path
// that uses them.
type DeepSeekThinkingConfig struct {
	Type string `json:"type"`
}

// OpenAIMessage covers system, user, assistant, and tool roles. Content is
// string for simple cases or an array of parts when mixed text+image.
type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// OpenAIContentPart represents a single chunk within a multi-modal user
// message: text or image_url.
type OpenAIContentPart struct {
	Type     string          `json:"type"` // "text" | "image_url"
	Text     string          `json:"text,omitempty"`
	ImageURL *OpenAIImageURL `json:"image_url,omitempty"`
}

// OpenAIImageURL accepts http(s) URLs or data: URLs for inline base64.
type OpenAIImageURL struct {
	URL string `json:"url"`
}

// OpenAITool is the function-wrapped tool declaration.
type OpenAITool struct {
	Type     string             `json:"type"` // always "function"
	Function OpenAIToolFunction `json:"function"`
}

// OpenAIToolFunction inner declaration.
type OpenAIToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// OpenAIToolCall is one tool invocation on the assistant side.
type OpenAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"` // always "function"
	Function OpenAIToolCallBody `json:"function"`
}

// OpenAIToolCallBody is the name + serialised JSON arguments.
type OpenAIToolCallBody struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string per spec
}

// OpenAIResponse is the upstream response we receive.
type OpenAIResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

// OpenAIChoice is one completion option. We only consume index 0.
type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

// OpenAIUsage carries prompt/completion token counts.
type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
