package streamfmt

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatter_GrokRendering(t *testing.T) {
	// Not parallel: New() → GlamourStyle() mutates package-level termstyle state.

	t.Run("text data", func(t *testing.T) {
		fix := newFixture(true, "grok")
		fix.writeLine(`{"type":"text","data":"Hello review"}`)
		fix.writeLine(`{"type":"end","stopReason":"EndTurn"}`)
		fix.assertContains(t, "Hello review")
	})

	t.Run("thought as reasoning", func(t *testing.T) {
		fix := newFixture(true, "grok")
		fix.writeLine(`{"type":"thought","data":"considering the diff"}`)
		fix.writeLine(`{"type":"end"}`)
		fix.assertContains(t, "considering the diff")
	})

	// Official streaming-json fixtures (rawInput / title / kind), not the
	// invented "args" shape used by earlier tests.
	t.Run("read_file tool_call rawInput path", func(t *testing.T) {
		fix := newFixture(true, "grok")
		fix.writeLine(`{"type":"tool_call","toolCallId":"call_1","title":"Read","kind":"read","status":"in_progress","toolName":"read_file","rawInput":{"path":"internal/foo.go"},"content":[],"locations":[]}`)
		out := fix.output()
		assert.Contains(t, out, "Read")
		assert.Contains(t, out, "internal/foo.go")
		assert.NotContains(t, out, `"type":"tool_call"`)
		assert.NotContains(t, out, "rawInput")
	})

	t.Run("grep tool_call rawInput", func(t *testing.T) {
		fix := newFixture(true, "grok")
		fix.writeLine(`{"type":"tool_call","toolCallId":"2","title":"Grep","kind":"search","toolName":"grep","rawInput":{"pattern":"TODO","path":"."}}`)
		out := fix.output()
		assert.Contains(t, out, "Grep")
		assert.Contains(t, out, "TODO")
	})

	t.Run("list_dir tool_call rawInput", func(t *testing.T) {
		fix := newFixture(true, "grok")
		fix.writeLine(`{"type":"tool_call","toolCallId":"3","title":"List","kind":"search","toolName":"list_dir","rawInput":{"path":"cmd"}}`)
		out := fix.output()
		assert.Contains(t, out, "List")
		assert.Contains(t, out, "cmd")
	})

	t.Run("shell agentic tool_call rawInput", func(t *testing.T) {
		fix := newFixture(true, "grok")
		fix.writeLine(`{"type":"tool_call","toolCallId":"4","title":"Bash","kind":"execute","toolName":"run_terminal_cmd","rawInput":{"command":"go test ./..."}}`)
		out := fix.output()
		assert.Contains(t, out, "Bash")
		assert.Contains(t, out, "go test")
	})

	t.Run("edit agentic tool_call rawInput", func(t *testing.T) {
		fix := newFixture(true, "grok")
		fix.writeLine(`{"type":"tool_call","toolCallId":"5","title":"Edit","kind":"edit","toolName":"search_replace","rawInput":{"file_path":"a.go","old_string":"x","new_string":"y"}}`)
		out := fix.output()
		assert.Contains(t, out, "Edit")
		assert.Contains(t, out, "a.go")
	})

	t.Run("tool_call once; update without toolName does not duplicate", func(t *testing.T) {
		fix := newFixture(true, "grok")
		// Official update omits toolName and carries rawOutput.
		fix.writeLine(`{"type":"tool_call","toolCallId":"dup","title":"Read","kind":"read","toolName":"read_file","rawInput":{"path":"once.go"},"content":[],"locations":[]}`)
		fix.writeLine(`{"type":"tool_call_update","toolCallId":"dup","status":"completed","content":[],"rawOutput":{"lines":42},"locations":[]}`)
		fix.assertCount(t, "once.go", 1)
		out := fix.output()
		assert.NotContains(t, out, "rawOutput")
		assert.NotContains(t, out, `"type":"tool_call_update"`)
	})

	t.Run("failed tool update from rawOutput without toolName", func(t *testing.T) {
		fix := newFixture(true, "grok")
		fix.writeLine(`{"type":"tool_call","toolCallId":"f1","title":"Grep","kind":"search","toolName":"grep","rawInput":{"pattern":"x"}}`)
		// Update has no toolName; failure detail lives in rawOutput.
		fix.writeLine(`{"type":"tool_call_update","toolCallId":"f1","status":"failed","content":[],"rawOutput":{"error":"permission denied"}}`)
		out := fix.output()
		assert.Contains(t, out, "Grep")
		assert.Contains(t, out, "failed")
		assert.Contains(t, out, "permission denied")
		assert.NotContains(t, out, `"type":"tool_call_update"`)
		assert.NotContains(t, out, "rawOutput")
	})

	t.Run("failed update content array detail", func(t *testing.T) {
		fix := newFixture(true, "grok")
		fix.writeLine(`{"type":"tool_call","toolCallId":"f2","toolName":"read_file","rawInput":{"path":"secret.go"}}`)
		fix.writeLine(`{"type":"tool_call_update","toolCallId":"f2","status":"failed","content":[{"type":"text","text":"access denied"}]}`)
		out := fix.output()
		assert.Contains(t, out, "failed")
		assert.Contains(t, out, "access denied")
	})

	t.Run("title used when toolName absent", func(t *testing.T) {
		fix := newFixture(true, "grok")
		// Some ACP-shaped frames may only carry title/kind + rawInput.
		fix.writeLine(`{"type":"tool_call","toolCallId":"t1","title":"Read","kind":"read","rawInput":{"path":"only-title.go"}}`)
		out := fix.output()
		assert.Contains(t, out, "Read")
		assert.Contains(t, out, "only-title.go")
	})

	t.Run("plan suppressed", func(t *testing.T) {
		fix := newFixture(true, "grok")
		fix.writeLine(`{"type":"plan","data":"1. read files 2. summarize"}`)
		fix.assertEmpty(t)
	})

	t.Run("error event formats string message", func(t *testing.T) {
		fix := newFixture(true, "grok")
		// Official Grok shape: message is a JSON string, not a Claude object.
		line := `{"type":"error","message":"auth failed"}`
		fix.writeLine(line)
		out := fix.output()
		// Exactly the formatted form — not the raw NDJSON line.
		assert.Contains(t, out, "error: auth failed")
		assert.NotContains(t, out, `"type":"error"`)
		assert.NotContains(t, out, line)
		// Ensure we did not dump the whole JSON object as text.
		assert.NotContains(t, out, `{"type"`, "output leaked raw JSON: %q", out)
	})

	t.Run("error with longer official-style message", func(t *testing.T) {
		fix := newFixture(true, "grok")
		fix.writeLine(`{"type":"error","message":"Couldn't start session: not authenticated"}`)
		out := fix.output()
		assert.Contains(t, out, "error: Couldn't start session: not authenticated")
		assert.NotContains(t, out, `"type":"error"`)
	})

	t.Run("end and session suppressed", func(t *testing.T) {
		fix := newFixture(true, "grok")
		fix.writeLine(`{"type":"end","stopReason":"end_turn","sessionId":"abc"}`)
		fix.writeLine(`{"type":"session","sessionId":"abc"}`)
		fix.assertEmpty(t)
	})

	t.Run("non-TTY passthrough", func(t *testing.T) {
		fix := newFixture(false, "grok")
		line := `{"type":"text","data":"raw"}`
		fix.writeLine(line)
		assert.Contains(t, fix.buf.String(), line)
	})

	t.Run("Claude assistant still works with Message RawMessage", func(t *testing.T) {
		// Regression: Message is now RawMessage so Grok string errors decode;
		// Claude nested objects must still render.
		fix := newFixture(true, "claude-code")
		fix.writeLine(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"claude ok"}]}}`)
		fix.assertContains(t, "claude ok")
	})
}

// If incremental rendering finalizes Grok state between polls, adjacent text
// frames no longer form one Markdown document and wrap independently.
func TestGrokDecoderKeepsAdjacentTextAcrossIncrementalChunks(t *testing.T) {
	var out bytes.Buffer
	fmtr := NewWithWidth(
		&out, 24, GlamourStyle(), DecoderForAgent("grok"),
	)

	require.NoError(t, RenderLogChunkWith(
		strings.NewReader(`{"type":"text","data":"**adjacent"}`+"\n"),
		fmtr,
	))
	assert.Empty(t, out.String())
	require.NoError(t, RenderLogChunkWith(
		strings.NewReader(`{"type":"text","data":" text**"}`+"\n"),
		fmtr,
	))
	assert.Empty(t, out.String())
	require.NoError(t, RenderLogWith(
		strings.NewReader(`{"type":"end"}`+"\n"), fmtr,
	))

	plain := StripANSI(out.String())
	assert.Contains(t, plain, "adjacent text")
	assert.NotContains(t, plain, "**")
}

// If Grok thought frames are rendered independently, token-sized provider
// chunks consume one terminal row each instead of wrapping as prose.
func TestGrokThoughtChunksRenderAsWrappedProse(t *testing.T) {
	tests := []struct {
		name  string
		width int
		want  []string
	}{
		{
			name:  "wide",
			width: 80,
			want:  []string{"This synthetic reasoning flows as normal prose."},
		},
		{
			name:  "narrow",
			width: 24,
			want: []string{
				"This synthetic",
				"reasoning flows as",
				"normal prose.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			fmtr := NewWithWidth(
				&out, tt.width, GlamourStyle(), DecoderForAgent("grok"),
			)

			require.NoError(t, RenderLogChunkWith(
				strings.NewReader(strings.Join([]string{
					`{"type":"thought","data":"This synthetic"}`,
					`{"type":"thought","data":" reasoning flows"}`,
				}, "\n")+"\n"),
				fmtr,
			))
			assert.Empty(t, out.String())
			require.NoError(t, RenderLogWith(
				strings.NewReader(strings.Join([]string{
					`{"type":"thought","data":" as normal prose."}`,
					`{"type":"end"}`,
				}, "\n")+"\n"),
				fmtr,
			))

			plain := strings.TrimSuffix(StripANSI(out.String()), "\n")
			assert.Equal(t, tt.want, strings.Split(plain, "\n"))
		})
	}
}

// If Grok thought assembly normalizes whitespace, intentional paragraphs and
// structured blocks collapse into one line.
func TestGrokThoughtChunksPreserveStructuredLineBreaks(t *testing.T) {
	var out bytes.Buffer
	fmtr := NewWithWidth(
		&out, 80, GlamourStyle(), DecoderForAgent("grok"),
	)
	input := strings.Join([]string{
		`{"type":"thought","data":"Inspection notes:\n\n"}`,
		`{"type":"thought","data":"- branch A\n- branch B\n\n"}`,
		"{\"type\":\"thought\",\"data\":\"```text\\nstatus: ok\\n```\"}",
		`{"type":"end"}`,
	}, "\n") + "\n"

	require.NoError(t, RenderLogWith(strings.NewReader(input), fmtr))

	assert.Equal(t, strings.Join([]string{
		"Inspection notes:",
		"",
		"- branch A",
		"- branch B",
		"",
		"```text",
		"status: ok",
		"```",
		"",
	}, "\n"), StripANSI(out.String()))
}

func TestDecodeClaudeMessage_RejectsString(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, _, ok := decodeClaudeMessage(nil)
	assert.False(ok)

	_, _, ok = decodeClaudeMessage(jsonRaw(`"auth failed"`))
	assert.False(ok)

	role, content, ok := decodeClaudeMessage(jsonRaw(`{"role":"assistant","content":[{"type":"text","text":"hi"}]}`))
	require.True(ok)
	assert.Equal("assistant", role)
	assert.NotEmpty(content)

	assert.Equal("auth failed", jsonStringField(jsonRaw(`"auth failed"`)))
	assert.Empty(jsonStringField(jsonRaw(`{"role":"assistant"}`)))
}

func jsonRaw(s string) []byte {
	return []byte(s)
}
