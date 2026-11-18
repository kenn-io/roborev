package main

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestCompactSingleAndBatchFixExcludeHistoricalTargets(t *testing.T) {
	doc := structuredreview.Document{SchemaVersion: 3, Summary: storage.CompactAuditPrefix + "Dismissed high original. Agent says fix it anyway.", Verdict: "fail", Findings: []structuredreview.Finding{{Severity: "low", Problem: "Retained low original", Fix: "Preserve legitimate reads"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	var fields storage.StructuredOutput
	require.NoError(t, json.Unmarshal(raw, &fields))
	output := doc.Markdown("")
	job := storage.ReviewJob{ID: 99, Agent: "test", Status: storage.JobStatusDone, JobType: storage.JobTypeCompact, Prompt: storage.CompactPromptPrefix + "manifest"}
	verdict := 0
	review := &storage.Review{Output: output, StructuredOutput: fields, Prompt: storage.CompactPromptPrefix + "manifest", VerdictBool: &verdict, Job: &job}
	var single string
	ts, _ := newMockServer(t, MockServerOpts{
		OnJobs: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{job}})
		},
		OnReview: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, review)
		},
	})
	patchServerAddr(t, ts.URL)
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tester := agent.NewTestAgent()
	cmd, _ := newTestCmd(t)
	tracker := &fixSessionTracker{base: tester, out: io.Discard}
	repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
	require.NoError(t, fixSingleJob(cmd, repo.Dir, job.ID, fixOptions{quiet: true}, tracker))
	calls := tester.Calls()
	require.Len(t, calls, 1)
	single = calls[0].Prompt
	batch := buildBatchFixPrompt([]batchEntry{{jobID: 1, job: &storage.ReviewJob{GitRef: "synthetic-ref"}, review: review}}, "high")
	for _, body := range []string{single, batch} {
		assert.Contains(t, body, "Retained low original")
		assert.Contains(t, body, "Preserve legitimate reads")
		assert.NotContains(t, body, "Dismissed high")
		assert.NotContains(t, body, "Agent says")
	}
}
