package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type normalizeTestCase struct {
	name        string
	input       string
	wantNil     bool
	wantText    string
	wantType    string
	notWantType string
}

func runNormalizeTests(t *testing.T, fn func(string) *OutputLine, cases []normalizeTestCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := fn(tc.input)
			if tc.wantNil {
				assert.Nil(t, result, "input %q: expected nil, got %+v", tc.input, result)
				return
			}
			require.NotNil(t, result, "input %q: expected non-nil result", tc.input)
			assert.Equal(t, tc.wantText, result.Text,
				"input %q: text = %q, want %q", tc.input, result.Text, tc.wantText)
			if tc.wantType != "" {
				assert.Equal(t, tc.wantType, result.Type, "input %q: type = %q, want %q", tc.input, result.Type, tc.wantType)
			}

			if tc.notWantType != "" {
				assert.NotEqual(t, tc.notWantType, result.Type,
					"input %q: type = %q, should not be %q", tc.input, result.Type, tc.notWantType)
			}
		})
	}
}

func TestNormalizeClaudeOutput(t *testing.T) {
	runNormalizeTests(t, NormalizeClaudeOutput, []normalizeTestCase{
		{
			name:     "AssistantMessage",
			input:    `{"type":"assistant","message":{"content":"Hello, I will review this code."}}`,
			wantText: "Hello, I will review this code.",
			wantType: "text",
		},
		{
			name:     "Result",
			input:    `{"type":"result","result":"Review complete: PASS"}`,
			wantText: "Review complete: PASS",
			wantType: "text",
		},
		{
			name:     "ToolUse",
			input:    `{"type":"tool_use","name":"Read","input":{"file_path":"/foo/bar.go"}}`,
			wantText: "[Tool: Read]",
			wantType: "tool",
		},
		{
			name:     "ToolResult",
			input:    `{"type":"tool_result","content":"file contents here"}`,
			wantText: "[Tool completed]",
			wantType: "tool",
		},
		{
			name:     "SystemInit",
			input:    `{"type":"system","subtype":"init","session_id":"abc123def456"}`,
			wantText: "[Session: abc123de...]",
		},
		{
			name:     "SystemInitShortSessionID",
			input:    `{"type":"system","subtype":"init","session_id":"abc"}`,
			wantText: "[Session: abc...]",
		},
		{
			name:     "InvalidJSON",
			input:    "not json at all",
			wantText: "not json at all",
		},
		{
			name:    "EmptyAssistant",
			input:   `{"type":"assistant","message":{}}`,
			wantNil: true,
		},
		{
			name:    "EmptyLine",
			input:   "",
			wantNil: true,
		},
		{
			name:    "WhitespaceLine",
			input:   "   ",
			wantNil: true,
		},
	})
}

func TestNormalizeClaudeOutput_SkipsLifecycleEvents(t *testing.T) {
	runNormalizeTests(t, NormalizeClaudeOutput, []normalizeTestCase{
		{name: "MessageStart", input: `{"type":"message_start"}`, wantNil: true},
		{name: "MessageDelta", input: `{"type":"message_delta"}`, wantNil: true},
		{name: "MessageStop", input: `{"type":"message_stop"}`, wantNil: true},
		{name: "ContentBlockStart", input: `{"type":"content_block_start"}`, wantNil: true},
		{name: "ContentBlockStop", input: `{"type":"content_block_stop"}`, wantNil: true},
	})
}

func TestNormalizeOpenCodeOutput(t *testing.T) {
	runNormalizeTests(t, NormalizeOpenCodeOutput, []normalizeTestCase{
		{
			name:     "PlainText",
			input:    "Reviewing the code changes...",
			wantText: "Reviewing the code changes...",
			wantType: "text",
		},
		{
			name:     "StripsANSI",
			input:    "\x1b[32mGreen text\x1b[0m",
			wantText: "Green text",
		},
		{
			name:     "FilterToolCall",
			input:    `{"name":"read","arguments":{"path":"/foo"}}`,
			wantText: "[Tool call]",
			wantType: "tool",
		},
		{
			name:        "PreservesJSONWithExtraKeys",
			input:       `{"name":"test","arguments":{},"extra":"key"}`,
			wantText:    `{"name":"test","arguments":{},"extra":"key"}`,
			notWantType: "tool",
		},
		{
			name:    "EmptyLine",
			input:   "",
			wantNil: true,
		},
	})
}

func TestNormalizeGenericOutput(t *testing.T) {
	runNormalizeTests(t, NormalizeGenericOutput, []normalizeTestCase{
		{
			name:     "PlainText",
			input:    "Some agent output",
			wantText: "Some agent output",
		},
		{
			name:     "StripsANSI",
			input:    "\x1b[1;31mBold red\x1b[0m normal",
			wantText: "Bold red normal",
		},
	})
}

func TestGetNormalizer(t *testing.T) {
	cases := []struct {
		agent    string
		input    string
		wantText string
		wantType string
	}{
		{
			agent:    "claude-code",
			input:    `{"type":"assistant","message":{"content":"hi"}}`,
			wantText: "hi",
			wantType: "text",
		},
		{
			agent:    "opencode",
			input:    `{"name":"read","arguments":{}}`,
			wantText: "[Tool call]",
			wantType: "tool",
		},
		{
			agent:    "codex",
			input:    `{"type":"item.completed","item":{"type":"agent_message","text":"hello"}}`,
			wantText: "hello",
			wantType: "text",
		},
		{
			agent:    "gemini",
			input:    `{"type":"assistant","message":{"content":"hi"}}`,
			wantText: `{"type":"assistant","message":{"content":"hi"}}`,
		},
		{
			agent:    "unknown",
			input:    `{"type":"assistant","message":{"content":"hi"}}`,
			wantText: `{"type":"assistant","message":{"content":"hi"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			fn := GetNormalizer(tc.agent)
			require.NotNil(t, fn, "GetNormalizer(%q) returned nil", tc.agent)

			result := fn(tc.input)
			require.NotNil(t, result, "Normalizer for %q returned nil for input %q", tc.agent, tc.input)

			assert.Equal(t, tc.wantText, result.Text,
				"Normalizer for %q: got text %q, want %q", tc.agent, result.Text, tc.wantText)
			if tc.wantType != "" {
				assert.Equal(t, tc.wantType, result.Type, "Normalizer for %q: got type %q, want %q", tc.agent, result.Type, tc.wantType)
			}
		})
	}
}

func TestNormalizeCodexOutput(t *testing.T) {
	runNormalizeTests(t, NormalizeCodexOutput, []normalizeTestCase{
		{
			name:     "AgentMessage",
			input:    `{"type":"item.completed","item":{"type":"agent_message","text":"Review complete."}}`,
			wantText: "Review complete.",
			wantType: "text",
		},
		{
			name:     "AgentMessageMultiline",
			input:    `{"type":"item.completed","item":{"type":"agent_message","text":"Line 1\nLine 2"}}`,
			wantText: "Line 1 Line 2",
			wantType: "text",
		},
		{
			name:     "AgentMessageStripsANSI",
			input:    `{"type":"item.completed","item":{"type":"agent_message","text":"\u001b[31mred\u001b[0m text"}}`,
			wantText: "red text",
			wantType: "text",
		},
		{
			name:     "AgentMessageStripsControlChars",
			input:    `{"type":"item.completed","item":{"type":"agent_message","text":"hello\u0007world"}}`,
			wantText: "helloworld",
			wantType: "text",
		},
		{
			name:     "CommandStartedStripsANSI",
			input:    `{"type":"item.started","item":{"type":"command_execution","command":"bash -lc \u001b[32mls\u001b[0m"}}`,
			wantText: "[Command: bash -lc ls]",
			wantType: "tool",
		},
		{
			name:     "CommandCompletedStripsControlChars",
			input:    `{"type":"item.completed","item":{"type":"command_execution","command":"ls\u0007\u001b[31m -la"}}`,
			wantText: "[Command: ls -la]",
			wantType: "tool",
		},
		{
			name:     "CommandStarted",
			input:    `{"type":"item.started","item":{"type":"command_execution","command":"bash -lc ls"}}`,
			wantText: "[Command: bash -lc ls]",
			wantType: "tool",
		},
		{
			name:     "CommandCompleted",
			input:    `{"type":"item.completed","item":{"type":"command_execution","command":"bash -lc ls"}}`,
			wantText: "[Command: bash -lc ls]",
			wantType: "tool",
		},
		{
			name:     "CommandCompletedNoCommand",
			input:    `{"type":"item.completed","item":{"type":"command_execution"}}`,
			wantText: "[Command completed]",
			wantType: "tool",
		},
		{
			name:    "CommandUpdatedNoCommand",
			input:   `{"type":"item.updated","item":{"type":"command_execution"}}`,
			wantNil: true,
		},
		{
			name:    "CommandUpdatedWithCommand",
			input:   `{"type":"item.updated","item":{"type":"command_execution","command":"bash -lc ls"}}`,
			wantNil: true,
		},
		{
			name:    "FileChangeUpdated",
			input:   `{"type":"item.updated","item":{"type":"file_change"}}`,
			wantNil: true,
		},
		{
			name:     "FileChange",
			input:    `{"type":"item.completed","item":{"type":"file_change"}}`,
			wantText: "[File change]",
			wantType: "tool",
		},
		{
			name:    "ThreadStarted",
			input:   `{"type":"thread.started","thread_id":"abc123"}`,
			wantNil: true,
		},
		{
			name:    "TurnStarted",
			input:   `{"type":"turn.started"}`,
			wantNil: true,
		},
		{
			name:    "TurnCompleted",
			input:   `{"type":"turn.completed","usage":{"input_tokens":100}}`,
			wantNil: true,
		},
		{
			name:     "TurnFailed",
			input:    `{"type":"turn.failed","error":{"message":"something broke"}}`,
			wantText: "[Error in stream]",
			wantType: "error",
		},
		{
			name:     "StreamError",
			input:    `{"type":"error","message":"stream error"}`,
			wantText: "[Error in stream]",
			wantType: "error",
		},
		{
			name:    "EmptyLine",
			input:   "",
			wantNil: true,
		},
		{
			name:     "NonJSON",
			input:    "some plain text output",
			wantText: "some plain text output",
			wantType: "text",
		},
		{
			name:     "NonJSONStripsControlChars",
			input:    "text with \x07bell and \x1b[31mcolor\x1b[0m",
			wantText: "text with bell and color",
			wantType: "text",
		},
	})
}

func TestStripANSI(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"plain text", "plain text"},
		{"\x1b[32mgreen\x1b[0m", "green"},
		{"\x1b[1;31;40mcomplex\x1b[0m", "complex"},
		{"\x1b[?25hcursor\x1b[?25l", "cursor"},
		{"no escape", "no escape"},
		{"\x1b]0;title\x07text", "text"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			result := stripANSI(tc.input)
			assert.Equal(t, tc.expected, result,
				"stripANSI(%q) = %q, want %q", tc.input, result, tc.expected)
		})
	}
}

func TestIsToolCallJSON(t *testing.T) {
	cases := []struct {
		input    string
		expected bool
	}{
		{`{"name":"read","arguments":{}}`, true},
		{`{"name":"write","arguments":{"path":"/foo"}}`, true},
		{`{"name":"test","arguments":{},"extra":true}`, false},
		{`{"name":"only"}`, false},
		{`{"arguments":{}}`, false},
		{`not json`, false},
		{`{"other":"object"}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			result := isToolCallJSON(tc.input)
			assert.Equal(t, tc.expected, result,
				"isToolCallJSON(%q) = %v, want %v", tc.input, result, tc.expected)
		})
	}
}
