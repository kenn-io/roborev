package streamfmt

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
)

type geminiDecoder struct {
	text strings.Builder
}

type geminiStreamEvent struct {
	Type       string         `json:"type"`
	Event      string         `json:"event"`
	Role       string         `json:"role,omitempty"`
	Delta      bool           `json:"delta,omitempty"`
	Content    jsontext.Value `json:"content,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	Parameters jsontext.Value `json:"parameters,omitempty"`
	Result     struct {
		Status         string `json:"status"`
		Response       string `json:"response"`
		Error          string `json:"error"`
		FinishedReason string `json:"finished_reason"`
	} `json:"result,omitempty"`
}

func (d *geminiDecoder) Decode(line string) []Event {
	var streamEvent geminiStreamEvent
	if err := json.Unmarshal([]byte(line), &streamEvent); err != nil {
		return append(d.flushBoundary(), Event{Kind: EventText, Text: line})
	}

	switch streamEvent.Type {
	case "message":
		if streamEvent.Role != "assistant" {
			return d.flushBoundary()
		}
		var text string
		if err := json.Unmarshal(streamEvent.Content, &text); err != nil {
			return d.flushBoundary()
		}
		if streamEvent.Delta {
			d.text.WriteString(text)
			return nil
		}
		events := d.flushBoundary()
		if text != "" {
			events = append(events, Event{Kind: EventText, Text: text})
		}
		return events
	case "tool_use":
		events := d.flushBoundary()
		if streamEvent.ToolName == "" {
			return events
		}
		return append(events, toolEvent(streamEvent.ToolName, streamEvent.Parameters))
	case "tool_result", "result":
		return d.flushBoundary()
	}

	switch streamEvent.Event {
	case "init":
		return d.flushBoundary()
	case "result":
		events := d.flushBoundary()
		if streamEvent.Result.FinishedReason != "" {
			return append(events, Event{Kind: EventText, Text: "[Error: Antigravity result was incomplete: " + streamEvent.Result.FinishedReason + "]"})
		}
		if streamEvent.Result.Status != "SUCCESS" {
			message := streamEvent.Result.Error
			if message == "" {
				message = "Antigravity ended with status " + streamEvent.Result.Status
			}
			return append(events, Event{Kind: EventText, Text: "[Error: " + message + "]"})
		}
		if streamEvent.Result.Response != "" {
			return append(events, Event{Kind: EventText, Text: streamEvent.Result.Response})
		}
		return events
	}
	return nil
}

func (d *geminiDecoder) Flush() []Event {
	if d.text.Len() == 0 {
		return nil
	}
	text := d.text.String()
	d.text.Reset()
	return []Event{{Kind: EventText, Text: text}}
}

func (d *geminiDecoder) flushBoundary() []Event {
	events := d.Flush()
	if len(events) == 0 {
		return nil
	}
	return append(events, Event{Kind: EventBoundary})
}
