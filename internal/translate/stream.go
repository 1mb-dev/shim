package translate

import (
	"encoding/json"
	"fmt"
)

// SSEEvent is one Anthropic-shaped SSE event, ready to wire-format.
// Marshal as `event: <Name>\ndata: <JSON(Data)>\n\n`.
type SSEEvent struct {
	Name string
	Data any
}

// Anthropic streaming event shapes (Stage 0 MVP — text + tool_use). See
// https://docs.anthropic.com/en/api/messages-streaming for the full
// protocol.

type sseMessageStart struct {
	Type    string                 `json:"type"`
	Message sseMessageStartContent `json:"message"`
}

type sseMessageStartContent struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []AnthropicBlock `json:"content"`
	StopReason   *string          `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        AnthropicUsage   `json:"usage"`
}

type sseContentBlockStart struct {
	Type         string         `json:"type"`
	Index        int            `json:"index"`
	ContentBlock AnthropicBlock `json:"content_block"`
}

type sseContentBlockDelta struct {
	Type  string   `json:"type"`
	Index int      `json:"index"`
	Delta sseDelta `json:"delta"`
}

type sseDelta struct {
	Type        string `json:"type"` // "text_delta" | "input_json_delta" | "thinking_delta" | "signature_delta"
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`  // for thinking_delta (Stage 2.6c)
	Signature   string `json:"signature,omitempty"` // for signature_delta (Stage 2.6c)
}

type sseContentBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type sseMessageDelta struct {
	Type  string              `json:"type"`
	Delta sseMessageDeltaBody `json:"delta"`
	Usage AnthropicUsage      `json:"usage"`
}

type sseMessageDeltaBody struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type sseMessageStop struct {
	Type string `json:"type"`
}

// ToAnthropicSSE converts a complete OpenAI response into an ordered
// sequence of Anthropic SSE events. Stage 0 emits the sequence in a single
// burst (buffer-then-restream) so callers see the canonical event order
// even though no real per-token streaming happens. Tool-use blocks emit one
// content_block_start + a single input_json_delta carrying the full
// arguments + content_block_stop.
func ToAnthropicSSE(resp *OpenAIResponse, originalModel string) ([]SSEEvent, error) {
	if resp == nil {
		return nil, fmt.Errorf("nil response")
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("response has no choices")
	}

	choice := resp.Choices[0]
	stopReason := mapFinishReason(choice.FinishReason)

	// Reconstruct content blocks once so we can index into them.
	blocks, err := messageToBlocks(choice.Message)
	if err != nil {
		return nil, fmt.Errorf("rebuild blocks: %w", err)
	}

	events := make([]SSEEvent, 0, 2+3*len(blocks))

	events = append(events, SSEEvent{
		Name: "message_start",
		Data: sseMessageStart{
			Type: "message_start",
			Message: sseMessageStartContent{
				ID:      resp.ID,
				Type:    "message",
				Role:    "assistant",
				Model:   originalModel,
				Content: []AnthropicBlock{},
				Usage: AnthropicUsage{
					InputTokens:  resp.Usage.PromptTokens,
					OutputTokens: 0, // updated in final message_delta
				},
			},
		},
	})

	for i, b := range blocks {
		startBlock := AnthropicBlock{Type: b.Type}
		switch b.Type {
		case "text":
			// content_block_start carries an empty text; delta carries the body.
			events = append(events, SSEEvent{
				Name: "content_block_start",
				Data: sseContentBlockStart{
					Type: "content_block_start", Index: i,
					ContentBlock: startBlock,
				},
			})
			events = append(events, SSEEvent{
				Name: "content_block_delta",
				Data: sseContentBlockDelta{
					Type: "content_block_delta", Index: i,
					Delta: sseDelta{Type: "text_delta", Text: b.Text},
				},
			})
		case "tool_use":
			startBlock.ID = b.ID
			startBlock.Name = b.Name
			startBlock.Input = json.RawMessage(`{}`)
			events = append(events, SSEEvent{
				Name: "content_block_start",
				Data: sseContentBlockStart{
					Type: "content_block_start", Index: i,
					ContentBlock: startBlock,
				},
			})
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			events = append(events, SSEEvent{
				Name: "content_block_delta",
				Data: sseContentBlockDelta{
					Type: "content_block_delta", Index: i,
					Delta: sseDelta{Type: "input_json_delta", PartialJSON: args},
				},
			})
		case "thinking":
			// Stage 2.6c — Anthropic's streaming protocol for thinking:
			// content_block_start carries empty thinking + empty signature;
			// thinking_delta carries text chunks (one chunk here, since
			// shim is buffer-then-restream); signature_delta carries the
			// signature; content_block_stop closes the block.
			events = append(events, SSEEvent{
				Name: "content_block_start",
				Data: sseContentBlockStart{
					Type: "content_block_start", Index: i,
					ContentBlock: AnthropicBlock{Type: "thinking"},
				},
			})
			events = append(events, SSEEvent{
				Name: "content_block_delta",
				Data: sseContentBlockDelta{
					Type: "content_block_delta", Index: i,
					Delta: sseDelta{Type: "thinking_delta", Thinking: b.Thinking},
				},
			})
			events = append(events, SSEEvent{
				Name: "content_block_delta",
				Data: sseContentBlockDelta{
					Type: "content_block_delta", Index: i,
					Delta: sseDelta{Type: "signature_delta", Signature: b.Signature},
				},
			})
		default:
			return nil, fmt.Errorf("block[%d]: unsupported streaming type %q", i, b.Type)
		}
		events = append(events, SSEEvent{
			Name: "content_block_stop",
			Data: sseContentBlockStop{Type: "content_block_stop", Index: i},
		})
	}

	events = append(events, SSEEvent{
		Name: "message_delta",
		Data: sseMessageDelta{
			Type:  "message_delta",
			Delta: sseMessageDeltaBody{StopReason: stopReason},
			Usage: AnthropicUsage{
				InputTokens:  resp.Usage.PromptTokens,
				OutputTokens: resp.Usage.CompletionTokens,
			},
		},
	})
	events = append(events, SSEEvent{
		Name: "message_stop",
		Data: sseMessageStop{Type: "message_stop"},
	})

	return events, nil
}
