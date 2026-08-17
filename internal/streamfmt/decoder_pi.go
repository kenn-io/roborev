package streamfmt

import (
	"encoding/json"
	"strings"
)

type piDecoder struct {
	renderedToolIDs   map[string]struct{}
	lastAssistantText string
}

type piStreamEvent struct {
	Type                  string                   `json:"type"`
	AssistantMessageEvent *piAssistantMessageEvent `json:"assistantMessageEvent,omitempty"`
	Message               json.RawMessage          `json:"message,omitempty"`
	ToolCallID            string                   `json:"toolCallId,omitempty"`
	ToolName              string                   `json:"toolName,omitempty"`
	Args                  json.RawMessage          `json:"args,omitempty"`
}

type piAssistantMessageEvent struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
}

type piContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (d *piDecoder) Decode(line string) []Event {
	var streamEvent piStreamEvent
	if err := json.Unmarshal([]byte(line), &streamEvent); err != nil {
		return []Event{{Kind: EventText, Text: line}}
	}

	switch streamEvent.Type {
	case "message_update":
		if streamEvent.AssistantMessageEvent == nil ||
			streamEvent.AssistantMessageEvent.Type != "text_end" {
			return nil
		}
		return d.assistantText(streamEvent.AssistantMessageEvent.Content)
	case "message_end":
		role, content, ok := decodeClaudeMessage(streamEvent.Message)
		if !ok || role != "assistant" {
			return nil
		}
		return d.decodeMessageEnd(content)
	case "tool_execution_start":
		if streamEvent.ToolCallID != "" {
			if d.renderedToolIDs == nil {
				d.renderedToolIDs = make(map[string]struct{})
			}
			if _, seen := d.renderedToolIDs[streamEvent.ToolCallID]; seen {
				return nil
			}
			d.renderedToolIDs[streamEvent.ToolCallID] = struct{}{}
		}
		return []Event{toolEvent(streamEvent.ToolName, streamEvent.Args)}
	default:
		return nil
	}
}

func (*piDecoder) Flush() []Event { return nil }

func (d *piDecoder) decodeMessageEnd(raw json.RawMessage) []Event {
	var blocks []piContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) == 0 {
			return nil
		}
		return d.assistantText(strings.Join(parts, "\n"))
	}

	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil
	}
	return d.assistantText(text)
}

func (d *piDecoder) assistantText(text string) []Event {
	text = strings.TrimSpace(SanitizeControlKeepNewlines(text))
	if text == "" || text == d.lastAssistantText {
		return nil
	}
	d.lastAssistantText = text
	return []Event{{Kind: EventText, Text: text}}
}
