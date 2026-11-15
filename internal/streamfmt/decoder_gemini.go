package streamfmt

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

type geminiDecoder struct{}

type geminiStreamEvent struct {
	Type       string         `json:"type"`
	Event      string         `json:"event"`
	Role       string         `json:"role,omitempty"`
	Content    jsontext.Value `json:"content,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	Parameters jsontext.Value `json:"parameters,omitempty"`
	Result     jsontext.Value `json:"result,omitempty"`
}

func (*geminiDecoder) Decode(line string) []Event {
	var streamEvent geminiStreamEvent
	if err := json.Unmarshal([]byte(line), &streamEvent); err != nil {
		return []Event{{Kind: EventText, Text: line}}
	}
	// Antigravity uses the documented init/result events. Intermediate events
	// stay hidden until the CLI produces a terminal result.
	if streamEvent.Event == "init" {
		return []Event{{Kind: EventText, Text: "Antigravity session started"}}
	}
	if streamEvent.Event == "result" {
		return geminiResultEvents(line, streamEvent.Result)
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

func geminiResultEvents(line string, raw jsontext.Value) []Event {
	var result struct {
		Status         string `json:"status"`
		Response       string `json:"response"`
		Error          string `json:"error"`
		FinishedReason string `json:"finished_reason"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return []Event{{Kind: EventText, Text: line}}
	}
	if result.Status != "SUCCESS" {
		if result.Error == "" {
			result.Error = "Antigravity finished with status " + result.Status
		}
		return []Event{{Kind: EventText, Text: "[Error: " + result.Error + "]"}}
	}
	events := []Event{{Kind: EventText, Text: result.Response}}
	if result.FinishedReason == "print_timeout" {
		events = append(events, Event{Kind: EventText, Text: "[Antigravity print timed out; output may be partial]"})
	}
	return events
}

func (*geminiDecoder) Flush() []Event { return nil }
