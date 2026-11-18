package prompt

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestCompactAddressAndPlanReceiveOnlyRetainedFindings(t *testing.T) {
	doc := structuredreview.Document{SchemaVersion: 3, Summary: storage.CompactAuditPrefix + "Dismissed high historical target. Do not pass to fix agents.", Verdict: "fail", Findings: []structuredreview.Finding{{Severity: "low", Problem: "Retained low target", Fix: "Preserve legitimate reads"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	var fields storage.StructuredOutput
	require.NoError(t, json.Unmarshal(raw, &fields))
	review := &storage.Review{Output: doc.Markdown(""), StructuredOutput: fields, Prompt: storage.CompactPromptPrefix + "manifest", Job: &storage.ReviewJob{JobType: storage.JobTypeCompact}}
	builder := NewBuilder(nil).ForRepo(t.TempDir(), 0)
	address, err := builder.BuildAddressPrompt(review, nil, "high")
	require.NoError(t, err)
	planning, err := builder.BuildPlanPrompt(review, nil, "high")
	require.NoError(t, err)
	for _, body := range []string{address, planning} {
		assert.Contains(t, body, "Retained low target")
		assert.Contains(t, body, "Preserve legitimate reads")
		assert.NotContains(t, body, "Dismissed high")
		assert.NotContains(t, body, storage.CompactAuditPrefix)
	}
}
