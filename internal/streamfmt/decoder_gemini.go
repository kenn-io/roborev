package streamfmt

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

type geminiDecoder struct {
	sawAntigravityText bool
}

type geminiStreamEvent struct {
	Type       string         `json:"type"`
	Event      string         `json:"event"`
	Role       string         `json:"role,omitempty"`
	Content    jsontext.Value `json:"content,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	Parameters jsontext.Value `json:"parameters,omitempty"`
	StepUpdate struct {
		StepType  string `json:"step_type"`
		TextDelta string `json:"text_delta"`
		ToolName  string `json:"tool_name"`
		ToolInfo  struct {
			Name       string         `json:"name"`
			Parameters jsontext.Value `json:"parameters"`
		} `json:"tool_info"`
	} `json:"step_update,omitempty"`
	Result struct {
		Status         string `json:"status"`
		Response       string `json:"response"`
		Error          string `json:"error"`
		FinishedReason string `json:"finished_reason"`
	} `json:"result,omitempty"`
}

func (d *geminiDecoder) Decode(line string) []Event {
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
	}

	switch streamEvent.Event {
	case "init":
		return nil
	case "step_update":
		switch streamEvent.StepUpdate.StepType {
		case "agent_response":
			if streamEvent.StepUpdate.TextDelta == "" {
				return nil
			}
			d.sawAntigravityText = true
			return []Event{{Kind: EventText, Text: streamEvent.StepUpdate.TextDelta}}
		case "tool":
			name := streamEvent.StepUpdate.ToolName
			parameters := jsontext.Value(nil)
			if name == "" {
				name = streamEvent.StepUpdate.ToolInfo.Name
			}
			parameters = streamEvent.StepUpdate.ToolInfo.Parameters
			if name == "" {
				return nil
			}
			return []Event{toolEvent(name, parameters)}
		default:
			return nil
		}
	case "result":
		if streamEvent.Result.FinishedReason != "" {
			return []Event{{Kind: EventText, Text: "[Error: Antigravity result was incomplete: " + streamEvent.Result.FinishedReason + "]"}}
		}
		if streamEvent.Result.Status != "SUCCESS" {
			message := streamEvent.Result.Error
			if message == "" {
				message = "Antigravity ended with status " + streamEvent.Result.Status
			}
			return []Event{{Kind: EventText, Text: "[Error: " + message + "]"}}
		}
		if !d.sawAntigravityText && streamEvent.Result.Response != "" {
			return []Event{{Kind: EventText, Text: streamEvent.Result.Response}}
		}
	}
	return nil
}

func (*geminiDecoder) Flush() []Event { return nil }
