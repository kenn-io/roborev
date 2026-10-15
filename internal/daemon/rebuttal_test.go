package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type structuredRebuttalTestAgent struct {
	sessionID       string
	progress        string
	result          string
	schema          jsontext.Value
	structuredCalls int
	proseCalls      int
}

type sessionOnlyRebuttalTestAgent struct{}

func (*sessionOnlyRebuttalTestAgent) Name() string { return "test" }
func (*sessionOnlyRebuttalTestAgent) Review(context.Context, string, string, string, io.Writer) (string, error) {
	return "", nil
}
func (a *sessionOnlyRebuttalTestAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }
func (a *sessionOnlyRebuttalTestAgent) WithAgentic(bool) agent.Agent                   { return a }
func (a *sessionOnlyRebuttalTestAgent) WithModel(string) agent.Agent                   { return a }
func (a *sessionOnlyRebuttalTestAgent) WithSessionID(string) agent.Agent               { return a }
func (*sessionOnlyRebuttalTestAgent) CommandLine() string                              { return "test" }

func (*structuredRebuttalTestAgent) SupportsReadOnlyReview() bool { return true }

func (a *structuredRebuttalTestAgent) Name() string { return "test" }

func (a *structuredRebuttalTestAgent) writeProgress(out io.Writer) error {
	if out == nil {
		return nil
	}
	_, err := fmt.Fprintf(out, "{\"type\":\"session\",\"id\":%q}\n%s\n%s\n", a.sessionID, a.progress, a.result)
	return err
}

func (a *structuredRebuttalTestAgent) Review(_ context.Context, _, _, _ string, out io.Writer) (string, error) {
	a.proseCalls++
	if err := a.writeProgress(out); err != nil {
		return "", err
	}
	return a.progress + "\n" + a.result, nil
}

func (a *structuredRebuttalTestAgent) ReviewWithSchema(_ context.Context, _, _, _ string, schema jsontext.Value, out io.Writer) (jsontext.Value, error) {
	a.structuredCalls++
	a.schema = append(jsontext.Value(nil), schema...)
	if err := a.writeProgress(out); err != nil {
		return nil, err
	}
	return jsontext.Value(a.result), nil
}

func (a *structuredRebuttalTestAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }
func (a *structuredRebuttalTestAgent) WithAgentic(bool) agent.Agent                   { return a }
func (a *structuredRebuttalTestAgent) WithModel(string) agent.Agent                   { return a }
func (a *structuredRebuttalTestAgent) WithSessionID(id string) agent.Agent {
	a.sessionID = id
	return a
}
func (a *structuredRebuttalTestAgent) CommandLine() string { return "test" }

func TestRebuttalWorkerUsesAssessmentStructuredOutput(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	env := newWorkerTestContext(t, 1)
	old, err := agent.Get("test")
	require.NoError(t, err)
	t.Cleanup(func() { agent.Register(old) })

	const result = `{"outcome":"refuted","explanation":"The caller checks the value."}`
	a := &structuredRebuttalTestAgent{
		progress: "Checking the caller's validation.",
		result:   result,
	}
	agent.Register(a)
	job, err := env.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: env.Repo.ID, Agent: "test", JobType: storage.JobTypeTask,
		Source: storage.JobSourceRebuttal, SessionID: "original-session",
		Prompt: "focused request",
	})
	require.NoError(t, err)
	claimed, err := env.DB.ClaimJob("worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)

	env.Pool.processJob("worker", claimed)

	got, err := env.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusDone, got.Status)
	review, err := env.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.JSONEq(t, result, review.Output)
	assert.Equal(t, "original-session", a.sessionID)
	assert.Equal(t, 1, a.structuredCalls)
	assert.Zero(t, a.proseCalls)
	assert.JSONEq(t, `{"type":"object","additionalProperties":false,"required":["outcome","explanation"],"properties":{"outcome":{"type":"string","enum":["confirmed","refuted","superseded"]},"explanation":{"type":"string","minLength":1}}}`, string(a.schema))
}

func TestRebuttalEnqueuePreservesOriginalSession(t *testing.T) {
	s, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	parent := testutil.CreateCompletedReview(t, db, repo.ID, "abc123", "test", "The guard is missing.\nAnother finding.")
	originalReview, err := db.GetReviewByJobID(parent.ID)
	require.NoError(t, err)
	require.NotNil(t, originalReview.UUID)
	_, err = db.Exec(`UPDATE review_jobs SET session_id='original-session', model='original-model', provider='original-provider', reasoning='thorough', branch='review-branch', git_ref='review-ref' WHERE id=?`, parent.ID)
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{"job_id": parent.ID, "finding": "The guard is missing.", "comment": "The caller checks it.", "commenter": "operator"})
	require.NoError(t, err)
	_, events := s.broadcaster.Subscribe("")
	rr := serveHuma(t, s, http.MethodPost, "/api/rebuttal", body)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	broadcast := map[string]Event{}
	for len(events) > 0 {
		e := <-events
		broadcast[e.Type] = e
	}
	var job storage.ReviewJob
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &job))
	require.Contains(t, broadcast, "review.commented", "the saved explanation must notify subscribers and hooks")
	assert.Equal(t, parent.ID, broadcast["review.commented"].JobID)
	assert.False(t, broadcast["review.commented"].SuppressHooks)
	require.Contains(t, broadcast, "review.enqueued")
	assert.Equal(t, job.ID, broadcast["review.enqueued"].JobID)
	assert.True(t, broadcast["review.enqueued"].SuppressHooks, "a queued rebuttal must not run review.* hooks")
	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	a := assert.New(t)
	a.Equal("original-session", stored.SessionID)
	a.Equal("original-model", stored.Model)
	a.Equal("original-provider", stored.Provider)
	a.Equal("thorough", stored.Reasoning)
	a.Equal("review-branch", stored.Branch)
	a.Equal("review-ref", stored.GitRef)
	a.Equal("rebuttal", stored.Source)
	a.Equal(storage.JobTypeTask, stored.JobType)
	a.Equal(&parent.ID, stored.ParentJobID)
	a.Equal(originalReview.UUID, job.OriginalReviewUUID)
	a.Contains(stored.Prompt, "The guard is missing.")
	a.Contains(stored.Prompt, "The caller checks it.")
	a.Contains(stored.Prompt, `non-empty "explanation"`)
	a.NotContains(stored.Prompt, "Another finding.")
	a.False(stored.Agentic)
	review, err := db.GetReviewByJobID(parent.ID)
	require.NoError(t, err)
	a.False(review.Closed)
}

func TestRebuttalFindingMatchIgnoresWhitespaceWraps(t *testing.T) {
	s, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	parent := testutil.CreateCompletedReview(t, db, repo.ID, "abc123", "test", "The guard is missing\nat this path.")
	_, err := db.Exec(`UPDATE review_jobs SET session_id='original-session' WHERE id=?`, parent.ID)
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{
		"job_id": parent.ID, "finding": "The guard is missing at this path.",
		"comment": "The sentence was wrapped in the review output.", "commenter": "operator",
	})
	require.NoError(t, err)

	rr := serveHuma(t, s, http.MethodPost, "/api/rebuttal", body)

	assert.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
}

func TestGuardedMutationsReturnNotFoundForMissingJobs(t *testing.T) {
	s, _, _ := newTestServer(t)
	expectedUUID := uuid.New()
	commentBody, err := json.Marshal(AddCommentRequest{
		JobID: 99999, Commenter: "test", Comment: "assessment",
		ExpectedReviewUUID: &expectedUUID,
	})
	require.NoError(t, err)
	comment := serveHuma(t, s, http.MethodPost, "/api/comment", commentBody)
	assert.Equal(t, http.StatusNotFound, comment.Code, comment.Body.String())

	closeBody, err := json.Marshal(CloseReviewRequest{
		JobID: 99999, Closed: true, ExpectedReviewUUID: &expectedUUID,
	})
	require.NoError(t, err)
	closeReview := serveHuma(t, s, http.MethodPost, "/api/review/close", closeBody)
	assert.Equal(t, http.StatusNotFound, closeReview.Code, closeReview.Body.String())
}

func TestRebuttalRejectsAssessmentAndCloseAfterReviewReplacement(t *testing.T) {
	s, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	parent := testutil.CreateCompletedReview(t, db, repo.ID, "abc123", "test", "The guard is missing.")
	review, err := db.GetReviewByJobID(parent.ID)
	require.NoError(t, err)
	require.NotNil(t, review.UUID)
	_, err = db.Exec(`UPDATE review_jobs SET session_id='original-session' WHERE id=?`, parent.ID)
	require.NoError(t, err)

	body := fmt.Appendf(nil, `{"job_id":%d,"finding":"The guard is missing.","comment":"The caller checks it.","commenter":"operator"}`, parent.ID)
	queued := serveHuma(t, s, http.MethodPost, "/api/rebuttal", body)
	require.Equal(t, http.StatusCreated, queued.Code, queued.Body.String())
	var job storage.ReviewJob
	require.NoError(t, json.Unmarshal(queued.Body.Bytes(), &job))
	assert.Equal(t, review.UUID, job.OriginalReviewUUID)

	replacementUUID := uuid.New()
	_, err = db.Exec(`UPDATE reviews SET uuid = ?, closed = 0 WHERE job_id = ?`, replacementUUID, parent.ID)
	require.NoError(t, err)

	commentBody, err := json.Marshal(AddCommentRequest{
		JobID: parent.ID, Commenter: "test", Comment: "stale assessment",
		ExpectedReviewUUID: review.UUID,
	})
	require.NoError(t, err)
	staleComment := serveHuma(t, s, http.MethodPost, "/api/comment", commentBody)
	assert.Equal(t, http.StatusConflict, staleComment.Code, staleComment.Body.String())
	comments, err := db.GetCommentsForJob(parent.ID)
	require.NoError(t, err)
	assert.Len(t, comments, 1)
	assert.Equal(t, "The caller checks it.", comments[0].Response)

	closeBody, err := json.Marshal(CloseReviewRequest{
		JobID: parent.ID, Closed: true, ExpectedReviewUUID: review.UUID,
	})
	require.NoError(t, err)
	staleClose := serveHuma(t, s, http.MethodPost, "/api/review/close", closeBody)
	assert.Equal(t, http.StatusConflict, staleClose.Code, staleClose.Body.String())
	replacement, err := db.GetReviewByJobID(parent.ID)
	require.NoError(t, err)
	assert.Equal(t, replacementUUID, *replacement.UUID)
	assert.False(t, replacement.Closed)

	commentBody, err = json.Marshal(AddCommentRequest{
		JobID: parent.ID, Commenter: "test", Comment: "current assessment",
		ExpectedReviewUUID: &replacementUUID,
	})
	require.NoError(t, err)
	currentComment := serveHuma(t, s, http.MethodPost, "/api/comment", commentBody)
	assert.Equal(t, http.StatusCreated, currentComment.Code, currentComment.Body.String())
	closeBody, err = json.Marshal(CloseReviewRequest{
		JobID: parent.ID, Closed: true, ExpectedReviewUUID: &replacementUUID,
	})
	require.NoError(t, err)
	currentClose := serveHuma(t, s, http.MethodPost, "/api/review/close", closeBody)
	assert.Equal(t, http.StatusOK, currentClose.Code, currentClose.Body.String())
	replacement, err = db.GetReviewByJobID(parent.ID)
	require.NoError(t, err)
	assert.True(t, replacement.Closed)
}

func TestRebuttalInvalidRequestDoesNotQueue(t *testing.T) {
	for _, tc := range []struct{ name, session, finding string }{
		{"missing session", "", "The guard is missing."},
		{"invalid session", "--session", "The guard is missing."},
		{"unselected finding", "original-session", "different finding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db, _ := newTestServer(t)
			repo := testutil.CreateTestRepo(t, db)
			parent := testutil.CreateCompletedReview(t, db, repo.ID, "abc123", "test", "The guard is missing.")
			_, err := db.Exec(`UPDATE review_jobs SET session_id=? WHERE id=?`, tc.session, parent.ID)
			require.NoError(t, err)
			body := fmt.Appendf(nil, `{"job_id":%d,"finding":%q,"comment":"Explanation","commenter":"operator"}`, parent.ID, tc.finding)
			rr := serveHuma(t, s, http.MethodPost, "/api/rebuttal", body)
			assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			var count int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM review_jobs`).Scan(&count))
			assert.Equal(t, 1, count)
		})
	}
}

func TestRebuttalRejectsSessionAgentWithoutReadOnlySupport(t *testing.T) {
	old, err := agent.Get("test")
	require.NoError(t, err)
	agent.Register(&sessionOnlyRebuttalTestAgent{})
	t.Cleanup(func() { agent.Register(old) })

	s, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	parent := testutil.CreateCompletedReview(t, db, repo.ID, "abc123", "test", "The guard is missing.")
	_, err = db.Exec(`UPDATE review_jobs SET session_id='original-session' WHERE id=?`, parent.ID)
	require.NoError(t, err)
	body := fmt.Appendf(nil, `{"job_id":%d,"finding":"The guard is missing.","comment":"The caller checks it.","commenter":"operator"}`, parent.ID)

	rr := serveHuma(t, s, http.MethodPost, "/api/rebuttal", body)

	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "cannot enforce read-only review permissions")
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM review_jobs`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestRebuttalWorkerNeverStartsFreshOrRetries(t *testing.T) {
	for _, tc := range []struct {
		name, session, path string
		fail                bool
	}{
		{name: "missing session"},
		{name: "missing worktree", session: "original-session", path: "missing-checkout"},
		{name: "agent failure", session: "original-session", fail: true},
		{name: "success", session: "original-session"},
		{name: "malformed result", session: "original-session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			env := newWorkerTestContext(t, 1)
			old, err := agent.Get("test")
			require.NoError(t, err)
			t.Cleanup(func() { agent.Register(old) })
			a := agent.NewTestAgent()
			a.Fail = tc.fail
			a.Output = `{"outcome":"refuted","explanation":"Verified the guard rejects the input before this path runs."}`
			if tc.name == "malformed result" {
				a.Output = "invalid assessment"
			}
			agent.Register(a)
			j, err := env.DB.EnqueueJob(storage.EnqueueOpts{RepoID: env.Repo.ID, Agent: "test", JobType: storage.JobTypeTask, Source: "rebuttal", SessionID: tc.session, WorktreePath: tc.path, Prompt: "focused request"})
			require.NoError(t, err)
			claimed, err := env.DB.ClaimJob("worker")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			_, events := env.Broadcaster.Subscribe("")
			env.Pool.processJob("worker", claimed)
			got, err := env.DB.GetJobByID(j.ID)
			require.NoError(t, err)
			terminal := map[string]Event{}
			for len(events) > 0 {
				e := <-events
				assert.True(t, e.SuppressHooks, "rebuttal %s event must not run hooks", e.Type)
				terminal[e.Type] = e
			}
			if tc.name == "success" {
				assert.Equal(t, storage.JobStatusDone, got.Status)
				calls := a.Calls()
				require.Len(t, calls, 1)
				assert.Equal(t, "original-session", calls[0].SessionID)
				assert.Equal(t, "focused request", calls[0].Prompt)
				require.Contains(t, terminal, "review.completed")
				assert.Empty(t, terminal["review.completed"].Verdict, "assessments must not report a review verdict")
			} else {
				assert.Contains(t, terminal, "review.failed")
				assert.Equal(t, storage.JobStatusFailed, got.Status)
				assert.Zero(t, got.RetryCount)
				if !tc.fail && tc.name != "malformed result" {
					assert.Empty(t, a.Calls())
				}
			}
		})
	}
}

func TestRebuttalRejectsIneligibleParents(t *testing.T) {
	for _, field := range []string{"source", "job_type", "agent", "closed", "repo_path", "isolated", "status"} {
		t.Run(field, func(t *testing.T) {
			s, db, _ := newTestServer(t)
			repo := testutil.CreateTestRepo(t, db)
			parent := testutil.CreateCompletedReview(t, db, repo.ID, "abc123", "test", "Missing guard.")
			_, err := db.Exec(`UPDATE review_jobs SET session_id='original-session' WHERE id=?`, parent.ID)
			require.NoError(t, err)
			switch field {
			case "status":
				_, err = db.Exec(`UPDATE review_jobs SET status='running' WHERE id=?`, parent.ID)
			case "isolated":
				_, err = db.Exec(`UPDATE review_jobs SET session_isolated=1 WHERE id=?`, parent.ID)
			case "source":
				_, err = db.Exec(`UPDATE review_jobs SET source='ci' WHERE id=?`, parent.ID)
			case "job_type":
				_, err = db.Exec(`UPDATE review_jobs SET job_type='task' WHERE id=?`, parent.ID)
			case "agent":
				_, err = db.Exec(`UPDATE review_jobs SET agent='missing-agent' WHERE id=?`, parent.ID)
			case "closed":
				err = db.MarkReviewClosedByJobID(parent.ID, true)
			case "repo_path":
				_, err = db.Exec(`UPDATE repos SET root_path=? WHERE id=?`, filepath.Join(t.TempDir(), "missing"), repo.ID)
			}
			require.NoError(t, err)
			body := fmt.Appendf(nil, `{"job_id":%d,"finding":"Missing guard.","comment":"The caller checks it.","commenter":"operator"}`, parent.ID)
			rr := serveHuma(t, s, http.MethodPost, "/api/rebuttal", body)
			assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
		})
	}
}

func TestRebuttalCannotRerunWithoutSession(t *testing.T) {
	s, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	j := testutil.CreateCompletedReview(t, db, repo.ID, "abc123", "test", "original")
	_, err := db.Exec(`UPDATE review_jobs SET source='rebuttal' WHERE id=?`, j.ID)
	require.NoError(t, err)
	rr := serveHuma(t, s, http.MethodPost, "/api/job/rerun", fmt.Appendf(nil, `{"job_id":%d}`, j.ID))
	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
}
