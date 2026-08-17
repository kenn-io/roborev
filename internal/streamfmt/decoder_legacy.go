package streamfmt

import (
	"encoding/json"
	"strings"
)

type legacyProtocol uint8

const (
	legacyLiteral legacyProtocol = iota
	legacyClaude
	legacyGemini
	legacyCodex
	legacyOpenCode
	legacyPi
	legacyGrok
)

// LegacyMixedDecoder enables minimal protocol-shape detection for persisted
// auto-design logs that can contain appended output from two providers. Normal
// callers must use DecoderForAgent instead.
func LegacyMixedDecoder(agent string) Decoder {
	fallback := legacyProtocolForAgent(agent)
	return &legacyMixedDecoder{
		fallback: fallback,
		decoders: map[legacyProtocol]Decoder{
			legacyLiteral:  &literalDecoder{},
			legacyClaude:   &claudeDecoder{},
			legacyGemini:   &geminiDecoder{},
			legacyCodex:    &codexDecoder{},
			legacyOpenCode: &openCodeDecoder{},
			legacyPi:       &piDecoder{},
			legacyGrok:     &grokDecoder{},
		},
	}
}

type legacyMixedDecoder struct {
	fallback  legacyProtocol
	active    legacyProtocol
	hasActive bool
	decoders  map[legacyProtocol]Decoder
}

func (d *legacyMixedDecoder) Decode(line string) []Event {
	protocol := detectLegacyProtocol(line)
	if protocol == legacyLiteral {
		protocol = d.fallback
	}

	var events []Event
	if d.hasActive && protocol != d.active {
		flushed := d.decoders[d.active].Flush()
		if len(flushed) > 0 {
			events = append(events, flushed...)
			events = append(events, Event{Kind: EventBoundary})
		}
	}
	d.active = protocol
	d.hasActive = true
	return append(events, d.decoders[protocol].Decode(line)...)
}

func (d *legacyMixedDecoder) Flush() []Event {
	if !d.hasActive {
		return nil
	}
	return d.decoders[d.active].Flush()
}

func legacyProtocolForAgent(agent string) legacyProtocol {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "claude", "claude-code", "cursor":
		return legacyClaude
	case "gemini":
		return legacyGemini
	case "codex":
		return legacyCodex
	case "opencode", "kilo":
		return legacyOpenCode
	case "pi":
		return legacyPi
	case "grok", "grok-build":
		return legacyGrok
	default:
		return legacyLiteral
	}
}

func detectLegacyProtocol(line string) legacyProtocol {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return legacyLiteral
	}
	var eventType string
	if err := json.Unmarshal(probe["type"], &eventType); err != nil {
		return legacyLiteral
	}
	if part := probe["part"]; len(part) > 0 && string(part) != "null" {
		return legacyOpenCode
	}

	switch eventType {
	case "assistant":
		return legacyClaude
	case "message":
		if len(probe["role"]) > 0 || len(probe["content"]) > 0 {
			return legacyGemini
		}
	case "tool_use":
		if len(probe["tool_name"]) > 0 {
			return legacyGemini
		}
	case "item.started", "item.completed", "item.updated",
		"thread.started", "turn.started", "turn.completed":
		return legacyCodex
	case "message_update", "message_end", "tool_execution_start",
		"tool_execution_update", "tool_execution_end", "agent_start",
		"agent_end", "turn_start", "turn_end":
		return legacyPi
	case "text", "reasoning":
		if len(probe["data"]) > 0 {
			return legacyGrok
		}
	case "thought", "tool_call", "tool_call_update", "plan", "error":
		return legacyGrok
	}
	return legacyLiteral
}
