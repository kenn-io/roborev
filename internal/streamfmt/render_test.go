package streamfmt

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func executeRenderJobLog(
	t *testing.T, input string, isTTY bool, agent string,
) string {
	t.Helper()
	var buf bytes.Buffer
	fmtr := New(&buf, isTTY, DecoderForAgent(agent))
	require.NoError(t, RenderLogWith(
		strings.NewReader(input), fmtr,
	), "RenderLogWith")
	return buf.String()
}

func assertLogContains(t *testing.T, got, want string) {
	t.Helper()
	assert.Contains(t, got, want)
}

func assertLogNotContains(t *testing.T, got, want string) {
	t.Helper()
	assert.NotContains(t, got, want)
}

func TestRenderJobLog_JSONL(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"abc123"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Reviewing the code."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"main.go"}}]}}`,
		`{"type":"tool_result","content":"file contents"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Looks good overall."}]}}`,
	}, "\n")

	out := StripANSI(executeRenderJobLog(t, input, true, "claude-code"))

	assertLogContains(t, out, "Reviewing the code.")
	assertLogContains(t, out, "Read")
	assertLogContains(t, out, "main.go")
	assertLogNotContains(t, out, `"type"`)
}

func TestRenderJobLog_Behaviors(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		isJSON      bool
		agent       string
		wantContain []string
		wantExclude []string
		wantEmpty   bool
	}{
		{
			name:        "PlainText",
			input:       "line 1\nline 2\nline 3\n",
			isJSON:      true,
			agent:       "plain",
			wantContain: []string{"line 1", "line 3"},
		},
		{
			name:      "Empty",
			input:     "",
			isJSON:    true,
			agent:     "plain",
			wantEmpty: true,
		},
		{
			name:        "OversizedLine",
			input:       fmt.Sprintf(`{"type":"tool_result","content":"%s"}`, strings.Repeat("x", 2*1024*1024)),
			isJSON:      true,
			agent:       "claude-code",
			wantContain: []string{},
		},
		{
			name:        "PlainTextPreservesBlankLines",
			input:       "line 1\n\nline 3\n",
			isJSON:      true,
			agent:       "plain",
			wantContain: []string{"line 1\n\nline 3"},
		},
		{
			name:        "SanitizesControlChars",
			input:       "\x1b[31mred text\x1b[0m\n\x1b]0;evil title\x07\nclean line",
			isJSON:      true,
			agent:       "plain",
			wantContain: []string{"red text", "clean line"},
			wantExclude: []string{"\x1b[", "\x1b]", "\x07"},
		},
		{
			name: "SanitizeMixedJSONAndControl",
			input: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`,
				"\x1b[1mbold agent stderr\x1b[0m",
			}, "\n"),
			isJSON:      true,
			agent:       "claude-code",
			wantContain: []string{"ok", "bold agent stderr"},
			wantExclude: []string{"\x1b[1m"},
		},
		{
			name: "PreservesBlankLinesInMixedLog",
			input: strings.Join([]string{
				`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`,
				"stderr line 1",
				"",
				"stderr line 2",
			}, "\n"),
			isJSON:      true,
			agent:       "claude-code",
			wantContain: []string{"stderr line 1\n\n", "stderr line 2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := executeRenderJobLog(t, tt.input, tt.isJSON, tt.agent)
			if tt.wantEmpty {
				assert.Empty(t, out)
			}
			for _, want := range tt.wantContain {
				assertLogContains(t, out, want)
			}
			for _, exclude := range tt.wantExclude {
				assertLogNotContains(t, out, exclude)
			}
		})
	}
}

func TestRenderJobLog_MixedJSONAndPlainText(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]}}`,
		`stderr: warning something`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Done"}]}}`,
		`exit status 0`,
	}, "\n")

	out := executeRenderJobLog(t, input, false, "claude-code")

	assertLogContains(t, out, "Hello")
	assertLogContains(t, out, "stderr: warning something")
	assertLogContains(t, out, "Done")
	assertLogContains(t, out, "exit status 0")

	helloIdx := strings.Index(out, "Hello")
	stderrIdx := strings.Index(out, "stderr: warning")
	doneIdx := strings.Index(out, "Done")
	exitIdx := strings.Index(out, "exit status 0")
	assert.True(t,
		helloIdx < stderrIdx &&
			stderrIdx < doneIdx &&
			doneIdx < exitIdx,
		"output order wrong: Hello@%d stderr@%d Done@%d exit@%d",
		helloIdx, stderrIdx, doneIdx, exitIdx,
	)
}

// If non-TTY stderr is wrapped, downstream consumers receive extra record
// boundaries instead of the original piped line.
func TestRenderLogNonTTYPreservesLongStderr(t *testing.T) {
	line := strings.Repeat("x", 120)
	var out bytes.Buffer

	require.NoError(t, RenderLog(
		strings.NewReader(line+"\n"), &out, false,
	))
	assert.Equal(t, line+"\n", out.String())
}

func TestRenderJobLog_OpenCodeEvents(t *testing.T) {
	input := strings.Join([]string{
		eventOpenCode("step_start", openCodePart{Type: "step-start"}),
		eventOpenCode("text", openCodePart{
			Type: "text",
			Text: "Reviewing the code.",
		}),
		eventOpenCode("tool", openCodePart{
			Type: "tool",
			Tool: "Read",
			ID:   "tc_1",
			State: &openCodeState{
				Status: "running",
				Input:  filePathInput("main.go"),
			},
		}),
		eventOpenCode("text", openCodePart{
			Type: "text",
			Text: "Looks good overall.",
		}),
		eventOpenCode("step_finish", openCodePart{
			Type:   "step-finish",
			Reason: "stop",
		}),
	}, "\n")

	out := StripANSI(executeRenderJobLog(t, input, true, "opencode"))

	assertLogContains(t, out, "Reviewing the code.")
	assertLogContains(t, out, "Read")
	assertLogContains(t, out, "main.go")
	assertLogContains(t, out, "Looks good overall.")
	assertLogNotContains(t, out, "step_start")
	assertLogNotContains(t, out, "step_finish")
	assertLogNotContains(t, out, `"type"`)
}

func TestStripTrailingPadding(t *testing.T) {
	bold := "\x1b[1m"
	underline := "\x1b[4m"
	reset := "\x1b[0m"
	red := "\x1b[31m"

	tests := []struct {
		name    string
		line    string
		noColor bool
		want    string
	}{
		{
			name:    "color mode appends reset",
			line:    red + "hello" + reset,
			noColor: false,
			want:    red + "hello" + reset,
		},
		{
			name:    "color mode strips trailing padding",
			line:    red + "hello" + reset + "   " + reset,
			noColor: false,
			want:    red + "hello" + reset,
		},
		{
			name:    "noColor strips mid-line bold",
			line:    bold + "hello" + reset,
			noColor: true,
			want:    "hello",
		},
		{
			name:    "noColor strips mid-line underline",
			line:    underline + "text" + reset,
			noColor: true,
			want:    "text",
		},
		{
			name:    "noColor strips mixed SGR sequences",
			line:    bold + underline + "mixed" + reset + " plain",
			noColor: true,
			want:    "mixed plain",
		},
		{
			name:    "noColor preserves plain text",
			line:    "no formatting here",
			noColor: true,
			want:    "no formatting here",
		},
		{
			name:    "noColor strips trailing padding and all SGR",
			line:    bold + "word" + reset + "   ",
			noColor: true,
			want:    "word",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripTrailingPadding(tt.line, tt.noColor)
			assert.Equal(t, tt.want, got)
		})
	}
}
