package streamfmt

import "encoding/json"

type geminiDecoder struct{}

type geminiStreamEvent struct {
	Type       string          `json:"type"`
	Role       string          `json:"role,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

func (*geminiDecoder) Decode(line string) []Event {
	var streamEvent geminiStreamEvent
	if err := json.Unmarshal([]byte(line), &streamEvent); err != nil {
		return []Event{{Kind: EventText, Text: line}}
	}

	switch streamEvent.Type {
	case "message":
		if streamEvent.Role != "assistant" {
			return nil
		}
		var text string
		if err := json.Unmarshal(streamEvent.Content, &text); err != nil {
			return nil
		}
		return []Event{{Kind: EventText, Text: text}}
	case "tool_use":
		if streamEvent.ToolName == "" {
			return nil
		}
		return []Event{toolEvent(streamEvent.ToolName, streamEvent.Parameters)}
	default:
		return nil
	}
}

func (*geminiDecoder) Flush() []Event { return nil }
