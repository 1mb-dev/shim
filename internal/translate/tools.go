package translate

import (
	"encoding/json"
	"fmt"
)

// Tools-related translation: the Anthropic tools[]/tool_choice → OpenAI
// mapping and back.

// wireTools attaches the OpenAI-shaped tools[] and tool_choice fields from
// an Anthropic request.
func wireTools(in *AnthropicRequest, out *OpenAIRequest) error {
	if len(in.Tools) > 0 {
		out.Tools = make([]OpenAITool, 0, len(in.Tools))
		for i, t := range in.Tools {
			if t.Name == "" {
				return fmt.Errorf("tools[%d]: missing name", i)
			}
			out.Tools = append(out.Tools, OpenAITool{
				Type: "function",
				Function: OpenAIToolFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.InputSchema,
				},
			})
		}
	}
	if len(in.ToolChoice) > 0 {
		mapped, err := mapToolChoice(in.ToolChoice)
		if err != nil {
			return fmt.Errorf("tool_choice: %w", err)
		}
		out.ToolChoice = mapped
	}
	return nil
}

// mapToolChoice passes through valid {"type":"auto|any|none"} or
// {"type":"tool","name":"..."} as the OpenAI-shaped tool_choice.
func mapToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	var probe struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("malformed: %w", err)
	}
	switch probe.Type {
	case "auto":
		return json.RawMessage(`"auto"`), nil
	case "any":
		return json.RawMessage(`"required"`), nil
	case "none":
		return json.RawMessage(`"none"`), nil
	case "tool":
		if probe.Name == "" {
			return nil, fmt.Errorf("tool choice requires name")
		}
		return json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": probe.Name},
		})
	default:
		return nil, fmt.Errorf("unknown tool_choice type %q", probe.Type)
	}
}

// toolUseToOpenAI converts an Anthropic tool_use block to one OpenAI tool
// call entry on an assistant message.
func toolUseToOpenAI(b AnthropicBlock) (OpenAIToolCall, error) {
	if b.ID == "" || b.Name == "" {
		return OpenAIToolCall{}, fmt.Errorf("tool_use requires id and name")
	}
	args := string(b.Input)
	if args == "" {
		args = "{}"
	}
	return OpenAIToolCall{
		ID:   b.ID,
		Type: "function",
		Function: OpenAIToolCallBody{
			Name:      b.Name,
			Arguments: args,
		},
	}, nil
}

// toolResultToOpenAI converts an Anthropic tool_result block (which lives
// inside a user message) into a standalone OpenAI role:"tool" message.
func toolResultToOpenAI(b AnthropicBlock) (OpenAIMessage, error) {
	if b.ToolUseID == "" {
		return OpenAIMessage{}, fmt.Errorf("tool_result requires tool_use_id")
	}

	// tool_result content can be a string or an array of text/image blocks.
	// OpenAI tool messages expect a string body — flatten arrays by
	// concatenating text and serialising images as a placeholder.
	body, err := flattenToolResultContent(b.ToolContent)
	if err != nil {
		return OpenAIMessage{}, err
	}
	content, _ := json.Marshal(body)

	return OpenAIMessage{
		Role:       "tool",
		Content:    content,
		ToolCallID: b.ToolUseID,
	}, nil
}

func flattenToolResultContent(raw json.RawMessage) (string, error) {
	raw = trimJSON(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	if raw[0] == '[' {
		var blocks []AnthropicBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return "", err
		}
		var sb []byte
		for _, blk := range blocks {
			switch blk.Type {
			case "text":
				sb = append(sb, blk.Text...)
			case "image":
				sb = append(sb, "[image elided in tool_result]"...)
			default:
				return "", fmt.Errorf("tool_result block type %q not allowed", blk.Type)
			}
		}
		return string(sb), nil
	}
	return "", fmt.Errorf("tool_result content must be string or array")
}

// toolCallToAnthropic converts one OpenAI tool call back to an Anthropic
// tool_use block (response side).
func toolCallToAnthropic(tc OpenAIToolCall) (AnthropicBlock, error) {
	if tc.ID == "" || tc.Function.Name == "" {
		return AnthropicBlock{}, fmt.Errorf("tool_call requires id and function.name")
	}
	args := tc.Function.Arguments
	if args == "" {
		args = "{}"
	}
	// Validate the JSON-ness of arguments — OpenAI sends a JSON-encoded
	// string, Anthropic expects an object.
	var probe any
	if err := json.Unmarshal([]byte(args), &probe); err != nil {
		return AnthropicBlock{}, fmt.Errorf("tool_call arguments not valid JSON: %w", err)
	}
	return AnthropicBlock{
		Type:  "tool_use",
		ID:    tc.ID,
		Name:  tc.Function.Name,
		Input: json.RawMessage(args),
	}, nil
}
