package daemon

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type strictSessionTestAgent struct {
	*structuredWorkerTestAgent
	session, path string
	invoke        func(*strictSessionTestAgent)
}

func (a *strictSessionTestAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }
func (a *strictSessionTestAgent) WithModel(string) agent.Agent                   { return a }
func (a *strictSessionTestAgent) WithAgentic(bool) agent.Agent                   { return a }
func (a *strictSessionTestAgent) WithSessionID(id string) agent.Agent {
	clone := *a
	clone.session = id
	return &clone
}

func (a *strictSessionTestAgent) WithStrictReview(_ config.ReviewToolPolicy, path string) agent.Agent {
	clone := *a
	clone.path = path
	return &clone
}

func (a *strictSessionTestAgent) CommandLine() string { return a.name + " --strict-config " + a.path }

func (a *strictSessionTestAgent) ReviewWithSchema(_ context.Context, _, _, _ string, _ jsontext.Value, out io.Writer) (jsontext.Value, error) {
	a.invoke(a)
	_, err := fmt.Fprintln(out, `{"type":"system","subtype":"init","session_id":"fresh-session"}`)
	if err != nil {
		return nil, err
	}
	return a.result, nil
}

func TestStrictReviewPersistsSessionIsolation(t *testing.T) {
	for _, synthesis := range []bool{false, true} {
		for _, checkoutIsolation := range []bool{false, true} {
			t.Run(fmt.Sprintf("synthesis=%t/checkout=%t", synthesis, checkoutIsolation), func(t *testing.T) {
				tc := newWorkerTestContext(t, 1)
				cfg := config.DefaultConfig()
				cfg.IsolateReviews = checkoutIsolation
				tc.reconfigurePool(cfg)
				a := &strictSessionTestAgent{structuredWorkerTestAgent: &structuredWorkerTestAgent{name: "strict-session", result: jsontext.Value(`{"schema_version":2,"verdict":"fail","summary":"Check the reference.","findings":[{"severity":"medium","problem":"Reference is missing.","fix":"Add the reference.","location":null,"sources":[1]}]}`)}}
				agent.RegisterForTest(t, a)
				raw := `{"tools":{"mode":"read-only","mcp":{}}}`
				var claimed *storage.ReviewJob
				if synthesis {
					run, members, _ := enqueuePanelRun(t, tc, "strict", []memberSpec{{name: "first", agent: a.Name()}, {name: "second", agent: a.Name()}})
					_, err := tc.DB.Exec("UPDATE review_jobs SET panel_member_config_json=? WHERE id=?", raw, members[0].ID)
					require.NoError(t, err)
					for _, member := range members {
						completeMember(t, tc, member.ID, a.Name(), "- Medium: Reference is missing.")
					}
					setSynthesisAgent(t, tc, run, a.Name())
					claimed = releaseAndClaimSynthesis(t, tc, run)
				} else {
					_, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: testutil.GetHeadSHA(t, tc.TmpDir), Agent: a.Name(), PanelMemberConfigJSON: raw})
					require.NoError(t, err)
					claimed, err = tc.DB.ClaimJob(testWorkerID)
					require.NoError(t, err)
				}
				require.NotNil(t, claimed)
				_, err := tc.DB.Exec("UPDATE review_jobs SET session_id=?,resume_source_job_uuid=? WHERE id=?", "old-session", "11111111-1111-4111-8111-111111111111", claimed.ID)
				require.NoError(t, err)
				claimed.SessionID = "old-session"
				var path string
				calls := 0
				a.invoke = func(selected *strictSessionTestAgent) {
					calls++
					path = selected.path
					require.NotEmpty(t, path, "strict native config must be prepared")
					assert.FileExists(t, path)
					assert.Empty(t, selected.session)
					isolated, err := tc.DB.JobSessionIsolated(t.Context(), claimed.ID)
					require.NoError(t, err)
					assert.True(t, isolated)
					row, err := tc.DB.GetJobByID(claimed.ID)
					require.NoError(t, err)
					assert.Empty(t, row.SessionID)
					assert.Nil(t, row.ResumeSourceJobUUID)
					assert.Contains(t, row.CommandLine, path, "metadata must describe the prepared strict invocation")
				}
				if synthesis {
					tc.Pool.processSynthesisJob(t.Context(), testWorkerID, claimed)
				} else {
					tc.Pool.processJob(testWorkerID, claimed)
				}
				require.Equal(t, 1, calls)
				row, err := tc.DB.GetJobByID(claimed.ID)
				require.NoError(t, err)
				require.Equal(t, storage.JobStatusDone, row.Status, row.Error)
				assert.Equal(t, "fresh-session", row.SessionID)
				isolated, err := tc.DB.JobSessionIsolated(t.Context(), row.ID)
				require.NoError(t, err)
				assert.True(t, isolated)
				_, err = os.Stat(path)
				require.ErrorIs(t, err, os.ErrNotExist)
				if !synthesis {
					_, err := tc.DB.Exec("UPDATE review_jobs SET branch=? WHERE id=?", "drafts", row.ID)
					require.NoError(t, err)
					candidate, err := tc.DB.FindReusableSessionCandidate(row.RepoID, "drafts", row.Agent, row.ReviewType, row.WorktreePath)
					require.NoError(t, err)
					assert.Nil(t, candidate, "captured strict session cannot become a reuse candidate")
				}
				server := &Server{db: tc.DB}
				_, err = server.humaRebuttal(t.Context(), &RebuttalInput{Body: RebuttalRequest{JobID: row.ID, Finding: "Reference is missing.", Comment: "Please check.", Commenter: "reviewer"}})
				if synthesis {
					require.ErrorContains(t, err, "completed local code review")
				} else {
					require.ErrorContains(t, err, "isolated review sessions")
				}
			})
		}
	}
}
