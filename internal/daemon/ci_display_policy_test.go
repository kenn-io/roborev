package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	reviewpkg "go.kenn.io/roborev/internal/review"
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

func TestSuccessfulPanelSynthesisInheritsThreshold(t *testing.T) {
	for _, policy := range []struct {
		name      string
		members   []string
		ci        string
		effective string
		verdict   storage.Verdict
	}{
		{"inherited", []string{"medium", "medium"}, "", "medium", storage.VerdictPass},
		{"explicit low", []string{"medium", "medium"}, "low", "low", storage.VerdictFail},
		{"explicit high", []string{"medium", "medium"}, "high", "high", storage.VerdictPass},
		{"different thresholds", []string{"high", "medium"}, "", "medium", storage.VerdictPass},
		{"unfiltered member", []string{"medium", ""}, "", "", storage.VerdictFail},
	} {
		t.Run(policy.name, func(t *testing.T) {
			assert := assert.New(t)
			tc := newWorkerTestContext(t, 1)
			document := json.RawMessage(`{"schema_version":2,"summary":"Combined.","verdict":"fail","findings":[{"severity":"low","problem":"Minor naming issue.","fix":"Rename it.","location":null,"sources":[1,2]}]}`)
			a := &synthesisEntrypointTestAgent{name: "threshold-synthesis", result: string(document)}
			agent.Register(a)
			t.Cleanup(func() { agent.Unregister(a.name) })
			runUUID, members, synthJob := enqueuePanelRun(t, tc, "threshold-synthesis", []memberSpec{{name: "first", agent: "test"}, {name: "second", agent: "test"}})
			setSynthesisAgent(t, tc, runUUID, a.name)
			_, err := tc.DB.Exec("UPDATE review_jobs SET min_severity=? WHERE id=?", policy.ci, synthJob.ID)
			require.NoError(t, err)
			for i, m := range members {
				markMemberRunning(t, tc, m.ID)
				require.NoError(t, tc.DB.CompleteJobResult(m.ID, "test", "", storage.ReviewCompletion{Output: "### Low\nMinor naming issue.", StructuredOutput: document, Verdict: storage.ParseVerdictAtSeverity("### Low\nMinor naming issue.", policy.members[i]), MinSeverity: policy.members[i]}))
			}
			synth := releaseAndClaimSynthesis(t, tc, runUUID)
			tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)
			stored, err := tc.DB.GetReviewByJobID(synth.ID)
			require.NoError(t, err)
			assert.Contains(a.synthPrompt, "Minor naming issue.", "successful synthesis uses the full member review")
			assert.Contains(stored.Output, "Minor naming issue.")
			assert.Equal(policy.effective, stored.Job.MinSeverity)
			assert.Equal(policy.verdict, stored.Verdict())
			raw, err := json.Marshal(stored.StructuredOutput)
			require.NoError(t, err)
			assert.JSONEq(string(document), string(raw))
			rows, err := tc.DB.GetPanelMemberReviews(runUUID)
			require.NoError(t, err)
			p := &CIPoller{db: tc.DB, cfgGetter: NewStaticConfig(config.DefaultConfig())}
			body, err := p.panelCommentBody(&storage.CIPanel{PanelRunUUID: runUUID, HeadSHA: "abcdef123456", GithubRepo: "example/project"}, rows)
			require.NoError(t, err)
			if policy.verdict == storage.VerdictFail {
				assert.Contains(body, "Minor naming issue.")
			} else {
				assert.NotContains(body, "Minor naming issue.")
			}
			for i, m := range members {
				original, err := tc.DB.GetReviewByJobID(m.ID)
				require.NoError(t, err)
				assert.Equal("### Low\nMinor naming issue.", original.Output)
				assert.Equal(policy.members[i], original.Job.MinSeverity)
			}
		})
	}
}

func TestPanelProseVerdictAfterRubric(t *testing.T) {
	for _, boundary := range []string{"Review Findings:", "2. **Review Findings**:", "---"} {
		t.Run(boundary, func(t *testing.T) {
			assert := assert.New(t)
			tc := newWorkerTestContext(t, 1)
			output := "Severity levels:\nHigh: immediate action.\nLow: minor concern.\n\n" + boundary + "\n### Low\nMinor naming issue."
			a := &agent.FakeAgent{NameStr: "rubric-review", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) { return output, nil }}
			result, err := reviewpkg.RunAgentReview(context.Background(), a, tc.TmpDir, "HEAD", "Review the change.", "default", "medium", nil)
			require.NoError(t, err)
			assert.Equal(storage.VerdictPass, result.Verdict)
			runUUID, members, _ := enqueuePanelRun(t, tc, "rubric-review", []memberSpec{{name: "first", agent: "test"}})
			markMemberRunning(t, tc, members[0].ID)
			require.NoError(t, tc.DB.CompleteJobResult(members[0].ID, a.Name(), "", storage.ReviewCompletion{Output: result.Output, Verdict: result.Verdict, MinSeverity: result.MinSeverity}))
			synth := releaseAndClaimSynthesis(t, tc, runUUID)
			tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)
			stored, err := tc.DB.GetReviewByJobID(synth.ID)
			require.NoError(t, err)
			assert.Equal(storage.VerdictPass, stored.Verdict())
			assert.Equal(output, stored.Output)
			rows, err := tc.DB.GetPanelMemberReviews(runUUID)
			require.NoError(t, err)
			p := &CIPoller{db: tc.DB, cfgGetter: NewStaticConfig(config.DefaultConfig())}
			body, err := p.panelCommentBody(&storage.CIPanel{PanelRunUUID: runUUID, HeadSHA: "abcdef123456", GithubRepo: "example/project"}, rows)
			require.NoError(t, err)
			assert.NotContains(body, "Minor naming issue.")
			assert.NotContains(body, "High: immediate action.")
		})
	}
}
