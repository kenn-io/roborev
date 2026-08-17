package streamfmt

import (
	"encoding/json"
	"strings"
)

type codexDecoder struct {
	commands codexCommandTracker
}

type codexStreamEvent struct {
	Type string     `json:"type"`
	Item *codexItem `json:"item,omitempty"`
}

type codexItem struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type,omitempty"`
	Text    string `json:"text,omitempty"`
	Command string `json:"command,omitempty"`
}

func (d *codexDecoder) Decode(line string) []Event {
	var streamEvent codexStreamEvent
	if err := json.Unmarshal([]byte(line), &streamEvent); err != nil {
		return []Event{{Kind: EventText, Text: line}}
	}
	if streamEvent.Item == nil {
		return nil
	}

	item := streamEvent.Item
	switch item.Type {
	case "reasoning":
		if streamEvent.Type != "item.completed" {
			return nil
		}
		text := strings.TrimSpace(item.Text)
		if text == "" {
			return nil
		}
		return []Event{{Kind: EventReasoning, Text: text}}
	case "agent_message":
		if streamEvent.Type != "item.completed" {
			return nil
		}
		return []Event{{Kind: EventText, Text: item.Text}}
	case "command_execution":
		command := strings.TrimSpace(sanitizeControl(item.Command))
		command, render := d.commands.Observe(
			streamEvent.Type, item.ID, command,
		)
		if !render {
			return nil
		}
		if len(command) > 80 {
			command = command[:77] + "..."
		}
		return []Event{{Kind: EventTool, Name: "Bash", Arg: command}}
	case "file_change":
		if streamEvent.Type == "item.completed" {
			return []Event{{Kind: EventTool, Name: "Edit"}}
		}
	}
	return nil
}

func (*codexDecoder) Flush() []Event { return nil }
