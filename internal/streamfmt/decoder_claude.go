package streamfmt

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

type claudeDecoder struct{}

type claudeStreamEvent struct {
	Type    string         `json:"type"`
	Message jsontext.Value `json:"message,omitempty"`
}

type claudeContentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input jsontext.Value `json:"input,omitempty"`
}

func (*claudeDecoder) Decode(line string) []Event {
	var streamEvent claudeStreamEvent
	if err := json.Unmarshal([]byte(line), &streamEvent); err != nil {
		return []Event{{Kind: EventText, Text: line}}
	}
	if streamEvent.Type != "assistant" {
		return nil
	}
	_, content, ok := decodeClaudeMessage(streamEvent.Message)
	if !ok {
		return nil
	}
	return decodeClaudeContent(content)
}

func (*claudeDecoder) Flush() []Event { return nil }

func decodeClaudeContent(raw jsontext.Value) []Event {
	var blocks []claudeContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		events := make([]Event, 0, len(blocks))
		for _, block := range blocks {
			switch block.Type {
			case "text":
				events = append(events, Event{Kind: EventText, Text: block.Text})
			case "tool_use":
				events = append(events, toolEvent(block.Name, block.Input))
			}
		}
		return events
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []Event{{Kind: EventText, Text: text}}
	}
	return nil
}

// decodeClaudeMessage accepts the nested message object shared by Claude and
// Pi while rejecting Grok's string-valued error message.
func decodeClaudeMessage(
	raw jsontext.Value,
) (role string, content jsontext.Value, ok bool) {
	if len(raw) == 0 || string(raw) == "null" || raw[0] == '"' {
		return "", nil, false
	}
	var message struct {
		Role    string         `json:"role,omitempty"`
		Content jsontext.Value `json:"content,omitempty"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return "", nil, false
	}
	if len(message.Content) == 0 {
		return message.Role, nil, false
	}
	return message.Role, message.Content, true
}
