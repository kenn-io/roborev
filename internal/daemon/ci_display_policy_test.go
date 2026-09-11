package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestSingleSurvivorDisplayPolicyHandoff(t *testing.T) {
	// Each case has its own panel run and terminal jobs. Reuse the database
	// and Git repository instead of rebuilding them for every table row.
	tc := newWorkerTestContext(t, 1)
	for _, format := range []string{"structured"} {
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
					runUUID, members, synthJob := enqueuePanelRun(t, tc, "display-policy", []memberSpec{
						{name: "first", agent: "test"},
						{name: "second", agent: "test"},
					})
					_, err := tc.DB.Exec("UPDATE review_jobs SET min_severity = ? WHERE id = ?", policy.ci, synthJob.ID)
					require.NoError(t, err)
					output := "Review notes."
					document := json.RawMessage(`{"schema_version":2,"summary":"Review notes.","verdict":"fail","findings":[{"severity":"low","problem":"Minor naming issue.","fix":"Rename it.","location":null},{"severity":"medium","problem":"Missing cleanup.","fix":"Close it.","location":null},{"severity":"high","problem":"State is lost.","fix":"Persist it.","location":null}]}`)
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
							assert.NotEmpty(raw)
							assert.Len(stored.StructuredOutput["findings"], 3)
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
					assert.Contains(member.Output, "Minor naming issue.", "publication leaves the stored member intact")
					assert.Equal(policy.member, member.Job.MinSeverity)
				})
			}
		}
	}
}

func TestSuccessfulPanelSynthesisInheritsThreshold(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
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
			delete(stored.StructuredOutput, "source_labels")
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
				assert.Contains(original.Output, "Minor naming issue.")
				assert.Equal(policy.members[i], original.Job.MinSeverity)
			}
		})
	}
}

func TestPanelDisplayPolicyAcrossOutcomes(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	for _, policy := range []struct {
		name    string
		ci      string
		members []string
		visible []string
	}{
		{"inherited mixed", "", []string{"high", "medium"}, []string{"Medium finding.", "High finding."}},
		{"explicit low", "low", []string{"high", "medium"}, []string{"Low finding.", "Medium finding.", "High finding."}},
		{"explicit high", "high", []string{"high", "medium"}, []string{"High finding."}},
		{"unfiltered member", "", []string{"high", ""}, []string{"Low finding.", "Medium finding.", "High finding."}},
	} {
		for _, outcome := range []string{"synthesis", "fallback"} {
			t.Run(policy.name+"/"+outcome, func(t *testing.T) {
				assert := assert.New(t)
				doc := reviewpkg.StructuredReview{SchemaVersion: 2, Verdict: "fail", Summary: "Review complete.", Findings: []reviewpkg.StructuredFinding{
					{Severity: "low", Problem: "Low finding.", Fix: "Fix it.", Sources: []int{1}},
					{Severity: "medium", Problem: "Medium finding.", Fix: "Fix it.", Sources: []int{1}},
					{Severity: "high", Problem: "High finding.", Fix: "Fix it.", Sources: []int{1}},
				}}
				raw, err := json.Marshal(doc)
				require.NoError(t, err)
				a := &synthesisEntrypointTestAgent{name: "policy-outcome", result: string(raw)}
				agent.Register(a)
				t.Cleanup(func() { agent.Unregister(a.Name()) })
				runUUID, members, synthJob := enqueuePanelRun(t, tc, "policy-outcome", []memberSpec{{name: "first", agent: "test"}, {name: "second", agent: "test"}})
				setSynthesisAgent(t, tc, runUUID, a.Name())
				_, err = tc.DB.Exec("UPDATE review_jobs SET min_severity=? WHERE id=?", policy.ci, synthJob.ID)
				require.NoError(t, err)
				for i, m := range members {
					completion := storage.ReviewCompletion{
						StructuredOutput: testutil.ReviewFixtureJSON("No issues found."), Output: "No issues found.", Verdict: storage.VerdictPass, MinSeverity: policy.members[i],
					}
					if i == 0 {
						completion.Output = doc.Markdown("")
						completion.StructuredOutput = raw
						completion.Verdict = storage.VerdictFail
					}
					markMemberRunning(t, tc, m.ID)
					require.NoError(t, tc.DB.CompleteJobResult(m.ID, "test", "", completion))
				}
				synth := releaseAndClaimSynthesis(t, tc, runUUID)
				if outcome == "synthesis" {
					tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)
					stored, err := tc.DB.GetReviewByJobID(synth.ID)
					require.NoError(t, err)
					require.NotNil(t, stored)
					assert.Contains(stored.Output, "Low finding.")
					assert.Contains(a.synthPrompt, "Low finding.")
				} else {
					failed, err := tc.DB.FailJob(synth.ID, testWorkerID, "Synthesis unavailable.")
					require.NoError(t, err)
					require.True(t, failed)
				}
				rows, err := tc.DB.GetPanelMemberReviews(runUUID)
				require.NoError(t, err)
				p := &CIPoller{db: tc.DB, cfgGetter: NewStaticConfig(config.DefaultConfig())}
				body, err := p.panelCommentBody(&storage.CIPanel{PanelRunUUID: runUUID, HeadSHA: "abcdef123456", GithubRepo: "example/project"}, rows)
				require.NoError(t, err)
				assert.Equal(outcome == "fallback", strings.Contains(body, "Synthesis unavailable."))
				for _, f := range doc.Findings {
					assert.Equal(slices.Contains(policy.visible, f.Problem), strings.Contains(body, f.Problem), f.Problem)
				}
				for i, m := range members {
					stored, err := tc.DB.GetReviewByJobID(m.ID)
					require.NoError(t, err)
					assert.Equal(policy.members[i], stored.Job.MinSeverity)
					if i == 0 {
						assert.Equal(doc.Markdown(""), stored.Output)
						storedJSON, err := json.Marshal(stored.StructuredOutput)
						require.NoError(t, err)
						assert.JSONEq(string(raw), string(storedJSON))
					}
				}
			})
		}
	}
}
