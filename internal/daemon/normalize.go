package daemon

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

// ansiEscapePattern matches ANSI escape sequences (colors, cursor movement, etc.)
var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\]([^\x07\x1b]|\x1b[^\\])*(\x07|\x1b\\)`)

// GetNormalizer returns the appropriate normalizer for an agent.
func GetNormalizer(agentName string) OutputNormalizer {
	switch agentName {
	case "claude-code":
		return NormalizeClaudeOutput
	case "codex":
		return NormalizeCodexOutput
	case "grok":
		return NormalizeGrokOutput
	case "opencode":
		return NormalizeOpenCodeOutput
	default:
		return NormalizeGenericOutput
	}
}

// NormalizeGrokOutput parses Grok Build's streaming-json format and extracts
// readable text, tool activity, and errors while suppressing lifecycle noise.
func NormalizeGrokOutput(line string) *OutputLine {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	var ev struct {
		Type     string `json:"type"`
		Data     string `json:"data,omitempty"`
		Message  string `json:"message,omitempty"`
		ToolName string `json:"toolName,omitempty"`
		Title    string `json:"title,omitempty"`
		Kind     string `json:"kind,omitempty"`
		Status   string `json:"status,omitempty"`
		Error    string `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return &OutputLine{Text: sanitizeControl(line), Type: "text"}
	}

	switch ev.Type {
	case "text":
		if ev.Data == "" {
			return nil
		}
		return &OutputLine{Text: sanitizeControl(ev.Data), Type: "text"}
	case "tool_call":
		name := firstGrokNonEmpty(ev.ToolName, ev.Title, ev.Kind)
		if name == "" {
			return &OutputLine{Text: "[Tool call]", Type: "tool"}
		}
		return &OutputLine{Text: "[Tool: " + sanitizeControl(name) + "]", Type: "tool"}
	case "tool_call_update":
		name := firstGrokNonEmpty(ev.ToolName, ev.Title, ev.Kind)
		if ev.Status == "failed" || ev.Status == "error" || ev.Error != "" {
			detail := firstGrokNonEmpty(ev.Error, name, "tool call failed")
			return &OutputLine{Text: "[Tool error: " + sanitizeControl(detail) + "]", Type: "error"}
		}
		return nil
	case "error":
		detail := firstGrokNonEmpty(ev.Message, ev.Data, "error in stream")
		return &OutputLine{Text: "[Error: " + sanitizeControl(detail) + "]", Type: "error"}
	case "thought", "reasoning", "plan", "end", "session", "session_start":
		return nil
	default:
		return nil
	}
}

func firstGrokNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// claudeNoisePatterns are status messages from Claude CLI that aren't useful progress info
var claudeNoisePatterns = []string{
	"mcp startup:",
	"Initializing",
	"Connected to",
	"Session started",
}

// isClaudeNoise returns true if the line is a Claude CLI status message to filter out
func isClaudeNoise(line string) bool {
	for _, pattern := range claudeNoisePatterns {
		if strings.Contains(line, pattern) {
			return true
		}
	}
	return false
}

// NormalizeClaudeOutput parses Claude's stream-json format and extracts readable content.
func NormalizeClaudeOutput(line string) *OutputLine {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	// Filter out Claude CLI noise/status messages
	if isClaudeNoise(line) {
		return nil
	}

	// Try to parse as JSON
	var msg struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype,omitempty"`
		Message struct {
			Content string `json:"content,omitempty"`
		} `json:"message,omitempty"`
		Result    string `json:"result,omitempty"`
		SessionID string `json:"session_id,omitempty"`

		// Tool-related fields
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`

		// Content delta for streaming
		ContentBlockDelta struct {
			Delta struct {
				Text string `json:"text,omitempty"`
			} `json:"delta,omitempty"`
		} `json:"content_block_delta,omitempty"`
	}

	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		// Not JSON - return as raw text (shouldn't happen with Claude)
		return &OutputLine{Text: stripANSI(line), Type: "text"}
	}

	switch msg.Type {
	case "assistant":
		// Full assistant message with content
		if msg.Message.Content != "" {
			// Replace embedded newlines with spaces to keep output on single line
			text := strings.ReplaceAll(msg.Message.Content, "\n", " ")
			text = strings.ReplaceAll(text, "\r", "")
			return &OutputLine{Text: text, Type: "text"}
		}
		// Skip empty assistant messages (e.g., start of response)
		return nil

	case "result":
		// Final result summary
		if msg.Result != "" {
			// Replace embedded newlines with spaces
			text := strings.ReplaceAll(msg.Result, "\n", " ")
			text = strings.ReplaceAll(text, "\r", "")
			return &OutputLine{Text: text, Type: "text"}
		}
		return nil

	case "tool_use":
		// Tool being called
		if msg.Name != "" {
			return &OutputLine{Text: "[Tool: " + msg.Name + "]", Type: "tool"}
		}
		return nil

	case "tool_result":
		// Tool finished - could show brief indicator
		return &OutputLine{Text: "[Tool completed]", Type: "tool"}

	case "content_block_start":
		// Start of a content block - skip
		return nil

	case "content_block_delta":
		// Streaming text delta
		if msg.ContentBlockDelta.Delta.Text != "" {
			// Replace embedded newlines with spaces
			text := strings.ReplaceAll(msg.ContentBlockDelta.Delta.Text, "\n", " ")
			text = strings.ReplaceAll(text, "\r", "")
			if text == "" || text == " " {
				return nil
			}
			return &OutputLine{Text: text, Type: "text"}
		}
		return nil

	case "content_block_stop":
		// End of content block - skip
		return nil

	case "message_start", "message_delta", "message_stop":
		// Message lifecycle events - skip
		return nil

	case "system":
		// System messages (e.g., init)
		if msg.Subtype == "init" {
			if msg.SessionID != "" {
				sessionPrefix := msg.SessionID
				if len(sessionPrefix) > 8 {
					sessionPrefix = sessionPrefix[:8]
				}
				return &OutputLine{Text: "[Session: " + sessionPrefix + "...]", Type: "text"}
			}
		}
		return nil

	case "error":
		// Error message
		return &OutputLine{Text: "[Error in stream]", Type: "error"}

	default:
		// Unknown type - skip to avoid noise
		return nil
	}
}

// NormalizeCodexOutput parses codex's --json JSONL format and extracts readable content.
func NormalizeCodexOutput(line string) *OutputLine {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	// Try to parse as JSON
	var ev struct {
		Type string `json:"type"`
		Item struct {
			Type    string `json:"type,omitempty"`
			Text    string `json:"text,omitempty"`
			Command string `json:"command,omitempty"`
			Status  string `json:"status,omitempty"`
		} `json:"item,omitempty"`
	}

	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		// Not JSON - return as raw text with full sanitization
		return &OutputLine{Text: sanitizeControl(line), Type: "text"}
	}

	switch ev.Type {
	case "item.completed":
		switch ev.Item.Type {
		case "agent_message":
			if ev.Item.Text != "" {
				return &OutputLine{Text: sanitizeControl(ev.Item.Text), Type: "text"}
			}
		case "command_execution":
			if ev.Item.Command != "" {
				return &OutputLine{Text: "[Command: " + sanitizeControl(ev.Item.Command) + "]", Type: "tool"}
			}
			return &OutputLine{Text: "[Command completed]", Type: "tool"}
		case "file_change":
			return &OutputLine{Text: "[File change]", Type: "tool"}
		}
		return nil

	case "item.started":
		switch ev.Item.Type {
		case "command_execution":
			if ev.Item.Command != "" {
				return &OutputLine{Text: "[Command: " + sanitizeControl(ev.Item.Command) + "]", Type: "tool"}
			}
		}
		return nil

	case "thread.started", "turn.started", "turn.completed":
		// Lifecycle events - skip
		return nil

	case "turn.failed", "error":
		return &OutputLine{Text: "[Error in stream]", Type: "error"}

	default:
		return nil
	}
}

// NormalizeOpenCodeOutput normalizes OpenCode output (plain text with ANSI codes).
func NormalizeOpenCodeOutput(line string) *OutputLine {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	// Filter tool call JSON lines (same logic as in agent package)
	if isToolCallJSON(line) {
		return &OutputLine{Text: "[Tool call]", Type: "tool"}
	}

	// Strip ANSI codes for clean display
	text := stripANSI(line)
	if text == "" {
		return nil
	}

	return &OutputLine{Text: text, Type: "text"}
}

// NormalizeGenericOutput is the default normalizer for other agents.
func NormalizeGenericOutput(line string) *OutputLine {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	// Strip ANSI codes
	text := stripANSI(line)
	if text == "" {
		return nil
	}

	// Filter tool call JSON if present
	if isToolCallJSON(text) {
		return &OutputLine{Text: "[Tool call]", Type: "tool"}
	}

	return &OutputLine{Text: text, Type: "text"}
}

// stripANSI removes ANSI escape sequences from a string.
func stripANSI(s string) string {
	return ansiEscapePattern.ReplaceAllString(s, "")
}

// sanitizeControl strips ANSI escapes and non-printable control characters,
// replacing newlines with spaces to avoid collapsing words. Used for
// untrusted model/subprocess output that reaches terminals.
func sanitizeControl(s string) string {
	s = stripANSI(s)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\t' || !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isToolCallJSON checks if a line is a tool call JSON object.
// Tool calls have exactly "name" and "arguments" keys.
func isToolCallJSON(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return false
	}
	// Tool calls have exactly "name" and "arguments" keys
	if len(m) != 2 {
		return false
	}
	_, hasName := m["name"]
	_, hasArgs := m["arguments"]
	return hasName && hasArgs
}
