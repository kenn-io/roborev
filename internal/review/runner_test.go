package review

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

func TestRunAgentReviewKeepsCustomVerdictWithRenderedOutput(t *testing.T) {
	a := &structuredBatchAgent{
		name: "structured",
		result: jsontext.Value(`{
	  "schema_version": 2,
	  "verdict": "pass",
	  "summary": "High: no actionable findings remain.",
  "findings": [
    {"severity":"low","problem":"Vague name.","fix":"Rename it.","location":null}
  ]
}`),
	}
	var streamed strings.Builder

	got, err := RunAgentReview(
		context.Background(), a, t.TempDir(), "HEAD", "prompt",
		"custom", "medium", &streamed,
	)
	require.NoError(t, err)
	require.NotNil(t, got.Structured)
	require.Len(t, got.Structured.Findings, 1)
	assert.Equal(t, "low", got.Structured.Findings[0].Severity)
	assert.JSONEq(t, string(a.result), string(got.StructuredOutput))
	assert.Equal(t, "medium", got.MinSeverity)
	assert.Contains(t, got.Output, "Vague name", "findings below the threshold stay in the output")
	assert.Contains(t, got.Output, "No findings at or above medium severity.")
	assert.Equal(t, storage.VerdictPass, got.Verdict)
	assert.Equal(t, got.Output+"\n", streamed.String())
	assert.Equal(t, storage.VerdictFail, storage.ParseVerdict(got.Output),
		"rendered prose must not replace the structured verdict")
}

func TestRunAgentReviewUsesSchemaForBuiltInTypesWhenSupported(t *testing.T) {
	a := &structuredBatchAgent{
		name: "structured", output: "PROSE FALLBACK",
		result: jsontext.Value(`{
  "schema_version": 2,
  "verdict": "pass",
  "summary": "One real bug.",
  "findings": [
    {"severity":"high","problem":"Nil deref.","fix":"Check the pointer.","location":"main.go:10"}
  ]
}`),
	}

	got, err := RunAgentReview(
		context.Background(), a, t.TempDir(), "HEAD", "prompt",
		"default", "", nil,
	)
	require.NoError(t, err)
	require.NotNil(t, got.Structured)
	assert.Equal(t, storage.VerdictFail, got.Verdict)
	assert.Contains(t, got.Output, "### 1. High")
	assert.Contains(t, got.Output, "**Location:** main.go:10")
	assert.NotContains(t, got.Output, "PROSE FALLBACK",
		"structured agents must not fall back to prose reviews")
}

func TestRunAgentReviewRequiresJSONWithoutNativeSchema(t *testing.T) {
	a := &agent.FakeAgent{NameStr: "plain", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		return "No issues found.", nil
	}}
	_, err := RunAgentReview(t.Context(), a, t.TempDir(), "HEAD", "prompt", "default", "", nil)
	require.ErrorContains(t, err, "decode structured review")
}

func TestRunAgentReviewAcceptsJSONWithoutNativeSchema(t *testing.T) {
	raw := `{"schema_version":2,"summary":"One finding.","verdict":"fail","findings":[{"severity":"low","problem":"Vague name.","fix":"Rename it.","location":null}]}`
	a := &mockAgent{name: "plain", output: raw}
	got, err := RunAgentReview(t.Context(), a, t.TempDir(), "HEAD", "prompt", "default", "medium", nil)
	require.NoError(t, err)
	require.NotNil(t, got.Structured)
	assert.JSONEq(t, raw, string(got.StructuredOutput))
	assert.Equal(t, storage.VerdictPass, got.Verdict)
}

func TestNoVerdictMessage(t *testing.T) {
	assert := assert.New(t)

	unreadable := NoVerdict("  I am unable to read the diff.\n")
	require.NotNil(t, unreadable)
	assert.Equal(storage.OutputUnreadableInput, unreadable.Kind)
	msg := NoVerdictMessage(unreadable)
	assert.True(strings.HasPrefix(msg, NoVerdictErrorPrefix))
	assert.Contains(msg, "review produced no recognizable verdict (unreadable input)")
	assert.Contains(msg, "I am unable to read the diff.")
	assert.False(strings.HasSuffix(msg, "\n"))

	empty := NoVerdict("No review output generated")
	require.NotNil(t, empty)
	assert.Equal(storage.OutputEmpty, empty.Kind)
	assert.Contains(NoVerdictMessage(empty), "(empty output)")

	assert.Nil(NoVerdict("No issues found."))
	assert.Nil(NoVerdict("The code has issues."))

	long := NoVerdictMessage(&NoVerdictError{Kind: storage.OutputUnreadableInput, Output: "cannot read the diff " + strings.Repeat("x", noVerdictOutputLimit+50)})
	assert.Contains(long, "[truncated]")
	assert.Less(len(long), noVerdictOutputLimit+120)

	assert.True(IsNoVerdictFailure(ReviewResult{Status: ResultFailed, Error: msg}))
	assert.False(IsNoVerdictFailure(ReviewResult{Status: ResultFailed, Error: "agent: boom"}))
}

func TestRunAgentReviewTreatsUnableToReviewAsFailure(t *testing.T) {
	a := &structuredBatchAgent{
		name: "structured",
		result: jsontext.Value(`{
  "schema_version": 2,
  "summary": "The diff was truncated before any code appeared.",
  "verdict": "unable_to_review",
  "findings": []
}`),
	}

	_, err := RunAgentReview(
		context.Background(), a, t.TempDir(), "HEAD", "prompt",
		"default", "", nil,
	)
	require.ErrorContains(t, err, "could not review the change")
	require.ErrorContains(t, err, "truncated before any code appeared")
}

func TestRunAgentReviewRendersAgentVerdict(t *testing.T) {
	a := &structuredBatchAgent{
		name: "structured",
		result: jsontext.Value(`{
  "schema_version": 2,
  "summary": "Clean change.",
  "verdict": "pass",
  "findings": []
}`),
	}

	got, err := RunAgentReview(
		context.Background(), a, t.TempDir(), "HEAD", "prompt",
		"default", "", nil,
	)
	require.NoError(t, err)
	assert.Equal(t, storage.VerdictPass, got.Verdict)
	assert.Contains(t, got.Output, "**Agent assessment:** Pass")
	assert.Contains(t, got.Output, "No issues found.")
}

func TestRunAgentReviewRejectsOldSchemaFromLiveAgent(t *testing.T) {
	a := &structuredBatchAgent{
		name:   "structured",
		result: jsontext.Value(`{"schema_version":1,"summary":"Clean.","findings":[]}`),
	}
	_, err := RunAgentReview(
		context.Background(), a, t.TempDir(), "HEAD", "prompt", "default", "", nil,
	)
	require.ErrorContains(t, err, "schema_version 1, want 2")
}

func TestRunAgentReviewRejectsFailWithoutFindings(t *testing.T) {
	a := &structuredBatchAgent{
		name:   "structured",
		result: jsontext.Value(`{"schema_version":2,"summary":"Bad.","verdict":"fail","findings":[]}`),
	}
	_, err := RunAgentReview(
		context.Background(), a, t.TempDir(), "HEAD", "prompt", "default", "", nil,
	)
	require.ErrorContains(t, err, "failing verdict without any finding")
}

// narratedReview wraps a review document the way agents without a native
// schema mode sometimes return it: investigation prose, then a fenced block.
func narratedReview(narration, document string) string {
	return narration + "\n\n```json\n" + document + "\n```\n"
}

// plainAgent returns raw verbatim from Review and has no native schema mode.
func plainAgent(raw string) *agent.FakeAgent {
	return &agent.FakeAgent{NameStr: "plain", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		return raw, nil
	}}
}

func TestRunAgentReviewRecoversJSONAfterNarration(t *testing.T) {
	document := `{"schema_version":2,"summary":"Clean change.","verdict":"pass","findings":[]}`
	tests := []struct {
		name      string
		narration string
	}{
		{
			name:      "braces in prose",
			narration: "Let me verify the caller handles the {nested} config correctly.",
		},
		{
			name: "read failure before a successful retry",
			narration: "I couldn't read the diff with git at first, " +
				"so I opened the changed files directly.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := plainAgent(narratedReview(tt.narration, document))
			got, err := RunAgentReview(
				t.Context(), a, t.TempDir(), "HEAD", "prompt", "default", "", nil,
			)
			require.NoError(t, err)
			require.NotNil(t, got.Structured)
			assert.Equal(t, storage.VerdictPass, got.Verdict)
			assert.JSONEq(t, document, string(got.StructuredOutput))
		})
	}
}

func TestRunAgentReviewValidatesRecoveredDocument(t *testing.T) {
	a := plainAgent(narratedReview(
		"I could not read the diff.",
		`{"schema_version":2,"summary":"The diff was unreadable.","verdict":"unable_to_review","findings":[]}`,
	))
	_, err := RunAgentReview(t.Context(), a, t.TempDir(), "HEAD", "prompt", "default", "", nil)
	require.ErrorContains(t, err, "could not review the change")
	_, isNoVerdict := errors.AsType[*NoVerdictError](err)
	assert.False(t, isNoVerdict, "a recovered document is judged by its verdict, not by prose")
}

func TestRunAgentReviewClassifiesProseWithoutDocument(t *testing.T) {
	a := plainAgent("I am unable to read the diff.")
	_, err := RunAgentReview(t.Context(), a, t.TempDir(), "HEAD", "prompt", "default", "", nil)
	noVerdict, ok := errors.AsType[*NoVerdictError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, storage.OutputUnreadableInput, noVerdict.Kind)
}

func TestDecodeSynthesisResultRecoversJSONAfterNarration(t *testing.T) {
	document := `{"schema_version":2,"summary":"Clean.","verdict":"pass","findings":[]}`
	raw := narratedReview("I couldn't read the diff, so I compared the reviews.", document)
	reviews := []ReviewResult{{Agent: "codex"}, {Agent: "gemini"}}

	doc, err := decodeSynthesisResult(
		plainAgent(""), reviews, jsontext.Value(raw), nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "Clean.", doc.Summary)
}

func TestExtractTrailingJSONObject(t *testing.T) {
	valid := `{"schema_version":2,"summary":"ok.","verdict":"pass","findings":[]}`
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "fenced block after narration", raw: narratedReview("Investigating.", valid), want: valid},
		{name: "fenced block alone", raw: "```json\n" + valid + "\n```", want: valid},
		{name: "unfenced object after narration", raw: "Investigating.\n" + valid, want: valid},
		{name: "complete object earlier in narration", raw: "{\"a\":1} was the old config.\n" + valid, want: valid},
		{name: "last of two fenced blocks", raw: narratedReview("Draft.", `{"draft":true}`) + "Final:\n```json\n" + valid + "\n```", want: valid},
		{name: "object on a prose line", raw: "The result is " + valid},
		{name: "text after the closing fence", raw: narratedReview("Investigating.", valid) + "Let me know if you need more."},
		{name: "no JSON object", raw: "No issues found."},
		{name: "unterminated object", raw: "Investigating.\n{\"summary\":"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractTrailingJSONObject(tt.raw))
		})
	}
}
