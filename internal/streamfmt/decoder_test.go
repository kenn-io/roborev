package streamfmt

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// If decoder selection becomes implicit, provider-shaped JSON from the wrong
// agent can be rendered as trusted stream output instead of being suppressed.
func TestExplicitDecoderDoesNotInterpretAnotherProvider(t *testing.T) {
	var out bytes.Buffer
	fmtr := New(&out, true, DecoderForAgent("codex"))
	_, err := fmtr.Write([]byte(`{"type":"text","data":"wrong provider"}` + "\n"))
	require.NoError(t, err)
	fmtr.Flush()
	assert.NotContains(t, StripANSI(out.String()), "wrong provider")
}

// If unknown agents inherit schema detection, future protocol data can be
// silently reinterpreted instead of remaining visible for diagnosis.
func TestUnknownAgentRendersJSONLiterally(t *testing.T) {
	var out bytes.Buffer
	line := `{"type":"text","data":"unknown protocol"}`
	fmtr := New(&out, true, DecoderForAgent("future-agent"))
	_, err := fmtr.Write([]byte(line + "\n"))
	require.NoError(t, err)
	fmtr.Flush()
	assert.Contains(t, StripANSI(out.String()), line)
}

// If unknown-agent JSON is rendered as Markdown, diagnostic delimiters are
// removed even though no provider protocol was selected.
func TestUnknownAgentPreservesMarkdownJSONLiterally(t *testing.T) {
	var out bytes.Buffer
	line := `{"type":"text","data":"**unknown protocol**"}`
	fmtr := New(&out, true, DecoderForAgent("future-agent"))

	_, err := fmtr.Write([]byte(line + "\n"))
	require.NoError(t, err)
	fmtr.Flush()
	assert.Equal(t, line+"\n", StripANSI(out.String()))
}

// If neutral reasoning rendering preserves embedded newlines, moving provider
// decoding out of Formatter makes completed summaries expand from one compact
// row into several terminal rows.
func TestCodexReasoningRemainsOneLine(t *testing.T) {
	var out bytes.Buffer
	fmtr := New(&out, true, DecoderForAgent("codex"))
	line := `{"type":"item.completed","item":{"type":"reasoning","text":"first line\nsecond line"}}`

	_, err := fmtr.Write([]byte(line + "\n"))
	require.NoError(t, err)
	fmtr.Flush()

	assert.Equal(t, "first line second line\n", StripANSI(out.String()))
}

// If mixed-log compatibility is removed, an archived auto-design log can lose
// either its classifier output or its appended design output.
func TestLegacyMixedDecoderRendersAppendedProviders(t *testing.T) {
	var out bytes.Buffer
	fmtr := New(&out, true, LegacyMixedDecoder("grok"))
	input := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"classifier"}}`,
		`{"type":"text","data":"design"}`,
		`{"type":"end"}`,
	}, "\n") + "\n"
	require.NoError(t, RenderLogWith(strings.NewReader(input), fmtr))
	plain := StripANSI(out.String())
	assert.Contains(t, plain, "classifier")
	assert.Contains(t, plain, "design")
}

// If compatible agent aliases stop selecting the same protocol decoder,
// their valid provider streams disappear from formatted output.
func TestDecoderAliasesRenderCompatibleProviders(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		line  string
		want  string
	}{
		{
			name:  "ClaudeAlias",
			agent: "claude",
			line:  eventAssistantText("claude alias output"),
			want:  "claude alias output",
		},
		{
			name:  "Cursor",
			agent: "cursor",
			line:  eventAssistantText("cursor output"),
			want:  "cursor output",
		},
		{
			name:  "Kilo",
			agent: "kilo",
			line: eventOpenCode("text", openCodePart{
				Type: "text", Text: "kilo output",
			}),
			want: "kilo output",
		},
		{
			name:  "GrokBuild",
			agent: "grok-build",
			line:  `{"type":"text","data":"grok-build output"}`,
			want:  "grok-build output",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			fmtr := New(&out, true, DecoderForAgent(tt.agent))
			_, err := fmtr.Write([]byte(tt.line + "\n"))
			require.NoError(t, err)
			fmtr.Flush()
			assert.Contains(t, StripANSI(out.String()), tt.want)
		})
	}
}

// If decoder factories reuse mutable protocol state, one formatter can
// suppress a tool event that belongs to a separate run.
func TestDecoderForAgentReturnsFreshState(t *testing.T) {
	line := eventOpenCode("tool", openCodePart{
		Type: "tool",
		Tool: "Read",
		ID:   "shared-id",
		State: &openCodeState{
			Status: "running",
			Input:  filePathInput("fresh.go"),
		},
	})

	var first bytes.Buffer
	firstFormatter := New(&first, true, DecoderForAgent("opencode"))
	_, err := firstFormatter.Write([]byte(line + "\n"))
	require.NoError(t, err)

	var second bytes.Buffer
	secondFormatter := New(&second, true, DecoderForAgent("opencode"))
	_, err = secondFormatter.Write([]byte(line + "\n"))
	require.NoError(t, err)

	assert.Contains(t, StripANSI(first.String()), "Read   fresh.go")
	assert.Contains(t, StripANSI(second.String()), "Read   fresh.go")
}
