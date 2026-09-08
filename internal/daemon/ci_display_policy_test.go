package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func TestSingleSurvivorDisplayPolicyHandoff(t *testing.T) {
	for _, format := range []string{"prose", "prefixed prose", "numbered sections", "severity rubric", "structured"} {
		for _, mode := range []string{"passthrough", "fallback"} {
			for _, policy := range []struct {
				name          string
				member        string
				ci            string
				lowVisible    bool
				mediumVisible bool
			}{
				{name: "unset", lowVisible: true, mediumVisible: true},
				{name: "inherited", member: "medium", mediumVisible: true},
				{name: "looser override", member: "medium", ci: "low", lowVisible: true, mediumVisible: true},
				{name: "stricter override", member: "medium", ci: "high"},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s", format, mode, policy.name), func(t *testing.T) {
					assert := assert.New(t)
					tc := newWorkerTestContext(t, 1)
					runUUID, members, synthJob := enqueuePanelRun(t, tc, "display-policy", []memberSpec{
						{name: "first", agent: "test"},
						{name: "second", agent: "test"},
					})
					_, err := tc.DB.Exec("UPDATE review_jobs SET min_severity = ? WHERE id = ?", policy.ci, synthJob.ID)
					require.NoError(t, err)
					output := "### Low\n\nMinor naming issue.\n\n### Medium\n\nMissing cleanup.\n\n### High\n\nState is lost."
					if format == "prefixed prose" {
						output = "## roborev: Combined Review (`abcdef1`)\n\n" + output
					}
					if format == "numbered sections" {
						output = "1. **Summary**: Minor naming issue.\n\n2. **Review Findings**:\n\n### High\n\nState is lost.\n\n### Medium\n\nMissing cleanup.\n\n### Low\n\nMinor naming issue."
					}
					if format == "severity rubric" {
						output = "Severity levels:\nHigh: immediate action.\nLow: minor concern.\n\nReview Findings:\n" + output
					}
					var document json.RawMessage
					if format == "structured" {
						document = json.RawMessage(`{"schema_version":2,"summary":"Review complete.","verdict":"fail","findings":[{"severity":"low","problem":"Minor naming issue.","fix":"Rename it.","location":null},{"severity":"medium","problem":"Missing cleanup.","fix":"Close it.","location":null},{"severity":"high","problem":"State is lost.","fix":"Persist it.","location":null}]}`)
					}
					// The successful review is deliberately not the first member.
					markMemberRunning(t, tc, members[1].ID)
					require.NoError(t, tc.DB.CompleteJobResult(members[1].ID, "test", "", storage.ReviewCompletion{
						Output: output, Verdict: storage.VerdictFail, StructuredOutput: document, MinSeverity: policy.member,
					}))
					failMember(t, tc, members[0].ID)
					synth := releaseAndClaimSynthesis(t, tc, runUUID)
					if mode == "passthrough" {
						tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)
						stored, err := tc.DB.GetReviewByJobID(synth.ID)
						require.NoError(t, err)
						assert.Contains(stored.Output, "Minor naming issue.", "storage retains the finding")
						assert.Contains(stored.Output, "Missing cleanup.")
						if document != nil {
							raw, err := json.Marshal(stored.StructuredOutput)
							require.NoError(t, err)
							assert.JSONEq(string(document), string(raw))
						}
					} else {
						failed, err := tc.DB.FailJob(synth.ID, testWorkerID, "Synthesis unavailable.")
						require.NoError(t, err)
						require.True(t, failed)
					}
					rows, err := tc.DB.GetPanelMemberReviews(runUUID)
					require.NoError(t, err)
					poller := &CIPoller{db: tc.DB, cfgGetter: NewStaticConfig(config.DefaultConfig())}
					body, err := poller.panelCommentBody(&storage.CIPanel{
						PanelRunUUID: runUUID, HeadSHA: "abcdef123456", GithubRepo: "example/project",
					}, rows)
					require.NoError(t, err)
					if policy.lowVisible {
						assert.Contains(body, "Minor naming issue.")
					} else {
						assert.NotContains(body, "Minor naming issue.")
						assert.NotContains(body, "High: immediate action.", "rubric entries are not findings")
					}
					if policy.mediumVisible {
						assert.Contains(body, "Missing cleanup.")
					} else {
						assert.NotContains(body, "Missing cleanup.")
					}
					assert.Contains(body, "State is lost.")
					member, err := tc.DB.GetReviewByJobID(members[1].ID)
					require.NoError(t, err)
					assert.Equal(output, member.Output, "publication leaves the stored member intact")
					assert.Equal(policy.member, member.Job.MinSeverity)
				})
			}
		}
	}
}
