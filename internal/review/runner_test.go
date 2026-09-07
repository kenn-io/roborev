package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestRunAgentReviewKeepsCustomVerdictWithRenderedOutput(t *testing.T) {
	a := &structuredBatchAgent{
		name: "structured",
		result: json.RawMessage(`{
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
		result: json.RawMessage(`{
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

func TestRunAgentReviewProseVerdictHonorsMinSeverity(t *testing.T) {
	const output = "Summary of the change.\n\n- Low: variable name could be clearer\n"
	a := &mockAgent{name: "prose", output: output}

	got, err := RunAgentReview(
		context.Background(), a, t.TempDir(), "HEAD", "prompt",
		"default", "medium", nil,
	)
	require.NoError(t, err)
	assert.Nil(t, got.Structured)
	assert.Equal(t, output, got.Output, "prose output is stored untouched")
	assert.Equal(t, "medium", got.MinSeverity)
	assert.Equal(t, storage.VerdictPass, got.Verdict, "a low-only review passes a medium threshold")

	got, err = RunAgentReview(
		context.Background(), a, t.TempDir(), "HEAD", "prompt",
		"default", "low", nil,
	)
	require.NoError(t, err)
	assert.Equal(t, storage.VerdictFail, got.Verdict)
}

func TestRunAgentReviewDerivesBuiltInVerdict(t *testing.T) {
	a := &mockAgent{name: "prose", output: "No issues found."}

	got, err := RunAgentReview(
		context.Background(), a, t.TempDir(), "HEAD", "prompt",
		"default", "", nil,
	)
	require.NoError(t, err)
	assert.Nil(t, got.Structured)
	assert.Equal(t, "No issues found.", got.Output)
	assert.Equal(t, storage.VerdictPass, got.Verdict)
}

func TestRunAgentReviewRejectsOutputWithoutVerdict(t *testing.T) {
	const output = "I am unable to read the diff file because it is ignored by configured ignore patterns."
	a := &mockAgent{name: "prose", output: output}

	got, err := RunAgentReview(
		context.Background(), a, t.TempDir(), "HEAD", "prompt",
		"default", "", nil,
	)
	var noVerdict *NoVerdictError
	require.ErrorAs(t, err, &noVerdict)
	assert.Equal(t, output, noVerdict.Output)
	assert.Equal(t, output, got.Output)
	assert.Equal(t, storage.VerdictUnknown, got.Verdict)
}

func TestRunAgentReviewKeepsProseFindings(t *testing.T) {
	a := &mockAgent{name: "prose", output: "The retry loop never terminates when the queue is empty."}

	got, err := RunAgentReview(
		context.Background(), a, t.TempDir(), "HEAD", "prompt",
		"default", "", nil,
	)
	require.NoError(t, err)
	assert.Equal(t, storage.VerdictFail, got.Verdict)
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
		result: json.RawMessage(`{
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
		result: json.RawMessage(`{
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
		result: json.RawMessage(`{"schema_version":1,"summary":"Clean.","findings":[]}`),
	}
	_, err := RunAgentReview(
		context.Background(), a, t.TempDir(), "HEAD", "prompt", "default", "", nil,
	)
	require.ErrorContains(t, err, "schema_version 1, want 2")
}

func TestRunAgentReviewRejectsFailWithoutFindings(t *testing.T) {
	a := &structuredBatchAgent{
		name:   "structured",
		result: json.RawMessage(`{"schema_version":2,"summary":"Bad.","verdict":"fail","findings":[]}`),
	}
	_, err := RunAgentReview(
		context.Background(), a, t.TempDir(), "HEAD", "prompt", "default", "", nil,
	)
	require.ErrorContains(t, err, "failing verdict without any finding")
}
