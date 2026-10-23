//go:build kata && !windows

package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
	"go.kenn.io/roborev/internal/katacontext"
	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/internal/kataresponse"
	"go.kenn.io/roborev/internal/storage"
)

type loseReadbackRootReceipt struct{ *kata.APIClient }

func (a loseReadbackRootReceipt) Comment(ctx context.Context, req kata.CommentReq) (kata.CommentResult, error) {
	if _, err := a.APIClient.Comment(ctx, req); err != nil {
		return kata.CommentResult{}, err
	}
	return kata.CommentResult{}, errors.New("synthetic transport interruption after acceptance")
}

// This exercises real Kata only in StartReal's disposable allowlisted home.
// It covers the panel -> discussion -> restart -> reassessment/fix -> readback
// loop without calling real review agents or touching an operator's ledger.
func TestKataPanelRepliesReadbackE2E(t *testing.T) {
	ctx := t.Context()
	k := katatest.StartReal(t, ctx)
	tc := newWorkerTestContext(t, 1)
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: k.SocketEndpoint(t, ctx), DefaultProject: "e2etest", Teammate: "roborev", PublishReassessments: true}
	cfg.Hooks = []config.HookConfig{{Event: "review.completed", Type: "kata", Findings: "comments"}}
	s := newServerWithLogs(tc.DB, cfg, "", newTestErrorLog(), newTestActivityLog())
	runUUID, members, synthesis := enqueuePanelRun(t, tc, "panel", []memberSpec{{name: "first", agent: "test"}, {name: "second", agent: "test"}})
	for _, member := range members {
		claimed := claimNext(t, tc)
		require.Equal(t, member.ID, claimed.ID)
		require.NoError(t, tc.DB.CompleteJobResult(member.ID, "test", storage.ReviewCompletion{StructuredOutput: []byte(findingsReviewDocument)}))
		handoffs, err := tc.DB.KataHandoffs(ctx, member.ID)
		require.NoError(t, err)
		assert.Empty(t, handoffs, "panel member findings must not publish separately")
	}
	require.NoError(t, tc.DB.MaybeReleasePanelSynthesis(runUUID))
	claimed := claimNext(t, tc)
	require.Equal(t, synthesis.ID, claimed.ID)
	const panelDocument = `{"schema_version":2,"summary":"Panel findings","verdict":"fail","source_labels":["first","second"],"findings":[{"severity":"high","problem":"Missing input guard.","fix":"Check the input.","location":"a.go:1","sources":[1]},{"severity":"medium","problem":"Cancellation is ignored.","fix":"Check cancellation.","location":"b.go:1","sources":[2]}]}`
	require.NoError(t, tc.DB.CompleteJobResult(synthesis.ID, "test", storage.ReviewCompletion{StructuredOutput: []byte(panelDocument)}))
	h, err := tc.DB.ClaimKataHandoff(ctx, time.Now())
	require.NoError(t, err)
	require.NotNil(t, h)
	api, err := kata.NewAPIClient("", cfg.Integrations.Kata, kata.WithoutWorkspace())
	require.NoError(t, err)
	defer api.CloseIdleConnections()
	h.State, err = katapublish.Deliver(ctx, loseReadbackRootReceipt{api}, h.Request, h.State, func(state katapublish.State) error {
		return tc.DB.CheckpointKataHandoff(ctx, h.OperationID, h.Claim, state)
	})
	require.Error(t, err)
	require.NoError(t, tc.DB.FinishKataHandoff(ctx, *h, err, time.Now()))

	// Reopen the durable local queue after a root was accepted but its receipt
	// was lost. The next delivery must reconcile without another root comment.
	require.NoError(t, tc.DB.Close())
	db, err := storage.Open(filepath.Join(tc.TmpDir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	s = newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
	_, err = db.Exec(`UPDATE kata_handoffs SET next_attempt=0`)
	require.NoError(t, err)
	deliver := func() {
		t.Helper()
		pending, err := db.ClaimKataHandoff(ctx, time.Now())
		require.NoError(t, err)
		require.NotNil(t, pending)
		s.kataPublisher.deliver(ctx, *pending)
		records, err := db.KataHandoffs(ctx, pending.JobID)
		require.NoError(t, err)
		for _, record := range records {
			if record.OperationID == pending.OperationID {
				require.Equal(t, "done", record.Status, record.LastError)
			}
		}
	}
	deliver()
	publications, err := db.KataHandoffs(ctx, synthesis.ID)
	require.NoError(t, err)
	require.Len(t, publications, 1)
	publication := publications[0]
	issue, err := api.ShowComplete(ctx, publication.State.IssueRef)
	require.NoError(t, err)
	require.Len(t, issue.Comments, 2)
	findings, err := db.AssignFindingSnapshots(ctx, synthesis.ID)
	require.NoError(t, err)
	require.Len(t, findings, 2)

	const humanEvidence = "The original input is validated by the caller; the focused reproducer passes."
	human := exec.CommandContext(ctx, "kata", "comment", publication.State.IssueRef, "--refute", publication.State.Receipts[findings[0].Selector].UID, "-m", humanEvidence)
	human.Dir = k.Repo
	human.Env = append(k.Env, "KATA_AUTHOR=reviewer", "KATA_TEAMMATE=human")
	out, err := human.CombinedOutput()
	require.NoError(t, err, "%s", out)

	_, err = db.Exec(`UPDATE review_jobs SET session_id='review-session' WHERE id=?`, synthesis.ID)
	require.NoError(t, err)
	reassessment := serveHuma(t, s, http.MethodPost, "/api/rebuttal", rebuttalRequestBody(t, map[string]any{
		"job_id": synthesis.ID, "finding_id": findings[1].Selector, "commenter": "reviewer", "comment": "Reassess cancellation against current code.",
	}))
	require.Equal(t, http.StatusCreated, reassessment.Code, reassessment.Body.String()) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	var reassessmentJob storage.ReviewJob
	require.NoError(t, json.Unmarshal(reassessment.Body.Bytes(), &reassessmentJob))
	job, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, reassessmentJob.ID, job.ID)
	const reassessmentEvidence = "Cancellation is still ignored along the reachable worker path; the reproduction fails."
	assessmentJSON, err := json.Marshal(map[string]any{"outcome": "confirmed", "explanation": reassessmentEvidence})
	require.NoError(t, err)
	require.NoError(t, db.CompleteJob(job.ID, "test", string(assessmentJSON)))
	deliver()

	// Match the mapped fix skill's ordering: commit first, then post the SHA
	// with the claimed verification. The claim must leave the review open.
	sha := tc.GitRepo.CommitFile("guard.go", "package example\n", "Add input guard")
	fixEvidence := "Fixed in " + sha + "; the input guard regression passes."
	intent := kataresponse.Request{JobID: synthesis.ID, OperationID: uuid.New().String(), FindingID: findings[0].Selector, Kind: "reply", Body: fixEvidence} //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	encoded, err := json.Marshal(intent)
	require.NoError(t, err)
	reply := serveHuma(t, s, http.MethodPost, "/api/kata/response", encoded)
	require.Equal(t, http.StatusAccepted, reply.Code, reply.Body.String()) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	deliver()

	contextView, err := s.humaKataContext(ctx, &KataContextInput{JobID: synthesis.ID, ReviewUUID: publication.ReviewUUID})
	require.NoError(t, err)
	claims := katacontext.Format(contextView.Body.Snapshots)
	for _, evidence := range []string{humanEvidence, reassessmentEvidence, fixEvidence} {
		assert.Contains(t, claims, evidence)
	}
	assert.Contains(t, claims, "human")
	assert.Contains(t, claims, "roborev-test")
	review, err := db.GetReviewByJobID(synthesis.ID)
	require.NoError(t, err)
	assert.False(t, review.Closed)
	assert.Equal(t, storage.VerdictFail, review.Verdict())

	next, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: sha, Agent: "test", JobType: storage.JobTypeReview})
	require.NoError(t, err)
	builder := s.workerPool.basePromptBuilderForJob(ctx, preparedJobCheckout{promptRepoPath: tc.TmpDir}, next, cfg)
	nextPrompt, err := builder.Build(sha, 2, "test", "", "")
	require.NoError(t, err)
	for _, evidence := range []string{humanEvidence, reassessmentEvidence, fixEvidence} {
		assert.Contains(t, nextPrompt, evidence)
	}

	fix, err := s.humaFixJob(ctx, &FixJobInput{Body: FixJobRequest{ParentJobID: synthesis.ID, PlanFirst: true}})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, fix.Status)
	fixJob, ok := fix.Body.(*storage.ReviewJob)
	require.True(t, ok)
	for _, evidence := range []string{humanEvidence, reassessmentEvidence, fixEvidence} {
		assert.Contains(t, fixJob.Prompt, evidence)
	}
}
