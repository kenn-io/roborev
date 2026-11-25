package storage

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestFoldedOutput(t *testing.T) {
	doc := structuredreview.Document{SchemaVersion: 4, Summary: "Summary", Findings: []structuredreview.Finding{{Severity: "low", Problem: "nit", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	var structured StructuredOutput
	require.NoError(t, json.Unmarshal(raw, &structured))
	review := Review{Job: &ReviewJob{}, StructuredOutput: structured, Output: "banner\nprefix\n" + doc.Markdown("")}
	output, ok := review.FoldedOutput()
	assert.True(t, ok)
	assert.Equal(t, "banner\nprefix\n"+doc.FoldedMarkdown(""), output)
	review.Output = "different"
	output, ok = review.FoldedOutput()
	assert.False(t, ok)
	assert.Equal(t, "different", output)
	review.StructuredOutput = nil
	output, ok = review.FoldedOutput()
	assert.False(t, ok)
	assert.Equal(t, "different", output)
}
