package streamfmt

import "strings"

// Decoder converts one provider stream line into neutral events.
type Decoder interface {
	Decode(line string) []Event
	Flush() []Event
}

// DecoderForAgent returns a fresh decoder selected only from agent identity.
func DecoderForAgent(agent string) Decoder {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "claude", "claude-code", "cursor":
		return &claudeDecoder{}
	case "gemini":
		return &geminiDecoder{}
	case "codex":
		return &codexDecoder{}
	case "opencode", "kilo":
		return &openCodeDecoder{}
	case "pi":
		return &piDecoder{}
	case "grok", "grok-build":
		return &grokDecoder{}
	default:
		return &literalDecoder{}
	}
}

// AgentUsesJSONL reports whether the agent's live log is newline-delimited JSON.
// Literal-text agents, including ACP, are false so a running job can stream an
// unterminated tail.
func AgentUsesJSONL(agent string) bool {
	_, literal := DecoderForAgent(agent).(*literalDecoder)
	return !literal
}

type literalDecoder struct{}

func (literalDecoder) Decode(line string) []Event {
	return []Event{{Kind: EventLiteral, Text: line}}
}

func (literalDecoder) Flush() []Event { return nil }
