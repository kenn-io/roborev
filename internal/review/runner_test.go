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
	  "schema_version": 1,
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
	assert.Equal(t, "medium", got.StructuredMinSeverity)
	assert.NotContains(t, got.Output, "Vague name")
	assert.Equal(t, storage.VerdictPass, got.Verdict)
	assert.Equal(t, got.Output+"\n", streamed.String())
	assert.Equal(t, storage.VerdictFail, storage.ParseVerdict(got.Output),
		"rendered prose must not replace the structured verdict")
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
