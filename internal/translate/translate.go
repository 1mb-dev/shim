package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// finishReasonMap is the OpenAI finish_reason → Anthropic stop_reason
// mapping locked in the handoff. Mutation-survival test (see
// translate_test.go) asserts every value is load-bearing.
var finishReasonMap = map[string]string{
	"stop":           "end_turn",
	"length":         "max_tokens",
	"tool_calls":     "tool_use",
	"content_filter": "stop_sequence",
}

// defaultStopReason is returned for OpenAI finish_reason values we don't
// recognise. Stage 0 [ASSUMPTION]: silent default to end_turn rather than
// erroring; documented in todos/shim-stage0-notes.md.
const defaultStopReason = "end_turn"

// AnthropicToOpenAI converts an Anthropic Messages request to an OpenAI
// ChatCompletions request. Tools-related fields are delegated to tools.go
// (step 5) but the wiring lives here.
//
// Model name mapping is delegated to the Adapter — translate stays pure.
func AnthropicToOpenAI(req *AnthropicRequest) (*OpenAIRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}

	out := &OpenAIRequest{
		Model:       req.Model, // Adapter.MapModel will replace this downstream.
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.StopSequences,
	}

	// System prompt -> single role:"system" message at index 0.
	if len(req.System) > 0 {
		sys, err := flattenSystem(req.System)
		if err != nil {
			return nil, fmt.Errorf("system: %w", err)
		}
		if sys != "" {
			content, _ := json.Marshal(sys)
			out.Messages = append(out.Messages, OpenAIMessage{
				Role:    "system",
				Content: content,
			})
		}
	}

	// Translate each message. Tool-result blocks become separate
	// role:"tool" messages (one per tool_result).
	for i, m := range req.Messages {
		oms, err := translateMessage(m)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Messages = append(out.Messages, oms...)
	}

	// Tools wiring (impl in tools.go).
	if err := wireTools(req, out); err != nil {
		return nil, err
	}

	// Thinking control plane (Stage 2.6c). Pass req.Thinking through
	// identity — when client omits, no thinking field on outbound (DeepSeek
	// ignores thinking=disabled on v4-pro anyway, so the 2.6b inject was
	// dead weight). When client sends thinking, forward it; reasoning
	// content roundtrips via OpenAIMessage.ReasoningContent ↔ Anthropic
	// thinking blocks (see assistantBlocksToMessages + messageToBlocks).
	if req.Thinking != nil {
		out.Thinking = &DeepSeekThinkingConfig{Type: req.Thinking.Type}
	}

	return out, nil
}

// OpenAIToAnthropic converts an OpenAI ChatCompletions response back to
// Anthropic Messages shape. Only choice[0] is consumed (Stage 0; n=1).
//
// originalModel is the model name the client sent (passed through into the
// response so Claude Code sees what it asked for).
func OpenAIToAnthropic(resp *OpenAIResponse, originalModel string) (*AnthropicResponse, error) {
	if resp == nil {
		return nil, fmt.Errorf("nil response")
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("response has no choices")
	}

	choice := resp.Choices[0]
	blocks, err := messageToBlocks(choice.Message)
	if err != nil {
		return nil, fmt.Errorf("choices[0].message: %w", err)
	}

	out := &AnthropicResponse{
		ID:         resp.ID,
		Type:       "message",
		Role:       "assistant",
		Model:      originalModel, // surface what the client asked for
		Content:    blocks,
		StopReason: mapFinishReason(choice.FinishReason),
		Usage: AnthropicUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}
	return out, nil
}

// mapFinishReason looks up the OpenAI finish_reason in the locked matrix,
// falling back to defaultStopReason for unknown values.
func mapFinishReason(r string) string {
	if v, ok := finishReasonMap[r]; ok {
		return v
	}
	return defaultStopReason
}

// --- helpers ---

// flattenSystem accepts either a JSON string ("be helpful") or a JSON array
// of text content blocks and returns the concatenated text body. Non-text
// blocks in a system array are an error.
func flattenSystem(raw json.RawMessage) (string, error) {
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
		var parts []string
		for i, b := range blocks {
			if b.Type != "text" {
				return "", fmt.Errorf("system[%d]: only text blocks allowed, got %q", i, b.Type)
			}
			parts = append(parts, b.Text)
		}
		return strings.Join(parts, "\n"), nil
	}
	return "", fmt.Errorf("unrecognised system shape (must be string or array)")
}

// translateMessage converts one Anthropic message into one or more OpenAI
// messages. Tool-result blocks within a user message become separate
// role:"tool" messages (one per block) per OpenAI's contract.
func translateMessage(m AnthropicMessage) ([]OpenAIMessage, error) {
	if m.Role != "user" && m.Role != "assistant" {
		return nil, fmt.Errorf("unsupported role %q", m.Role)
	}

	// Content can be a string or an array of blocks.
	content := trimJSON(m.Content)
	if len(content) == 0 || string(content) == "null" {
		// Empty content — emit an empty assistant/user message.
		empty, _ := json.Marshal("")
		return []OpenAIMessage{{Role: m.Role, Content: empty}}, nil
	}

	if content[0] == '"' {
		// Simple string content.
		return []OpenAIMessage{{Role: m.Role, Content: content}}, nil
	}

	if content[0] != '[' {
		return nil, fmt.Errorf("content must be string or array")
	}

	var blocks []AnthropicBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, fmt.Errorf("content blocks: %w", err)
	}

	return blocksToOpenAIMessages(m.Role, blocks)
}

// blocksToOpenAIMessages handles the role-specific block layout:
//
//   - user role: text/image blocks go into ONE message (mixed parts).
//     tool_result blocks become SEPARATE role:"tool" messages.
//   - assistant role: text blocks go into ONE message (text content).
//     tool_use blocks become entries in that message's tool_calls.
//   - thinking blocks: rejected (caller handles 501 — server layer).
func blocksToOpenAIMessages(role string, blocks []AnthropicBlock) ([]OpenAIMessage, error) {
	switch role {
	case "user":
		return userBlocksToMessages(blocks)
	case "assistant":
		return assistantBlocksToMessages(blocks)
	default:
		return nil, fmt.Errorf("unreachable: role %q", role)
	}
}

func userBlocksToMessages(blocks []AnthropicBlock) ([]OpenAIMessage, error) {
	var out []OpenAIMessage
	var parts []OpenAIContentPart

	flushParts := func() error {
		if len(parts) == 0 {
			return nil
		}
		raw, err := encodeContent(parts)
		if err != nil {
			return err
		}
		out = append(out, OpenAIMessage{Role: "user", Content: raw})
		parts = nil
		return nil
	}

	for i, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, OpenAIContentPart{Type: "text", Text: b.Text})
		case "image":
			part, err := imageBlockToPart(b)
			if err != nil {
				return nil, fmt.Errorf("blocks[%d]: %w", i, err)
			}
			parts = append(parts, part)
		case "tool_result":
			if err := flushParts(); err != nil {
				return nil, err
			}
			toolMsg, err := toolResultToOpenAI(b)
			if err != nil {
				return nil, fmt.Errorf("blocks[%d]: %w", i, err)
			}
			out = append(out, toolMsg)
		case "thinking":
			return nil, fmt.Errorf("blocks[%d]: thinking blocks not supported", i)
		default:
			return nil, fmt.Errorf("blocks[%d]: unknown type %q", i, b.Type)
		}
	}
	if err := flushParts(); err != nil {
		return nil, err
	}
	return out, nil
}

func assistantBlocksToMessages(blocks []AnthropicBlock) ([]OpenAIMessage, error) {
	msg := OpenAIMessage{Role: "assistant"}
	var textParts []string
	var thinkingParts []string

	for i, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			tc, err := toolUseToOpenAI(b)
			if err != nil {
				return nil, fmt.Errorf("blocks[%d]: %w", i, err)
			}
			msg.ToolCalls = append(msg.ToolCalls, tc)
		case "thinking":
			// Stage 2.6c: signature is discarded — DeepSeek doesn't accept
			// it and shim doesn't verify on roundtrip (constant string;
			// loopback threat model). Multiple thinking blocks concatenate
			// per the Anthropic spec; rare but the contract allows it.
			thinkingParts = append(thinkingParts, b.Thinking)
		default:
			return nil, fmt.Errorf("blocks[%d]: unknown assistant type %q", i, b.Type)
		}
	}

	if len(textParts) > 0 {
		raw, _ := json.Marshal(strings.Join(textParts, "\n"))
		msg.Content = raw
	}
	if len(thinkingParts) > 0 {
		msg.ReasoningContent = strings.Join(thinkingParts, "\n")
	}
	return []OpenAIMessage{msg}, nil
}

// imageBlockToPart converts an Anthropic image block to an OpenAI content
// part. Base64 sources become data: URLs; url sources pass through.
func imageBlockToPart(b AnthropicBlock) (OpenAIContentPart, error) {
	if b.Source == nil {
		return OpenAIContentPart{}, fmt.Errorf("image block missing source")
	}
	switch b.Source.Type {
	case "base64":
		if b.Source.MediaType == "" || b.Source.Data == "" {
			return OpenAIContentPart{}, fmt.Errorf("base64 image requires media_type and data")
		}
		url := fmt.Sprintf("data:%s;base64,%s", b.Source.MediaType, b.Source.Data)
		return OpenAIContentPart{Type: "image_url", ImageURL: &OpenAIImageURL{URL: url}}, nil
	case "url":
		if b.Source.URL == "" {
			return OpenAIContentPart{}, fmt.Errorf("url image requires url field")
		}
		return OpenAIContentPart{Type: "image_url", ImageURL: &OpenAIImageURL{URL: b.Source.URL}}, nil
	default:
		return OpenAIContentPart{}, fmt.Errorf("unknown image source type %q", b.Source.Type)
	}
}

// messageToBlocks reconstructs Anthropic content blocks from an OpenAI
// assistant message. Stage 2.6c block ordering: thinking first, then text,
// then tool_use — per Anthropic's contract that thinking precedes tool_use
// in assistant turns. Constant signature "shim-passthrough-v1" — shim
// doesn't verify on roundtrip (clients pass it back opaquely; DeepSeek
// ignores the field). See README "Errors and debugging" for the
// no-verification posture rationale.
func messageToBlocks(m OpenAIMessage) ([]AnthropicBlock, error) {
	var blocks []AnthropicBlock

	// Thinking block first (Stage 2.6c). Skip if empty — don't emit zero-
	// content thinking blocks just because the field exists.
	if m.ReasoningContent != "" {
		blocks = append(blocks, AnthropicBlock{
			Type:      "thinking",
			Thinking:  m.ReasoningContent,
			Signature: thinkingSignature,
		})
	}

	// Text content can be a JSON string or null/omitted.
	if len(m.Content) > 0 && string(m.Content) != "null" {
		text, err := flattenAssistantContent(m.Content)
		if err != nil {
			return nil, err
		}
		if text != "" {
			blocks = append(blocks, AnthropicBlock{Type: "text", Text: text})
		}
	}

	// Tool calls become tool_use blocks (impl in tools.go).
	for i, tc := range m.ToolCalls {
		b, err := toolCallToAnthropic(tc)
		if err != nil {
			return nil, fmt.Errorf("tool_calls[%d]: %w", i, err)
		}
		blocks = append(blocks, b)
	}

	// Anthropic responses always have at least one block. If empty, emit an
	// empty text block to keep the contract honest.
	if len(blocks) == 0 {
		blocks = []AnthropicBlock{{Type: "text", Text: ""}}
	}
	return blocks, nil
}

// thinkingSignature is the constant string shim attaches to every emitted
// thinking block. Anthropic's signature semantic is "opaque to clients;
// server-validated on roundtrip" — shim is the server in this loop, and
// the loopback threat model + non-verified-on-egress posture make HMAC
// theater unnecessary. Clients pass this back unchanged; DeepSeek discards
// the field entirely. See README "Errors and debugging" for the design
// rationale (so a future reader doesn't add HMAC back as "fix the gap").
const thinkingSignature = "shim-passthrough-v1"

// flattenAssistantContent collapses an OpenAI assistant content (which can
// be a string or an array of parts) into a single text string. For the
// assistant role, parts arrays are rare but possible — concatenate text
// parts and ignore other types (image_url shouldn't appear from upstream).
func flattenAssistantContent(raw json.RawMessage) (string, error) {
	raw = trimJSON(raw)
	if len(raw) == 0 {
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
		var parts []OpenAIContentPart
		if err := json.Unmarshal(raw, &parts); err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String(), nil
	}
	return "", fmt.Errorf("unrecognised content shape")
}

// encodeContent picks a compact form: a single text part becomes a JSON
// string; multi-part becomes a JSON array.
func encodeContent(parts []OpenAIContentPart) (json.RawMessage, error) {
	if len(parts) == 1 && parts[0].Type == "text" {
		return json.Marshal(parts[0].Text)
	}
	return json.Marshal(parts)
}

// trimJSON drops leading/trailing whitespace bytes from a json.RawMessage
// so we can peek at the first significant byte to detect string vs array.
func trimJSON(raw json.RawMessage) json.RawMessage {
	i, j := 0, len(raw)
	for i < j && isWhitespace(raw[i]) {
		i++
	}
	for j > i && isWhitespace(raw[j-1]) {
		j--
	}
	return raw[i:j]
}

func isWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
