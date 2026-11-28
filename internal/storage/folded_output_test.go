package storage

import (
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestFoldedOutput(t *testing.T) {
	t.Parallel()
	doc := structuredreview.Document{SchemaVersion: 4, Summary: "Summary", Findings: []structuredreview.Finding{{Severity: "low", Nit: true, Problem: "nit", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	var structured StructuredOutput
	require.NoError(t, json.Unmarshal(raw, &structured))
	review := Review{Job: &ReviewJob{}, StructuredOutput: structured, Output: doc.Markdown("")}
	output, ok := review.FoldedOutput()
	assert.True(t, ok)
	assert.Equal(t, doc.FoldedMarkdown(""), output)
	assert.Contains(t, output, "1 nit hidden")
	review.Output = "different"
	output, ok = review.FoldedOutput()
	assert.False(t, ok)
	assert.Equal(t, "different", output)
	review.StructuredOutput = nil
	output, ok = review.FoldedOutput()
	assert.False(t, ok)
	assert.Equal(t, "different", output)
}

func TestFoldedOutputHydratedSynthesis(t *testing.T) {
	t.Parallel()
	doc := structuredreview.Document{SchemaVersion: 4, Summary: "Summary", Findings: []structuredreview.Finding{{Severity: "low", Nit: true, Problem: "nit", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	review := Review{Job: &ReviewJob{JobType: JobTypeSynthesis}}
	fields := reviewScanFields{JobType: JobTypeSynthesis, NonVoting: 1, OutputPrefix: "Panel summary\n\n", StructuredOutput: sql.NullString{String: string(raw), Valid: true}}
	require.NoError(t, applyReviewScan(&review, fields))
	output, ok := review.FoldedOutput()
	assert.True(t, ok)
	assert.Equal(t, NonVotingBanner+fields.OutputPrefix+doc.FoldedMarkdown(""), output)
	assert.Contains(t, output, "1 nit hidden")
}
