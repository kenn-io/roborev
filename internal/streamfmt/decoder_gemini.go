package streamfmt

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"

	"go.kenn.io/roborev/internal/agent/antigravity"
)

type geminiDecoder struct{}

const geminiDeprecatedPromptMessage = "The --prompt (-p) flag has been deprecated and will be removed in a future version. Please use a positional argument for your prompt. See gemini --help for more information."

type geminiStreamEvent struct {
	Type       string             `json:"type"`
	Event      string             `json:"event"`
	Role       string             `json:"role,omitempty"`
	Content    jsontext.Value     `json:"content,omitempty"`
	ToolName   string             `json:"tool_name,omitempty"`
	Parameters jsontext.Value     `json:"parameters,omitempty"`
	Result     antigravity.Result `json:"result"`
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
		if streamEvent.Result.Status != "SUCCESS" {
			return []Event{{Kind: EventText, Text: "[Error: " + streamEvent.Result.Error + "]"}}
		}
		return []Event{{Kind: EventText, Text: streamEvent.Result.Response}}
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
		if strings.TrimSpace(text) == geminiDeprecatedPromptMessage {
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
