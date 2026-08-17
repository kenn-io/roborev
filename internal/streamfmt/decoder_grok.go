package streamfmt

import (
	"encoding/json"
	"strings"
)

type grokDecoder struct {
	renderedToolIDs map[string]struct{}
	toolByID        map[string]grokToolInfo
	text            strings.Builder
}

type grokToolInfo struct {
	name  string
	title string
	kind  string
}

type grokStreamEvent struct {
	Type              string          `json:"type"`
	Message           json.RawMessage `json:"message,omitempty"`
	Data              string          `json:"data,omitempty"`
	Status            string          `json:"status,omitempty"`
	Error             string          `json:"error,omitempty"`
	Content           json.RawMessage `json:"content,omitempty"`
	ToolCallID        string          `json:"toolCallId,omitempty"`
	ToolName          string          `json:"toolName,omitempty"`
	AlternateToolName string          `json:"tool_name,omitempty"`
	Args              json.RawMessage `json:"args,omitempty"`
	Parameters        json.RawMessage `json:"parameters,omitempty"`
	RawInput          json.RawMessage `json:"rawInput,omitempty"`
	RawOutput         json.RawMessage `json:"rawOutput,omitempty"`
	Title             string          `json:"title,omitempty"`
	Kind              string          `json:"kind,omitempty"`
}

func (d *grokDecoder) Decode(line string) []Event {
	var streamEvent grokStreamEvent
	if err := json.Unmarshal([]byte(line), &streamEvent); err != nil {
		return append(
			d.flushBoundary(), Event{Kind: EventText, Text: line},
		)
	}

	if streamEvent.Type == "text" && streamEvent.Data != "" {
		d.text.WriteString(streamEvent.Data)
		return nil
	}

	events := d.flushBoundary()
	switch streamEvent.Type {
	case "thought", "reasoning":
		if text := strings.TrimSpace(streamEvent.Data); text != "" {
			events = append(events, Event{Kind: EventReasoning, Text: text})
		}
	case "tool_call":
		if event, ok := d.decodeToolCall(streamEvent); ok {
			events = append(events, event)
		}
	case "tool_call_update":
		if event, ok := d.decodeToolUpdate(streamEvent); ok {
			events = append(events, event)
		}
	case "error":
		message := strings.TrimSpace(jsonStringField(streamEvent.Message))
		if message == "" {
			message = strings.TrimSpace(streamEvent.Data)
		}
		if message == "" {
			message = strings.TrimSpace(streamEvent.Error)
		}
		if message != "" {
			events = append(events, Event{
				Kind: EventText,
				Text: "error: " + message,
			})
		}
	}
	return events
}

func (d *grokDecoder) Flush() []Event {
	if d.text.Len() == 0 {
		return nil
	}
	text := d.text.String()
	d.text.Reset()
	return []Event{{Kind: EventText, Text: text}}
}

func (d *grokDecoder) flushBoundary() []Event {
	if d.text.Len() == 0 {
		return nil
	}
	text := d.text.String()
	d.text.Reset()
	return []Event{
		{Kind: EventText, Text: text},
		{Kind: EventBoundary},
	}
}

func (d *grokDecoder) decodeToolCall(
	streamEvent grokStreamEvent,
) (Event, bool) {
	id := streamEvent.ToolCallID
	if id != "" {
		if d.renderedToolIDs == nil {
			d.renderedToolIDs = make(map[string]struct{})
		}
		if _, seen := d.renderedToolIDs[id]; seen {
			return Event{}, false
		}
		d.renderedToolIDs[id] = struct{}{}
	}

	name := grokToolDisplayName(streamEvent, nil)
	if id != "" {
		if d.toolByID == nil {
			d.toolByID = make(map[string]grokToolInfo)
		}
		d.toolByID[id] = grokToolInfo{
			name: firstNonEmpty(
				streamEvent.ToolName, streamEvent.AlternateToolName,
			),
			title: streamEvent.Title,
			kind:  streamEvent.Kind,
		}
	}
	if name == "" {
		return Event{}, false
	}

	input := streamEvent.RawInput
	if len(input) == 0 {
		input = streamEvent.Args
	}
	if len(input) == 0 {
		input = streamEvent.Parameters
	}
	if len(input) > 4096 {
		input = input[:4096]
	}
	return toolEvent(name, input), true
}

func (d *grokDecoder) decodeToolUpdate(
	streamEvent grokStreamEvent,
) (Event, bool) {
	status := strings.ToLower(strings.TrimSpace(streamEvent.Status))
	if status != "failed" && status != "error" &&
		strings.TrimSpace(streamEvent.Error) == "" {
		return Event{}, false
	}
	message := grokFailureDetail(streamEvent)
	if message == "" {
		message = "tool failed"
	}
	if len(message) > 80 {
		message = message[:77] + "..."
	}

	var cached *grokToolInfo
	if info, ok := d.toolByID[streamEvent.ToolCallID]; ok {
		cached = &info
	}
	name := grokToolDisplayName(streamEvent, cached)
	if name == "" {
		name = "tool"
	}
	event := toolEvent(name, nil)
	event.Arg = "failed: " + message
	return event, true
}

func grokToolDisplayName(
	streamEvent grokStreamEvent, cached *grokToolInfo,
) string {
	if name := firstNonEmpty(
		streamEvent.ToolName, streamEvent.AlternateToolName,
	); name != "" {
		return name
	}
	if streamEvent.Title != "" {
		return streamEvent.Title
	}
	if streamEvent.Kind != "" {
		return streamEvent.Kind
	}
	if cached != nil {
		return firstNonEmpty(cached.name, cached.title, cached.kind)
	}
	return ""
}

func grokFailureDetail(streamEvent grokStreamEvent) string {
	if message := strings.TrimSpace(sanitizeControl(streamEvent.Error)); message != "" {
		return message
	}
	if message := strings.TrimSpace(sanitizeControl(streamEvent.Data)); message != "" {
		return message
	}
	if message := extractFailureFromJSON(streamEvent.RawOutput); message != "" {
		return message
	}
	return extractFailureFromJSON(streamEvent.Content)
}

func extractFailureFromJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if value := jsonStringField(raw); value != "" {
		return strings.TrimSpace(sanitizeControl(value))
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err == nil {
		for _, key := range []string{"error", "message", "stderr", "detail"} {
			if value := jsonString(object[key]); value != "" {
				return strings.TrimSpace(sanitizeControl(value))
			}
		}
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, rawBlock := range blocks {
			if value := jsonStringField(rawBlock); value != "" {
				return strings.TrimSpace(sanitizeControl(value))
			}
			var block struct {
				Text    string `json:"text"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(rawBlock, &block); err == nil {
				if value := firstNonEmpty(block.Text, block.Content); value != "" {
					return strings.TrimSpace(sanitizeControl(value))
				}
			}
		}
	}
	return ""
}
