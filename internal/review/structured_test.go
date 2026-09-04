package review

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStructuredReviewKeepsFindingsBelowThreshold(t *testing.T) {
	raw := json.RawMessage(`{
  "schema_version": 1,
  "summary": "Two maintainability risks found.",
  "findings": [
    {"severity":"high","problem":"State can diverge.","fix":"Use one owner.","location":"state.go:20"},
    {"severity":"low","problem":"Name is vague.","fix":"Rename it.","location":null}
  ]
}`)

	decoded, err := DecodeStructuredReview(raw)
	require.NoError(t, err)
	require.Len(t, decoded.Findings, 2)
	assert.False(t, decoded.Passed("medium"))
	assert.True(t, decoded.Passed("critical"))
	markdown := decoded.Markdown("medium")
	assert.Contains(t, markdown, "### 1. High")
	assert.Contains(t, markdown, "**Location:** state.go:20")
	assert.Contains(t, markdown, "### 2. Low")
	assert.Contains(t, markdown, "Name is vague")
	assert.NotContains(t, markdown, "No findings at or above")
}

func TestStructuredReviewPassesWhenAllFindingsBelowThreshold(t *testing.T) {
	decoded, err := DecodeStructuredReview(json.RawMessage(
		`{"schema_version":1,"summary":"Minor issue.","findings":[{"severity":"low","problem":"Vague name.","fix":"Rename it.","location":null}]}`,
	))
	require.NoError(t, err)
	assert.True(t, decoded.Passed("high"))
	assert.False(t, decoded.Passed(""))
	assert.False(t, decoded.Passed("low"))
	markdown := decoded.Markdown("high")
	assert.Contains(t, markdown, "No findings at or above high severity.")
	assert.Contains(t, markdown, "Vague name.")
	assert.NotContains(t, decoded.Markdown(""), "No findings at or above")
}

func TestStructuredReviewNoFindingsRendersNoIssues(t *testing.T) {
	decoded, err := DecodeStructuredReview(json.RawMessage(
		`{"schema_version":1,"summary":"Clean change.","findings":[]}`,
	))
	require.NoError(t, err)
	assert.True(t, decoded.Passed(""))
	assert.Contains(t, decoded.Markdown("high"), "No issues found.")
}

func TestStructuredReviewRejectsUnknownFields(t *testing.T) {
	_, err := DecodeStructuredReview(json.RawMessage(
		`{"schema_version":2,"summary":"Done.","verdict":"pass","findings":[],"confidence":1}`,
	))
	require.ErrorContains(t, err, "unknown field")
}

func TestStructuredReviewRequiresKnownSchemaVersion(t *testing.T) {
	_, err := DecodeStructuredReview(json.RawMessage(
		`{"schema_version":3,"summary":"Done.","verdict":"pass","findings":[]}`,
	))
	require.ErrorContains(t, err, "unsupported structured review schema_version 3")
}

func TestStructuredReviewVersionTwoCarriesVerdict(t *testing.T) {
	decoded, err := DecodeStructuredReview(json.RawMessage(
		`{"schema_version":2,"summary":"Looks wrong overall.","verdict":"FAIL","findings":[{"severity":"low","problem":"Nit.","fix":"Tidy.","location":null}]}`,
	))
	require.NoError(t, err)
	assert.Equal(t, "fail", decoded.Verdict)
	assert.True(t, decoded.Passed("medium"),
		"the agent verdict is informational; findings decide the outcome")
	assert.Contains(t, decoded.Markdown("medium"), "**Agent assessment:** Fail")
	assert.False(t, decoded.UnableToReview())
}

func TestStructuredReviewVersionOneStillDecodes(t *testing.T) {
	decoded, err := DecodeStructuredReview(json.RawMessage(
		`{"schema_version":1,"summary":"Legacy.","findings":[]}`,
	))
	require.NoError(t, err)
	assert.Empty(t, decoded.Verdict)
	assert.NotContains(t, decoded.Markdown(""), "Agent assessment")

	_, err = DecodeStructuredReview(json.RawMessage(
		`{"schema_version":1,"summary":"Legacy.","verdict":"pass","findings":[]}`,
	))
	require.ErrorContains(t, err, "schema_version 1 does not carry a verdict")
}

func TestStructuredReviewRejectsInvalidVerdict(t *testing.T) {
	_, err := DecodeStructuredReview(json.RawMessage(
		`{"schema_version":2,"summary":"Done.","verdict":"maybe","findings":[]}`,
	))
	require.ErrorContains(t, err, `invalid verdict "maybe"`)

	_, err = DecodeStructuredReview(json.RawMessage(
		`{"schema_version":2,"summary":"Done.","findings":[]}`,
	))
	require.ErrorContains(t, err, "invalid verdict")
}

func TestStructuredReviewUnableToReview(t *testing.T) {
	decoded, err := DecodeStructuredReview(json.RawMessage(
		`{"schema_version":2,"summary":"The diff was empty.","verdict":"unable_to_review","findings":[]}`,
	))
	require.NoError(t, err)
	assert.True(t, decoded.UnableToReview())
}
