package review

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStructuredReviewFiltersAndRenders(t *testing.T) {
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
	filtered := decoded.Filter("medium")
	require.Len(t, filtered.Findings, 1)
	assert.False(t, filtered.Passed())
	markdown := filtered.Markdown()
	assert.Contains(t, markdown, "### 1. High")
	assert.Contains(t, markdown, "**Location:** state.go:20")
	assert.NotContains(t, markdown, "Name is vague")
}

func TestStructuredReviewPassesAfterSeverityFiltering(t *testing.T) {
	decoded, err := DecodeStructuredReview(json.RawMessage(
		`{"schema_version":1,"summary":"Minor issue.","findings":[{"severity":"low","problem":"Vague name.","fix":"Rename it.","location":null}]}`,
	))
	require.NoError(t, err)
	filtered := decoded.Filter("high")
	assert.True(t, filtered.Passed())
	assert.Contains(t, filtered.Markdown(), "No findings at or above")
}

func TestStructuredReviewRejectsUnknownFields(t *testing.T) {
	_, err := DecodeStructuredReview(json.RawMessage(
		`{"schema_version":1,"summary":"Done.","findings":[],"verdict":"pass"}`,
	))
	require.ErrorContains(t, err, "unknown field")
}

func TestStructuredReviewRequiresCurrentSchemaVersion(t *testing.T) {
	_, err := DecodeStructuredReview(json.RawMessage(
		`{"schema_version":2,"summary":"Done.","findings":[]}`,
	))
	require.ErrorContains(t, err, "unsupported structured review schema_version 2")
}

func TestStricterMinSeverity(t *testing.T) {
	assert.Equal(t, "high", stricterMinSeverity("low", "high"))
	assert.Equal(t, "high", stricterMinSeverity("high", "low"))
}
