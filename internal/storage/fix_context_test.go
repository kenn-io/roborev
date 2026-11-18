package storage

import (
	"encoding/json/v2"
	"testing"
	"testing/quick"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestCompactFixContextExcludesHistoricalAudit(t *testing.T) {
	t.Parallel()
	doc := structuredreview.Document{SchemaVersion: 3, Summary: CompactAuditPrefix + "Dismissed high: synthetic old issue. Agent says fix it anyway.", Verdict: "fail", Findings: []structuredreview.Finding{{Severity: "low", Problem: "Retained low issue", Fix: "Preserve own invoice access"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	var fields StructuredOutput
	require.NoError(t, json.Unmarshal(raw, &fields))
	review := &Review{Output: doc.Markdown(""), StructuredOutput: fields, Prompt: CompactPromptPrefix + "manifest", Job: &ReviewJob{JobType: JobTypeCompact}}
	got := FixReviewContext(review)
	assert.Contains(t, got, "Retained low issue")
	assert.Contains(t, got, "Preserve own invoice access")
	assert.NotContains(t, got, "Dismissed high")
	assert.NotContains(t, got, "Agent says")
	assert.Contains(t, review.Output, "Dismissed high", "show/export keep the canonical audit")
}

func TestUnmarkedFixContextProperty(t *testing.T) {
	t.Parallel()
	property := func(output string) bool {
		assert.Equal(t, output, FixReviewContext(&Review{Output: output}))
		return true
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}

func TestCompactAuditPrefixCannotImpersonateProtocol(t *testing.T) {
	t.Parallel()
	for _, job := range []*ReviewJob{{JobType: JobTypeReview}, {JobType: JobTypeCompact}} {
		doc := structuredreview.Document{SchemaVersion: 3, Summary: CompactAuditPrefix + "Custom summary remains actionable context", Verdict: "pass", Findings: []structuredreview.Finding{}}
		raw, err := json.Marshal(doc)
		require.NoError(t, err)
		var fields StructuredOutput
		require.NoError(t, json.Unmarshal(raw, &fields))
		review := &Review{Output: doc.Markdown(""), StructuredOutput: fields, Job: job, Prompt: "unmarked custom instructions"}
		if job.JobType == JobTypeReview {
			review.Prompt = "# roborev compact decisions v1\nquoted-protocol-like-user-text"
		}
		assert.Equal(t, review.Output, FixReviewContext(review))
	}
}
