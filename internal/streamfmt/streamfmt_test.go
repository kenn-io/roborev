package streamfmt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"charm.land/glamour/v2/styles"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// streamFormatterFixture wraps the buffer and formatter setup
// common to most tests.
type streamFormatterFixture struct {
	buf bytes.Buffer
	f   *Formatter
}

func newFixture(tty bool) *streamFormatterFixture {
	fix := &streamFormatterFixture{}
	fix.f = New(&fix.buf, tty)
	return fix
}

func (fix *streamFormatterFixture) writeLine(s string) {
	_, _ = fix.f.Write([]byte(s + "\n"))
}

func (fix *streamFormatterFixture) output() string {
	return StripANSI(fix.buf.String())
}

func (fix *streamFormatterFixture) assertContains(
	t *testing.T, substr string,
) {
	t.Helper()
	assert.Contains(t, fix.output(), substr,
		"expected output to contain %q, got:\n%s",
		substr, fix.output())
}

func (fix *streamFormatterFixture) assertNotContains(
	t *testing.T, substr string,
) {
	t.Helper()
	assert.NotContains(t, fix.output(), substr,
		"expected output NOT to contain %q, got:\n%s",
		substr, fix.output())
}

func (fix *streamFormatterFixture) assertEmpty(t *testing.T) {
	t.Helper()
	assert.Empty(t, fix.output(), "expected empty output")
}

func (fix *streamFormatterFixture) assertCount(
	t *testing.T, substr string, want int,
) {
	t.Helper()
	got := strings.Count(fix.output(), substr)
	assert.Equal(t, want, got,
		"expected output to contain %q %d time(s), got %d:\n%s",
		substr, want, got, fix.output())
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("toJSON: %v", err))
	}
	return string(b)
}

type streamTestCase struct {
	name        string
	events      []string
	contains    []string
	notContains []string
	counts      map[string]int
	empty       bool
	checkOutput func(*testing.T, *streamFormatterFixture)
}

func runStreamTestCase(t *testing.T, tc streamTestCase) {
	t.Run(tc.name, func(t *testing.T) {
		fix := newFixture(true)
		for _, event := range tc.events {
			fix.writeLine(event)
		}

		if tc.empty {
			fix.assertEmpty(t)
		}
		for _, s := range tc.contains {
			fix.assertContains(t, s)
		}
		for _, s := range tc.notContains {
			fix.assertNotContains(t, s)
		}
		for s, count := range tc.counts {
			fix.assertCount(t, s, count)
		}
		if tc.checkOutput != nil {
			tc.checkOutput(t, fix)
		}
	})
}

func TestFormatterTTYRendersPlainText(t *testing.T) {
	fix := newFixture(true)

	fix.writeLine("No issues found.")
	fix.writeLine("Summary: Updates docs.")

	fix.assertContains(t, "No issues found.")
	fix.assertContains(t, "Summary: Updates docs.")
}

type toolInput struct {
	FilePath  string `json:"file_path,omitempty"`
	OldString string `json:"old_string,omitempty"`
	NewString string `json:"new_string,omitempty"`
	Command   string `json:"command,omitempty"`
	Pattern   string `json:"pattern,omitempty"`
	Path      string `json:"path,omitempty"`
}

func filePathInput(path string) *toolInput {
	return &toolInput{FilePath: path}
}

func editInput(
	path, oldString, newString string,
) *toolInput {
	return &toolInput{
		FilePath:  path,
		OldString: oldString,
		NewString: newString,
	}
}

func commandInput(command string) *toolInput {
	return &toolInput{Command: command}
}

func grepInput(pattern, path string) *toolInput {
	return &toolInput{Pattern: pattern, Path: path}
}

type anthropicContentBlock struct {
	Type  string     `json:"type"`
	Text  string     `json:"text,omitempty"`
	Name  string     `json:"name,omitempty"`
	Input *toolInput `json:"input,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role,omitempty"`
	Content any    `json:"content"`
}

type anthropicToolUseResult struct {
	FilePath string `json:"filePath"`
}

type anthropicEvent struct {
	Type          string                  `json:"type"`
	Message       *anthropicMessage       `json:"message,omitempty"`
	ToolUseResult *anthropicToolUseResult `json:"tool_use_result,omitempty"`
	Result        string                  `json:"result,omitempty"`
}

func eventAssistantToolUse(
	toolName string, input *toolInput,
) string {
	return toJSON(anthropicEvent{
		Type: "assistant",
		Message: &anthropicMessage{
			Content: []anthropicContentBlock{{
				Type:  "tool_use",
				Name:  toolName,
				Input: input,
			}},
		},
	})
}

func eventAssistantText(text string) string {
	return toJSON(anthropicEvent{
		Type: "assistant",
		Message: &anthropicMessage{
			Content: []anthropicContentBlock{{
				Type: "text",
				Text: text,
			}},
		},
	})
}

func eventAssistantMulti(
	blocks ...anthropicContentBlock,
) string {
	return toJSON(anthropicEvent{
		Type:    "assistant",
		Message: &anthropicMessage{Content: blocks},
	})
}

func contentBlockText(text string) anthropicContentBlock {
	return anthropicContentBlock{Type: "text", Text: text}
}

func contentBlockToolUse(
	toolName string, input *toolInput,
) anthropicContentBlock {
	return anthropicContentBlock{
		Type: "tool_use", Name: toolName, Input: input,
	}
}

func eventAssistantLegacy(content string) string {
	return toJSON(anthropicEvent{
		Type:    "assistant",
		Message: &anthropicMessage{Content: content},
	})
}

func eventAssistantToolUseResult(filePath string) string {
	return toJSON(anthropicEvent{
		Type: "user",
		ToolUseResult: &anthropicToolUseResult{
			FilePath: filePath,
		},
	})
}

func eventAssistantResult(result string) string {
	return toJSON(anthropicEvent{
		Type: "result", Result: result,
	})
}

type geminiInitEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
}

type geminiMessageEvent struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content string `json:"content"`
	Delta   bool   `json:"delta,omitempty"`
}

type geminiToolUseEvent struct {
	Type       string     `json:"type"`
	ToolName   string     `json:"tool_name"`
	ToolID     string     `json:"tool_id"`
	Parameters *toolInput `json:"parameters"`
}

type geminiToolResultEvent struct {
	Type   string `json:"type"`
	ToolID string `json:"tool_id"`
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
}

type geminiResultEvent struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

func eventGeminiInit(sessionID string) string {
	return toJSON(geminiInitEvent{
		Type: "init", SessionID: sessionID,
	})
}

func eventGeminiMessage(
	role, content string, delta bool,
) string {
	return toJSON(geminiMessageEvent{
		Type:    "message",
		Role:    role,
		Content: content,
		Delta:   delta,
	})
}

func eventGeminiToolUse(
	toolName, toolID string, params *toolInput,
) string {
	return toJSON(geminiToolUseEvent{
		Type:       "tool_use",
		ToolName:   toolName,
		ToolID:     toolID,
		Parameters: params,
	})
}

func eventGeminiToolResult(
	toolID, status, output string,
) string {
	return toJSON(geminiToolResultEvent{
		Type:   "tool_result",
		ToolID: toolID,
		Status: status,
		Output: output,
	})
}

func eventGeminiResult(status string) string {
	return toJSON(geminiResultEvent{
		Type: "result", Status: status,
	})
}

type openCodeState struct {
	Status string `json:"status"`
	Input  any    `json:"input,omitempty"`
}

type openCodePart struct {
	Type   string         `json:"type"`
	Text   string         `json:"text,omitempty"`
	Tool   string         `json:"tool,omitempty"`
	ID     string         `json:"id,omitempty"`
	State  *openCodeState `json:"state,omitempty"`
	Reason string         `json:"reason,omitempty"`
	Cost   float64        `json:"cost,omitempty"`
}

type openCodeEvent struct {
	Type string       `json:"type"`
	Part openCodePart `json:"part"`
}

func eventOpenCode(
	eventType string, part openCodePart,
) string {
	return toJSON(openCodeEvent{
		Type: eventType,
		Part: part,
	})
}

type piTestAssistantMessageEvent struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	Delta   string `json:"delta,omitempty"`
}

type piEvent struct {
	Type                  string                       `json:"type"`
	AssistantMessageEvent *piTestAssistantMessageEvent `json:"assistantMessageEvent,omitempty"`
	Message               *anthropicMessage            `json:"message,omitempty"`
	ToolCallID            string                       `json:"toolCallId,omitempty"`
	ToolName              string                       `json:"toolName,omitempty"`
	Args                  any                          `json:"args,omitempty"`
	PartialResult         string                       `json:"partialResult,omitempty"`
	Result                string                       `json:"result,omitempty"`
	IsError               bool                         `json:"isError,omitempty"`
}

func eventPiMessageUpdate(eventType, content, delta string) string {
	return toJSON(piEvent{
		Type: "message_update",
		AssistantMessageEvent: &piTestAssistantMessageEvent{
			Type:    eventType,
			Content: content,
			Delta:   delta,
		},
	})
}

func eventPiMessageEnd(role string, content any) string {
	return toJSON(piEvent{
		Type: "message_end",
		Message: &anthropicMessage{
			Role:    role,
			Content: content,
		},
	})
}

func eventPiToolExecution(
	eventType, callID, toolName string, args any,
) string {
	return toJSON(piEvent{
		Type:       eventType,
		ToolCallID: callID,
		ToolName:   toolName,
		Args:       args,
	})
}

func TestFormatter_NonTTY(t *testing.T) {
	fix := newFixture(false)
	raw := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`
	fix.writeLine(raw)
	require.Equal(t, raw+"\n", fix.output(),
		"non-TTY should pass through raw")
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestFormatter_WriteError(t *testing.T) {
	f := New(errWriter{}, true)

	line := eventAssistantText("hello") + "\n"
	_, err := f.Write([]byte(line))
	require.Error(t, err, "expected write error to propagate")
	require.ErrorIs(t, err, io.ErrClosedPipe, "expected ErrClosedPipe")
}

func TestFormatter_PartialWrites(t *testing.T) {
	fix := newFixture(true)

	full := eventAssistantText("hello") + "\n"
	_, _ = fix.f.Write([]byte(full[:20]))
	require.Equal(t, 0, fix.buf.Len(),
		"partial write should buffer")
	_, _ = fix.f.Write([]byte(full[20:]))
	fix.assertContains(t, "hello")
}

func TestFormatter_Anthropic(t *testing.T) {
	tests := []streamTestCase{
		{
			name: "ToolUse",
			events: []string{
				eventAssistantToolUse("Read", filePathInput("internal/gmail/ratelimit.go")),
				eventAssistantToolUseResult("internal/gmail/ratelimit.go"),
				eventAssistantToolUse("Edit", editInput(
					"internal/gmail/ratelimit.go", "foo", "bar",
				)),
				eventAssistantToolUse("Bash", commandInput(
					"go test ./internal/gmail/ -run TestRateLimiter",
				)),
			},
			contains: []string{
				"Read   internal/gmail/ratelimit.go",
				"Edit   internal/gmail/ratelimit.go",
				"Bash   go test ./internal/gmail/ -run TestRateLimiter",
			},
			notContains: []string{
				"tool_use_result",
				"filePath",
			},
		},
		{
			name: "TextOutput",
			events: []string{
				eventAssistantText("I'll fix this now."),
			},
			contains: []string{"I'll fix this now."},
		},
		{
			name: "ResultSuppressed",
			events: []string{
				eventAssistantResult("final summary text"),
			},
			empty: true,
		},
		{
			name: "BashTruncation",
			events: []string{
				eventAssistantToolUse("Bash", commandInput(
					strings.Repeat("x", 100),
				)),
			},
			checkOutput: func(t *testing.T, fix *streamFormatterFixture) {
				got := fix.output()
				if len(got) > 100 {
					assert.Contains(t, got, "...", "long bash command should be truncated")
				}
			},
		},
		{
			name: "GrepWithPath",
			events: []string{
				eventAssistantToolUse("Grep", grepInput(
					"TODO", "internal/",
				)),
			},
			contains: []string{"Grep   TODO  internal/"},
		},
		{
			name: "LegacyStringContent",
			events: []string{
				eventAssistantLegacy("legacy string content"),
			},
			contains: []string{"legacy string content"},
		},
		{
			name: "MultipleContentBlocks",
			events: []string{
				eventAssistantMulti(
					contentBlockText("thinking..."),
					contentBlockToolUse("Read", filePathInput("main.go")),
				),
			},
			contains: []string{
				"thinking...",
				"Read   main.go",
			},
		},
		{
			name: "TextToToolTransition",
			events: []string{
				eventAssistantText("Reviewing code."),
				eventAssistantToolUse("Read", filePathInput("main.go")),
				eventAssistantToolUse("Edit", editInput(
					"main.go", "old", "new",
				)),
				eventAssistantText("Done fixing."),
			},
			contains: []string{
				"Read   main.go",
				"Edit   main.go",
				"Done fixing.",
			},
			checkOutput: func(t *testing.T, fix *streamFormatterFixture) {
				out := fix.output()
				lines := strings.Split(out, "\n")
				var emptyCount int
				for _, l := range lines {
					if strings.TrimSpace(l) == "" {
						emptyCount++
					}
				}
				require.GreaterOrEqual(t, emptyCount, 2,
					"expected at least 2 blank lines for transitions, got %d in:\n%s",
					emptyCount, out)
				for _, l := range lines {
					if strings.Contains(l, "Read") && strings.Contains(l, "main.go") {
						assert.True(t, strings.Contains(l, "|") || strings.Contains(l, "│"),
							"tool line should have gutter prefix, got: %q", l)
					}
				}
			},
		},
		{
			name: "SanitizesAssistantText",
			events: []string{
				eventAssistantText("\x1b[31mred\x1b[0m normal \x1b]0;evil\x07"),
			},
			contains:    []string{"red", "normal"},
			notContains: []string{"evil"},
			checkOutput: func(t *testing.T, fix *streamFormatterFixture) {
				raw := fix.buf.String()
				assert.NotContains(t, raw, "\x1b]0;", "OSC title escape leaked")
				assert.NotContains(t, raw, "\x07", "BEL char leaked")
				assert.NotContains(t, raw, "\x1b[31m", "injected SGR escape leaked")
			},
		},
		{
			name: "SanitizesToolArgs",
			events: []string{
				eventAssistantToolUse("Read", filePathInput(
					"/tmp/\x1b]0;evil\x07\x1b[31mred\x1b[0m.go",
				)),
			},
			contains: []string{"Read", ".go"},
			checkOutput: func(t *testing.T, fix *streamFormatterFixture) {
				raw := fix.buf.String()
				assert.NotContains(t, raw, "\x1b]0;", "OSC escape leaked in tool arg")
				assert.NotContains(t, raw, "\x07", "BEL char leaked in tool arg")
				assert.NotContains(t, raw, "\x1b[31m", "injected SGR escape leaked in tool arg")
			},
		},
		{
			name: "SanitizesToolName",
			events: []string{
				eventAssistantToolUse(
					"Read\x1b[31m", filePathInput("clean.go"),
				),
			},
			contains: []string{"clean.go"},
			checkOutput: func(t *testing.T, fix *streamFormatterFixture) {
				raw := fix.buf.String()
				assert.NotContains(t, raw, "\x1b[31m", "injected SGR in tool name leaked")
			},
		},
	}

	for _, tc := range tests {
		runStreamTestCase(t, tc)
	}
}

func TestFormatter_Gemini(t *testing.T) {
	tests := []streamTestCase{
		{
			name: "ToolUse",
			events: []string{
				eventGeminiInit("abc"),
				eventGeminiMessage("user", "fix this", false),
				eventGeminiMessage("assistant", "I'll fix this.", true),
				eventGeminiToolUse("read_file", "t1", filePathInput("main.go")),
				eventGeminiToolResult("t1", "success", ""),
				eventGeminiToolUse("replace", "t2", editInput(
					"main.go", "foo", "bar",
				)),
				eventGeminiToolUse("run_shell_command", "t3", commandInput("go test ./...")),
				eventGeminiResult("success"),
			},
			contains: []string{
				"I'll fix this.",
				"Read   main.go",
				"Edit   main.go",
				"Bash   go test ./...",
			},
			notContains: []string{
				"session_id",
				"tool_id",
				"status",
			},
		},
		{
			// Exercises alias-table entries not covered by "ToolUse":
			// write_file, grep, glob, list_dir. list_dir specifically
			// takes a directory `path`, not a `pattern`, so it must
			// render through the List display path.
			name: "AliasCoverage",
			events: []string{
				eventGeminiToolUse("write_file", "t1",
					&toolInput{FilePath: "out.txt"}),
				eventGeminiToolUse("grep", "t2",
					&toolInput{Pattern: "TODO", Path: "internal/"}),
				eventGeminiToolUse("glob", "t3",
					&toolInput{Pattern: "**/*.go"}),
				eventGeminiToolUse("list_dir", "t4",
					&toolInput{Path: "internal/streamfmt"}),
			},
			contains: []string{
				"Write  out.txt",
				"Grep   TODO  internal/",
				"Glob   **/*.go",
				"List   internal/streamfmt",
			},
		},
		{
			name: "ToolResultSuppressed",
			events: []string{
				eventGeminiToolResult("t1", "success", "file contents here"),
			},
			empty: true,
		},
		{
			name: "SanitizesGeminiText",
			events: []string{
				toJSON(geminiMessageEvent{
					Type:    "message",
					Role:    "assistant",
					Content: "\x1b[1mbold\x1b[0m safe \x1b]0;title\x07",
				}),
			},
			contains:    []string{"bold", "safe"},
			notContains: []string{"title"},
			checkOutput: func(t *testing.T, fix *streamFormatterFixture) {
				raw := fix.buf.String()
				assert.NotContains(t, raw, "\x1b]0;", "OSC escape leaked in Gemini text")
				assert.NotContains(t, raw, "\x1b[1m", "injected bold escape leaked")
			},
		},
	}

	for _, tc := range tests {
		runStreamTestCase(t, tc)
	}
}

func TestFormatter_OpenCode(t *testing.T) {
	tests := []streamTestCase{
		{
			name: "Text",
			events: []string{
				eventOpenCode("text", openCodePart{
					Type: "text",
					Text: "I found a bug in the code.",
				}),
			},
			contains: []string{"I found a bug in the code."},
		},
		{
			name: "Tool",
			events: []string{
				eventOpenCode("tool", openCodePart{
					Type: "tool",
					Tool: "Read",
					ID:   "tc_1",
					State: &openCodeState{
						Status: "running",
						Input:  filePathInput("internal/server.go"),
					},
				}),
				eventOpenCode("tool", openCodePart{
					Type: "tool",
					Tool: "Bash",
					ID:   "tc_2",
					State: &openCodeState{
						Status: "running",
						Input:  commandInput("go test ./..."),
					},
				}),
				eventOpenCode("tool", openCodePart{
					Type: "tool",
					Tool: "Grep",
					ID:   "tc_3",
					State: &openCodeState{
						Status: "running",
						Input:  grepInput("TODO", "internal/"),
					},
				}),
			},
			contains: []string{
				"Read   internal/server.go",
				"Bash   go test ./...",
				"Grep   TODO  internal/",
			},
		},
		{
			name: "ToolDedup",
			events: []string{
				eventOpenCode("tool", openCodePart{
					Type: "tool", Tool: "Read", ID: "tc_1",
					State: &openCodeState{Status: "pending"},
				}),
				eventOpenCode("tool", openCodePart{
					Type: "tool", Tool: "Read", ID: "tc_1",
					State: &openCodeState{
						Status: "running",
						Input:  filePathInput("main.go"),
					},
				}),
				eventOpenCode("tool", openCodePart{
					Type: "tool", Tool: "Read", ID: "tc_1",
					State: &openCodeState{
						Status: "completed",
						Input:  filePathInput("main.go"),
					},
				}),
			},
			counts: map[string]int{"Read   main.go": 1},
		},
		{
			name: "Reasoning",
			events: []string{
				eventOpenCode("reasoning", openCodePart{
					Type: "reasoning",
					Text: "Reviewing error handling changes",
				}),
			},
			contains: []string{"Reviewing error handling changes"},
		},
		{
			name: "SuppressesLifecycle",
			events: []string{
				eventOpenCode("step_start", openCodePart{
					Type: "step-start",
				}),
				eventOpenCode("step_finish", openCodePart{
					Type:   "step-finish",
					Reason: "stop",
					Cost:   0.01,
				}),
			},
			empty: true,
		},
		{
			name: "PendingToolSuppressed",
			events: []string{
				eventOpenCode("tool", openCodePart{
					Type:  "tool",
					Tool:  "Read",
					ID:    "tc_1",
					State: &openCodeState{Status: "pending"},
				}),
			},
			empty: true,
		},
		{
			// opencode 1.4+ emits outer type "tool_use" while the
			// nested part.type stays "tool". Ensure the renderer
			// handles the newer event shape.
			name: "ToolUseOuterType",
			events: []string{
				eventOpenCode("tool_use", openCodePart{
					Type: "tool",
					Tool: "Read",
					ID:   "tc_newshape",
					State: &openCodeState{
						Status: "completed",
						Input:  filePathInput("internal/agent/opencode.go"),
					},
				}),
			},
			contains: []string{"Read   internal/agent/opencode.go"},
		},
		{
			// opencode 1.4 emits lowercase tool names and camelCase
			// input fields. Real events sampled from
			// `opencode run --format json` on 2026-04-16.
			name: "LowercaseNamesCamelCaseFields",
			events: []string{
				eventOpenCode("tool_use", openCodePart{
					Type: "tool", Tool: "read", ID: "tc_read",
					State: &openCodeState{
						Status: "completed",
						Input: map[string]any{
							"filePath": "main.go",
							"offset":   1,
							"limit":    2000,
						},
					},
				}),
				eventOpenCode("tool_use", openCodePart{
					Type: "tool", Tool: "grep", ID: "tc_grep",
					State: &openCodeState{
						Status: "completed",
						Input: map[string]any{
							"pattern": "TODO",
							"path":    "internal/",
						},
					},
				}),
				eventOpenCode("tool_use", openCodePart{
					Type: "tool", Tool: "glob", ID: "tc_glob",
					State: &openCodeState{
						Status: "completed",
						Input: map[string]any{
							"pattern": "**/*.go",
						},
					},
				}),
				eventOpenCode("tool_use", openCodePart{
					Type: "tool", Tool: "bash", ID: "tc_bash",
					State: &openCodeState{
						Status: "completed",
						Input: map[string]any{
							"command": "go test ./...",
						},
					},
				}),
				eventOpenCode("tool_use", openCodePart{
					Type: "tool", Tool: "edit", ID: "tc_edit",
					State: &openCodeState{
						Status: "completed",
						Input: map[string]any{
							"filePath":  "a.go",
							"oldString": "x",
							"newString": "y",
						},
					},
				}),
				eventOpenCode("tool_use", openCodePart{
					Type: "tool", Tool: "write", ID: "tc_write",
					State: &openCodeState{
						Status: "completed",
						Input: map[string]any{
							"filePath": "new.txt",
							"content":  "hi",
						},
					},
				}),
				eventOpenCode("tool_use", openCodePart{
					Type: "tool", Tool: "webfetch", ID: "tc_wf",
					State: &openCodeState{
						Status: "completed",
						Input: map[string]any{
							"url": "https://example.com",
						},
					},
				}),
				eventOpenCode("tool_use", openCodePart{
					Type: "tool", Tool: "list", ID: "tc_list",
					State: &openCodeState{
						Status: "completed",
						Input: map[string]any{
							"path": "internal/streamfmt",
						},
					},
				}),
			},
			contains: []string{
				"Read   main.go",
				"Grep   TODO  internal/",
				"Glob   **/*.go",
				"Bash   go test ./...",
				"Edit   a.go",
				"Write  new.txt",
				"WebFetch https://example.com",
				"List   internal/streamfmt",
			},
		},
	}

	for _, tc := range tests {
		runStreamTestCase(t, tc)
	}
}

func TestFormatter_CodexRendering(t *testing.T) {
	longCmd := "bash -lc " + strings.Repeat("x", 100)
	tests := []streamTestCase{
		{
			name: "Codex Events Lifecycle",
			events: []string{
				`{"type":"thread.started","thread_id":"abc123"}`,
				`{"type":"turn.started"}`,
				`{"type":"item.started","item":{"type":"command_execution","command":"bash -lc ls"}}`,
				`{"type":"item.updated","item":{"type":"command_execution","command":"bash -lc ls"}}`,
				`{"type":"item.completed","item":{"type":"command_execution","command":"bash -lc ls","exit_code":0}}`,
				`{"type":"item.updated","item":{"type":"file_change","changes":[{"path":"main.go","kind":"update"}]}}`,
				`{"type":"item.completed","item":{"type":"file_change","changes":[{"path":"main.go","kind":"update"}]}}`,
				`{"type":"item.updated","item":{"type":"agent_message","text":"draft"}}`,
				`{"type":"item.completed","item":{"type":"agent_message","text":"I fixed the issue."}}`,
				`{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":50}}`,
			},
			contains: []string{
				"Bash   bash -lc ls",
				"Edit",
				"I fixed the issue.",
			},
			counts: map[string]int{
				"Bash   bash -lc ls": 1,
				"Edit":               1,
			},
			notContains: []string{
				"thread_id",
				"turn.started",
				"input_tokens",
				"draft",
			},
		},
		{
			name: "Codex Updated Suppressed",
			events: []string{
				`{"type":"item.updated","item":{"type":"command_execution","command":"bash -lc ls"}}`,
				`{"type":"item.updated","item":{"type":"file_change","changes":[{"path":"main.go","kind":"update"}]}}`,
				`{"type":"item.updated","item":{"type":"agent_message","text":"still drafting"}}`,
			},
			empty: true,
		},
		{
			name: "Codex Sanitizes Control Chars",
			events: []string{
				`{"type":"item.completed","item":{"type":"agent_message","text":"\u001b[31mred\u001b[0m and \u0007bell"}}`,
				`{"type":"item.started","item":{"type":"command_execution","command":"bash -lc \u001b[32mls\u001b[0m"}}`,
			},
			contains: []string{
				"red and bell",
				"Bash   bash -lc ls",
			},
			notContains: []string{
				"\x1b",
				"\x07",
			},
		},
		{
			name: "Codex Lifecycle Suppressed",
			events: []string{
				`{"type":"thread.started","thread_id":"abc"}`,
				`{"type":"turn.started"}`,
				`{"type":"turn.completed","usage":{"input_tokens":100}}`,
			},
			empty: true,
		},
		{
			name: "Codex Command Truncation",
			events: []string{
				fmt.Sprintf(`{"type":"item.started","item":{"type":"command_execution","command":%q}}`, longCmd),
			},
			checkOutput: func(t *testing.T, fix *streamFormatterFixture) {
				output := fix.output()
				assert.Contains(t, output, "...", "long codex command should be truncated")
			},
		},
		{
			name: "Codex Reasoning Displayed",
			events: []string{
				`{"type":"item.completed","item":{"type":"reasoning","text":"**Reviewing error handling changes**"}}`,
			},
			contains: []string{"Reviewing error handling changes"},
		},
		{
			name: "Codex Reasoning Suppressed On Non-Completed",
			events: []string{
				`{"type":"item.started","item":{"type":"reasoning","text":"draft thinking"}}`,
				`{"type":"item.updated","item":{"type":"reasoning","text":"still thinking"}}`,
			},
			empty: true,
		},
	}

	for _, tc := range tests {
		runStreamTestCase(t, tc)
	}
}

func TestFormatter_PiRendering(t *testing.T) {
	tests := []streamTestCase{
		{
			name: "ToolCallsAndFinalText",
			events: []string{
				`{"type":"session","id":"pi-session-123"}`,
				`{"type":"agent_start"}`,
				`{"type":"turn_start"}`,
				eventPiMessageUpdate("thinking_delta", "", "checking repo"),
				eventPiToolExecution(
					"tool_execution_start",
					"tool-1",
					"bash",
					map[string]string{"command": "go test ./..."},
				),
				eventPiToolExecution(
					"tool_execution_update",
					"tool-1",
					"bash",
					map[string]string{"command": "go test ./..."},
				),
				eventPiToolExecution(
					"tool_execution_end",
					"tool-1",
					"bash",
					map[string]string{"command": "go test ./..."},
				),
				eventPiToolExecution(
					"tool_execution_start",
					"tool-2",
					"read",
					map[string]string{"path": "internal/streamfmt/streamfmt.go"},
				),
				eventPiMessageUpdate("text_delta", "", "draft"),
				eventPiMessageUpdate("text_end", "Final review.\n\nNo issues found.", ""),
				eventPiMessageEnd("assistant", []anthropicContentBlock{{
					Type: "text",
					Text: "Final review.\n\nNo issues found.",
				}}),
				`{"type":"turn_end"}`,
				`{"type":"agent_end"}`,
			},
			contains: []string{
				"Bash   go test ./...",
				"Read   internal/streamfmt/streamfmt.go",
				"Final review.",
				"No issues found.",
			},
			counts: map[string]int{
				"Bash   go test ./...": 1,
				"Final review.":        1,
				"No issues found.":     1,
			},
			notContains: []string{
				"checking repo",
				"draft",
				"tool_execution_update",
				"tool_execution_end",
				"pi-session-123",
			},
		},
		{
			name: "LifecycleSuppressed",
			events: []string{
				`{"type":"session","id":"pi-session-123"}`,
				`{"type":"agent_start"}`,
				`{"type":"turn_start"}`,
				`{"type":"turn_end"}`,
				`{"type":"agent_end"}`,
			},
			empty: true,
		},
		{
			name: "AssistantMessageEndText",
			events: []string{
				eventPiMessageEnd("user", []anthropicContentBlock{{
					Type: "text",
					Text: "prompt echo",
				}}),
				eventPiMessageEnd("assistant", []anthropicContentBlock{{
					Type: "text",
					Text: "Final review from message_end.",
				}}),
			},
			contains: []string{
				"Final review from message_end.",
			},
			notContains: []string{
				"prompt echo",
			},
		},
	}

	for _, tc := range tests {
		runStreamTestCase(t, tc)
	}
}

func TestSanitizeControl(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "hello world", "hello world"},
		{"newlines collapsed", "line1\nline2\r\nline3", "line1 line2 line3"},
		{"ansi stripped", "\x1b[31mred\x1b[0m text", "red text"},
		{"control chars removed", "bell\x07here", "bellhere"},
		{"tabs kept", "col1\tcol2", "col1\tcol2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeControl(tt.input)
			require.Equal(t, tt.want, got,
				"sanitizeControl(%q) = %q, want %q",
				tt.input, got, tt.want)
		})
	}
}

func TestSanitizeControlKeepNewlines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "hello", "hello"},
		{"newlines preserved", "line1\nline2", "line1\nline2"},
		{"crlf normalized", "line1\r\nline2\rline3", "line1\nline2\nline3"},
		{"ansi stripped", "\x1b[32mgreen\x1b[0m", "green"},
		{"control chars removed", "bell\x07here", "bellhere"},
		{"tabs kept", "col1\tcol2", "col1\tcol2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeControlKeepNewlines(tt.input)
			require.Equal(t, tt.want, got,
				"SanitizeControlKeepNewlines(%q) = %q, want %q",
				tt.input, got, tt.want)
		})
	}
}

func TestRenderLogWithWrapsStderr(t *testing.T) {
	// Non-JSON lines (stderr from agent process) should be word-wrapped
	// to the formatter's width, not passed through verbatim.
	width := 40
	longStderr := strings.Repeat("x", 80) // 80 chars, double the width

	input := strings.NewReader(longStderr + "\n")
	var buf bytes.Buffer
	fmtr := NewWithWidth(&buf, width, GlamourStyle())

	require.NoError(t, RenderLogWith(input, fmtr, &buf), "RenderLogWith failed")

	output := buf.String()
	for line := range strings.SplitSeq(strings.TrimRight(output, "\n"), "\n") {
		stripped := StripANSI(line)
		assert.LessOrEqual(t, len(stripped), width,
			"stderr line exceeds width %d: len=%d %q",
			width, len(stripped), stripped)
	}
}

func TestFormatterWidth(t *testing.T) {
	// Width() should return the configured terminal width.
	fmtr := NewWithWidth(io.Discard, 42, GlamourStyle())
	require.Equal(t, 42, fmtr.Width(), "Width() = %d, want 42", fmtr.Width())
}

func TestResolveColorProfile(t *testing.T) {
	t.Run("NO_COLOR returns Ascii", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		t.Setenv("ROBOREV_COLOR_MODE", "")
		assert.Equal(t, termenv.Ascii, ResolveColorProfile())
	})
	t.Run("ROBOREV_COLOR_MODE=none returns Ascii", func(t *testing.T) {
		t.Setenv("NO_COLOR", "")
		t.Setenv("ROBOREV_COLOR_MODE", "none")
		assert.Equal(t, termenv.Ascii, ResolveColorProfile())
	})
	t.Run("NO_COLOR takes precedence over ROBOREV_COLOR_MODE=dark", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		t.Setenv("ROBOREV_COLOR_MODE", "dark")
		assert.Equal(t, termenv.Ascii, ResolveColorProfile())
	})
	t.Run("default returns env color profile", func(t *testing.T) {
		t.Setenv("NO_COLOR", "")
		t.Setenv("ROBOREV_COLOR_MODE", "")
		// Should match what termenv detects for the current environment.
		assert.Equal(t, termenv.EnvColorProfile(), ResolveColorProfile())
	})
}

func TestRenderMarkdownLinesNoColor(t *testing.T) {
	// When colorProfile is Ascii, StripTrailingPadding removes all SGR
	// sequences (colors, bold, underline, reset) so no formatting can
	// bleed across lines.
	text := "# Heading\n\nSome **bold** text."
	style := GlamourStyle()
	lines := RenderMarkdownLines(text, 80, 80, style, 2, termenv.Ascii)

	combined := strings.Join(lines, "\n")
	allSGR := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	matches := allSGR.FindAllString(combined, -1)
	assert.Empty(t, matches, "expected no SGR sequences with Ascii profile, got: %v", matches)
}

func TestGlamourStyleRespectsColorMode(t *testing.T) {
	// Use the upstream style configs as reference values so tests don't
	// break if glamour changes its default color palette.
	darkDocColor := *styles.DarkStyleConfig.Document.Color
	lightDocColor := *styles.LightStyleConfig.Document.Color
	require.NotEqual(t, darkDocColor, lightDocColor, "dark and light Document.Color must differ for this test to be meaningful")

	t.Run("dark mode selects dark style", func(t *testing.T) {
		t.Setenv("ROBOREV_COLOR_MODE", "dark")
		t.Setenv("NO_COLOR", "")
		style := GlamourStyle()
		require.NotNil(t, style.Document.Color)
		assert.Equal(t, darkDocColor, *style.Document.Color)
	})
	t.Run("light mode selects light style", func(t *testing.T) {
		t.Setenv("ROBOREV_COLOR_MODE", "light")
		t.Setenv("NO_COLOR", "")
		style := GlamourStyle()
		require.NotNil(t, style.Document.Color)
		assert.Equal(t, lightDocColor, *style.Document.Color)
	})
	t.Run("none mode selects dark style as base", func(t *testing.T) {
		t.Setenv("ROBOREV_COLOR_MODE", "none")
		t.Setenv("NO_COLOR", "")
		style := GlamourStyle()
		require.NotNil(t, style.Document.Color)
		assert.Equal(t, darkDocColor, *style.Document.Color)
	})
	t.Run("NO_COLOR selects dark style as base", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		t.Setenv("ROBOREV_COLOR_MODE", "")
		style := GlamourStyle()
		require.NotNil(t, style.Document.Color)
		assert.Equal(t, darkDocColor, *style.Document.Color)
	})
	t.Run("NO_COLOR takes precedence over dark mode", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		t.Setenv("ROBOREV_COLOR_MODE", "dark")
		// NO_COLOR wins: profile should be Ascii, style should be dark base.
		assert.Equal(t, termenv.Ascii, ResolveColorProfile())
		style := GlamourStyle()
		require.NotNil(t, style.Document.Color)
		assert.Equal(t, darkDocColor, *style.Document.Color)
	})
}
