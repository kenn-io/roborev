package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func fakeWhois(access RemoteAccess, err error) whoisFunc {
	return func(context.Context, string) (RemoteCaller, error) {
		return RemoteCaller{Node: "laptop.", Login: "user-a@example.com", Access: access}, err
	}
}

func serveRemote(t *testing.T, s *Server, whois whoisFunc, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.RemoteAddr = "100.64.0.2:5555"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(remoteConnContext(req.Context(), nil))
	w := httptest.NewRecorder()
	s.newRemoteHandler(s.httpServer.Handler, whois).ServeHTTP(w, req)
	return w
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	body, err := json.Marshal(v)
	require.NoError(t, err)
	return body
}

func errorBody(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	return body.Error
}

func TestRemoteHandlerAuthAndAllowlist(t *testing.T) {
	server, _, _ := newTestServer(t)
	tests := []struct {
		name       string
		whois      whoisFunc
		method     string
		target     string
		wantStatus int
		wantError  string
	}{
		{
			"whois failure", fakeWhois(RemoteAccessNone, errors.New("tailscale whois failed for 100.64.0.2:5555: no peer")),
			http.MethodGet, "/api/status", http.StatusForbidden, "tailscale whois failed",
		},
		{
			"no grant", fakeWhois(RemoteAccessNone, nil),
			http.MethodGet, "/api/status", http.StatusForbidden, "tailnet policy grants this node no roborev access",
		},
		{
			"read can read", fakeWhois(RemoteAccessRead, nil),
			http.MethodGet, "/api/status", http.StatusOK, "",
		},
		{
			"read cannot close", fakeWhois(RemoteAccessRead, nil),
			http.MethodPost, "/api/review/close", http.StatusForbidden, "/api/review/close requires queue access; this node has read access",
		},
		{
			"queue denied admin route", fakeWhois(RemoteAccessQueue, nil),
			http.MethodPost, "/api/shutdown", http.StatusForbidden, "/api/shutdown is not available over the remote API; run it on the daemon host",
		},
		{
			"mcp denied", fakeWhois(RemoteAccessQueue, nil),
			http.MethodPost, "/mcp", http.StatusForbidden, "is not available over the remote API",
		},
		{
			"prefix filter denied", fakeWhois(RemoteAccessRead, nil),
			http.MethodGet, "/api/jobs?repo_prefix=/srv", http.StatusBadRequest, "path-prefix filters",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serveRemote(t, server, tt.whois, tt.method, tt.target, nil)
			require.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			if tt.wantError != "" {
				assert.Contains(t, errorBody(t, w), tt.wantError)
			}
		})
	}
}

func TestRemoteHandlerRetriesFailedWhoisOnSameConnection(t *testing.T) {
	server, _, _ := newTestServer(t)
	calls := 0
	whois := func(context.Context, string) (RemoteCaller, error) {
		calls++
		if calls == 1 {
			return RemoteCaller{}, errors.New("tailscale whois failed: context canceled")
		}
		return RemoteCaller{Node: "laptop.", Access: RemoteAccessRead}, nil
	}
	handler := server.newRemoteHandler(server.httpServer.Handler, whois)
	connCtx := remoteConnContext(context.Background(), nil)
	serve := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil).WithContext(connCtx)
		req.RemoteAddr = "100.64.0.2:5555"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	assert := assert.New(t)

	assert.Equal(http.StatusForbidden, serve().Code, "first whois fails")
	assert.Equal(http.StatusOK, serve().Code, "failed whois is retried on the same connection")
	assert.Equal(http.StatusOK, serve().Code)
	assert.Equal(2, calls, "a successful whois is cached for the connection")
}

func TestRemoteHandlerRepoFilters(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "project")
	testutil.InitTestGitRepo(t, repoDir)
	const id = "https://example.com/org/project.git"
	_, err := db.GetOrCreateRepo(repoDir, id)
	require.NoError(t, err)
	read := fakeWhois(RemoteAccessRead, nil)

	w := serveRemote(t, server, read, http.MethodGet, "/api/jobs?repo="+id, nil)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = serveRemote(t, server, read, http.MethodGet, "/api/jobs?repo="+repoDir, nil)
	assert.Equal(t, http.StatusOK, w.Code, "registered root_path passes through: "+w.Body.String())

	w = serveRemote(t, server, read, http.MethodGet, "/api/jobs?repo=https://example.com/org/unknown.git", nil)
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, errorBody(t, w), "is not registered on the daemon host")

	w = serveRemote(t, server, read, http.MethodGet, "/api/jobs?repo=", nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, errorBody(t, w), "repo identity is required")
}

func TestRemoteHandlerRepoIdentityResolution(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	const id = "https://example.com/org/dup.git"
	_, err := db.GetOrCreateRepoByIdentity(id) // sync placeholder: never matches
	require.NoError(t, err)
	queue := fakeWhois(RemoteAccessQueue, nil)
	body := mustJSON(t, EnqueueRequest{RepoIdentity: id, GitRef: shaA, Agent: "test"})

	w := serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", body)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	for _, name := range []string{"a", "b"} {
		dir := filepath.Join(tmpDir, name)
		testutil.InitTestGitRepo(t, dir)
		_, err := db.GetOrCreateRepo(dir, id)
		require.NoError(t, err)
	}
	w = serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", body)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, errorBody(t, w), "matches several daemon checkouts")
}

func TestRemoteEnqueueValidation(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "project")
	repo := testutil.InitTestGitRepo(t, repoDir)
	head := repo.CommitFile("a.txt", "a", "a")
	const id = "https://example.com/org/project.git"
	_, err := db.GetOrCreateRepo(repoDir, id)
	require.NoError(t, err)
	queue := fakeWhois(RemoteAccessQueue, nil)

	tests := []struct {
		name       string
		req        EnqueueRequest
		wantStatus int
		wantError  string
	}{
		{"repo_path rejected", EnqueueRequest{RepoPath: repoDir, RepoIdentity: id, GitRef: head}, http.StatusBadRequest, "repo_path is not accepted"},
		{"identity required", EnqueueRequest{GitRef: head}, http.StatusBadRequest, "repo_identity is required"},
		{"dirty", EnqueueRequest{RepoIdentity: id, GitRef: "dirty", DiffContent: "diff"}, http.StatusForbidden, "dirty reviews need a local daemon"},
		{"custom prompt", EnqueueRequest{RepoIdentity: id, GitRef: head, CustomPrompt: "do things"}, http.StatusForbidden, "task reviews need a local daemon"},
		{"agentic", EnqueueRequest{RepoIdentity: id, GitRef: head, Agentic: true}, http.StatusForbidden, "agentic reviews need a local daemon"},
		{"fix job type", EnqueueRequest{RepoIdentity: id, GitRef: head, JobType: storage.JobTypeFix}, http.StatusForbidden, "fix reviews need a local daemon"},
		{"insights job type", EnqueueRequest{RepoIdentity: id, GitRef: head, JobType: storage.JobTypeInsights}, http.StatusForbidden, "insights reviews need a local daemon"},
		{"insights since", EnqueueRequest{RepoIdentity: id, GitRef: head, Since: "2026-01-01T00:00:00Z"}, http.StatusForbidden, "insights reviews need a local daemon"},
		{"analyze", EnqueueRequest{RepoIdentity: id, GitRef: head, AnalysisType: "refactor"}, http.StatusForbidden, "analyze reviews need a local daemon"},
		{"compact job type", EnqueueRequest{RepoIdentity: id, GitRef: head, JobType: storage.JobTypeCompact}, http.StatusForbidden, "compact reviews need a local daemon"},
		{"symbolic ref", EnqueueRequest{RepoIdentity: id, GitRef: "HEAD"}, http.StatusBadRequest, "remote enqueue needs full commit SHAs"},
		{"bad branch", EnqueueRequest{RepoIdentity: id, GitRef: head, Branch: "feat..x"}, http.StatusBadRequest, "invalid branch name"},
		{"dash branch", EnqueueRequest{RepoIdentity: id, GitRef: head, Branch: "-x"}, http.StatusBadRequest, "invalid branch name"},
		{"previous branch shorthand", EnqueueRequest{RepoIdentity: id, GitRef: head, Branch: "@{-1}"}, http.StatusBadRequest, "invalid branch name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", mustJSON(t, tt.req))
			require.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			assert.Contains(t, errorBody(t, w), tt.wantError)
		})
	}
}

func TestRemoteEnqueueMissingCommitsAndSuccess(t *testing.T) {
	server, db, _ := newTestServer(t)
	f := newRemoteGitFixture(t)
	const id = "https://example.com/org/project.git"
	_, err := db.GetOrCreateRepo(f.daemonDir, id)
	require.NoError(t, err)
	queue := fakeWhois(RemoteAccessQueue, nil)
	body := mustJSON(t, EnqueueRequest{RepoIdentity: id, GitRef: f.unpushed, Branch: "feature-x", Agent: "test"})

	w := serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", body)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var missing MissingCommitsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &missing))
	assert.Equal(t, MissingCommitsCode, missing.Code)
	assert.Equal(t, []string{f.unpushed}, missing.Missing)
	assert.Contains(t, missing.Have, gitOut(t, f.daemonDir, "rev-parse", "HEAD"))

	pack := buildPack(t, f.laptopDir, missing.Missing, missing.Have)
	w = serveRemote(t, server, queue, http.MethodPost,
		RemotePackPath+"?repo_identity="+id+"&tip="+f.unpushed, pack)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", body)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var job storage.ReviewJob
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &job))
	assert.Equal(t, "feature-x", job.Branch, "request branch wins over the daemon checkout branch")
}

func TestRemoteEnqueueEmptyBranchIgnoresCheckoutBranch(t *testing.T) {
	server, db, _ := newTestServer(t)
	f := newRemoteGitFixture(t)
	gitOut(t, f.daemonDir, "checkout", "-q", "-b", "daemon-local")
	const id = "https://example.com/org/project.git"
	_, err := db.GetOrCreateRepo(f.daemonDir, id)
	require.NoError(t, err)
	queue := fakeWhois(RemoteAccessQueue, nil)
	pack := buildPack(t, f.laptopDir, []string{f.unpushed}, []string{gitOut(t, f.daemonDir, "rev-parse", "HEAD")})
	w := serveRemote(t, server, queue, http.MethodPost,
		RemotePackPath+"?repo_identity="+id+"&tip="+f.unpushed, pack)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	body := mustJSON(t, EnqueueRequest{RepoIdentity: id, GitRef: f.unpushed, Agent: "test"})
	w = serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", body)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var job storage.ReviewJob
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &job))
	// The uploaded commit is on no daemon branch, so inference finds none,
	// and the daemon checkout's own branch must never be used.
	assert.Empty(t, job.Branch)
}

func TestRemoteEnqueueRootInclusiveRange(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "project")
	repo := testutil.InitTestGitRepo(t, repoDir)
	root := repo.HeadSHA() // InitTestGitRepo's initial commit has no parent
	head := repo.CommitFile("b.txt", "b", "second")
	const id = "https://example.com/org/project.git"
	_, err := db.GetOrCreateRepo(repoDir, id)
	require.NoError(t, err)

	body := mustJSON(t, EnqueueRequest{RepoIdentity: id, GitRef: root + "^.." + head, Branch: "main", Agent: "test"})
	w := serveRemote(t, server, fakeWhois(RemoteAccessQueue, nil), http.MethodPost, "/api/enqueue", body)
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

func TestRemoteRerunEligibility(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	// Same setup as TestHumaRerunJob: tmpDir is a real directory for rerun
	// validation, and failed jobs are rerunnable.
	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "deadbeef", "A", "S", time.Now())
	require.NoError(t, err)
	review, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: "deadbeef", Agent: "test",
	})
	require.NoError(t, err)
	task, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "deadbeef", Agent: "test",
		JobType: storage.JobTypeTask, Prompt: "hello",
	})
	require.NoError(t, err)
	for range 2 {
		claimed, err := db.ClaimJob("w")
		require.NoError(t, err)
		_, err = db.FailJob(claimed.ID, "", "some error")
		require.NoError(t, err)
	}
	queue := fakeWhois(RemoteAccessQueue, nil)

	body := mustJSON(t, map[string]any{"job_id": task.ID})
	w := serveRemote(t, server, queue, http.MethodPost, "/api/job/rerun", body)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, errorBody(t, w), "needs a local daemon")

	body = mustJSON(t, map[string]any{"job_id": review.ID})
	w = serveRemote(t, server, queue, http.MethodPost, "/api/job/rerun", body)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	runUUID, members, synth := enqueueServerPanelRun(t, db, 2)
	markPanelMembersStatus(t, db, runUUID, storage.JobStatusDone)
	markJobStatus(t, db, synth.ID, storage.JobStatusDone)
	body = mustJSON(t, map[string]any{"job_id": synth.ID})
	w = serveRemote(t, server, queue, http.MethodPost, "/api/job/rerun", body)
	assert.Equal(t, http.StatusOK, w.Code, "panel of plain reviews reruns: "+w.Body.String())

	_, err = db.Exec("UPDATE review_jobs SET agentic = 1 WHERE id = ?", members[0].ID)
	require.NoError(t, err)
	w = serveRemote(t, server, queue, http.MethodPost, "/api/job/rerun", body)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, errorBody(t, w), "needs a local daemon")
}

func TestRemoteRerunRefusesLocalOnlyJobs(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "deadbeef", "A", "S", time.Now())
	require.NoError(t, err)
	queue := fakeWhois(RemoteAccessQueue, nil)

	tests := []struct {
		name string
		opts storage.EnqueueOpts
	}{
		{"agentic review", storage.EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: "deadbeef", Agent: "test", Agentic: true,
		}},
		{"dirty review", storage.EnqueueOpts{
			RepoID: repo.ID, GitRef: "dirty", Agent: "test",
			JobType: storage.JobTypeDirty, DiffContent: "diff --git a/x b/x",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job, err := db.EnqueueJob(tt.opts)
			require.NoError(t, err)
			markJobStatus(t, db, job.ID, storage.JobStatusFailed)

			w := serveRemote(t, server, queue, http.MethodPost, "/api/job/rerun",
				mustJSON(t, map[string]any{"job_id": job.ID}))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			assert.Contains(t, errorBody(t, w), "needs a local daemon")
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusFailed, stored.Status, "refused rerun leaves the job alone")
		})
	}
}

func TestRemoteRerunStopsOnJobLookupError(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "deadbeef", Agent: "test",
		JobType: storage.JobTypeTask, Prompt: "hello",
	})
	require.NoError(t, err)
	// A value that cannot scan into the agentic column makes GetJobByID
	// fail with an error other than sql.ErrNoRows.
	_, err = db.Exec("UPDATE review_jobs SET agentic = 'not-a-number' WHERE id = ?", job.ID)
	require.NoError(t, err)

	w := serveRemote(t, server, fakeWhois(RemoteAccessQueue, nil), http.MethodPost, "/api/job/rerun",
		mustJSON(t, map[string]any{"job_id": job.ID}))
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	assert.Contains(t, errorBody(t, w), "look up job", "the remote gate answers, not the core handler")
}

func TestRemoteEnqueueExcludedBranches(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "project")
	repo := testutil.InitTestGitRepo(t, repoDir)
	repo.CheckoutNewBranch("wip-feature")
	head := repo.CommitFile("a.txt", "a", "a")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".roborev.toml"),
		[]byte(`excluded_branches = ["wip-feature"]`), 0o644))
	const id = "https://example.com/org/project.git"
	_, err := db.GetOrCreateRepo(repoDir, id)
	require.NoError(t, err)
	queue := fakeWhois(RemoteAccessQueue, nil)

	w := serveRemote(t, server, queue, http.MethodPost, "/api/enqueue",
		mustJSON(t, EnqueueRequest{RepoIdentity: id, GitRef: head, Branch: "wip-feature", Agent: "test"}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var skipped EnqueueSkippedResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &skipped))
	assert.True(t, skipped.Skipped, "the reported branch is excluded")

	w = serveRemote(t, server, queue, http.MethodPost, "/api/enqueue",
		mustJSON(t, EnqueueRequest{RepoIdentity: id, GitRef: head, Branch: "feature-x", Agent: "test"}))
	assert.Equal(t, http.StatusCreated, w.Code,
		"the daemon checkout's excluded branch does not apply: "+w.Body.String())
}

func TestRemotePackUploadErrors(t *testing.T) {
	server, db, _ := newTestServer(t)
	f := newRemoteGitFixture(t)
	const id = "https://example.com/org/project.git"
	_, err := db.GetOrCreateRepo(f.daemonDir, id)
	require.NoError(t, err)
	queue := fakeWhois(RemoteAccessQueue, nil)

	// A second unpushed commit whose pack leaves out its parent, which the
	// daemon clone also lacks.
	gitOut(t, f.laptopDir, "commit", "-q", "--allow-empty", "-m", "second unpushed")
	second := gitOut(t, f.laptopDir, "rev-parse", "HEAD")
	thinPack := buildPack(t, f.laptopDir, []string{second}, []string{f.unpushed})

	tests := []struct {
		name       string
		query      string
		pack       []byte
		wantStatus int
		wantError  string
	}{
		{"no identity", "?tip=" + second, thinPack, http.StatusBadRequest, "repo identity is required"},
		{"no tip", "?repo_identity=" + id, thinPack, http.StatusBadRequest, "at least one tip is required"},
		{"short tip", "?repo_identity=" + id + "&tip=" + second[:12], thinPack, http.StatusBadRequest, "is not a full commit SHA"},
		{"missing base", "?repo_identity=" + id + "&tip=" + second, thinPack, http.StatusConflict, "lacks base commits"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := serveRemote(t, server, queue, http.MethodPost, RemotePackPath+tt.query, tt.pack)
			require.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			assert.Contains(t, errorBody(t, w), tt.wantError)
		})
	}
}
