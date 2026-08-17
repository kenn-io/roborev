package streamfmt

import (
	"encoding/json"
	"strings"
)

type openCodeDecoder struct {
	renderedToolIDs map[string]struct{}
}

type openCodeStreamEvent struct {
	Type string          `json:"type"`
	Part json.RawMessage `json:"part,omitempty"`
}

type openCodeTextPart struct {
	Text string `json:"text,omitempty"`
}

type openCodeToolPart struct {
	Tool  string `json:"tool"`
	ID    string `json:"id,omitempty"`
	State struct {
		Status string                     `json:"status,omitempty"`
		Input  map[string]json.RawMessage `json:"input,omitempty"`
	} `json:"state"`
}

func (d *openCodeDecoder) Decode(line string) []Event {
	var streamEvent openCodeStreamEvent
	if err := json.Unmarshal([]byte(line), &streamEvent); err != nil {
		return []Event{{Kind: EventText, Text: line}}
	}
	if len(streamEvent.Part) == 0 {
		return nil
	}

	switch streamEvent.Type {
	case "text":
		var part openCodeTextPart
		if err := json.Unmarshal(streamEvent.Part, &part); err != nil || part.Text == "" {
			return nil
		}
		return []Event{{Kind: EventText, Text: part.Text}}
	case "reasoning":
		var part openCodeTextPart
		if err := json.Unmarshal(streamEvent.Part, &part); err != nil {
			return nil
		}
		text := strings.TrimSpace(part.Text)
		if text == "" {
			return nil
		}
		return []Event{{Kind: EventReasoning, Text: text}}
	case "tool", "tool_use":
		return d.decodeTool(streamEvent.Part)
	default:
		return nil
	}
}

func (*openCodeDecoder) Flush() []Event { return nil }

func (d *openCodeDecoder) decodeTool(raw json.RawMessage) []Event {
	var part openCodeToolPart
	if err := json.Unmarshal(raw, &part); err != nil || part.Tool == "" {
		return nil
	}
	if part.State.Status != "running" && part.State.Status != "completed" {
		return nil
	}
	if part.ID != "" {
		if d.renderedToolIDs == nil {
			d.renderedToolIDs = make(map[string]struct{})
		}
		if _, seen := d.renderedToolIDs[part.ID]; seen {
			return nil
		}
		d.renderedToolIDs[part.ID] = struct{}{}
	}
	input, err := json.Marshal(part.State.Input)
	if err != nil {
		input = nil
	}
	return []Event{toolEvent(part.Tool, input)}
}
