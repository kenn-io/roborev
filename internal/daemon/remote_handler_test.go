package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
}

func TestRemoteHandlerRepoIdentityResolution(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	const id = "https://example.com/org/dup.git"
	_, err := db.GetOrCreateRepoByIdentity(id) // sync placeholder: never matches
	require.NoError(t, err)
	queue := fakeWhois(RemoteAccessQueue, nil)
	body, _ := json.Marshal(EnqueueRequest{RepoIdentity: id, GitRef: shaA, Agent: "test"})

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
		{"symbolic ref", EnqueueRequest{RepoIdentity: id, GitRef: "HEAD"}, http.StatusBadRequest, "remote enqueue needs full commit SHAs"},
		{"bad branch", EnqueueRequest{RepoIdentity: id, GitRef: head, Branch: "feat..x"}, http.StatusBadRequest, "invalid branch name"},
		{"dash branch", EnqueueRequest{RepoIdentity: id, GitRef: head, Branch: "-x"}, http.StatusBadRequest, "invalid branch name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.req)
			w := serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", body)
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
	body, _ := json.Marshal(EnqueueRequest{RepoIdentity: id, GitRef: f.unpushed, Branch: "feature-x", Agent: "test"})

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

	body, _ := json.Marshal(EnqueueRequest{RepoIdentity: id, GitRef: f.unpushed, Agent: "test"})
	w = serveRemote(t, server, queue, http.MethodPost, "/api/enqueue", body)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var job storage.ReviewJob
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &job))
	// Inference against the clone's refs may name a branch or leave it
	// empty; the daemon checkout's own branch must never be used.
	assert.NotEqual(t, "daemon-local", job.Branch)
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

	body, _ := json.Marshal(EnqueueRequest{RepoIdentity: id, GitRef: root + "^.." + head, Branch: "main", Agent: "test"})
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

	body, _ := json.Marshal(map[string]any{"job_id": task.ID})
	w := serveRemote(t, server, queue, http.MethodPost, "/api/job/rerun", body)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, errorBody(t, w), "needs a local daemon")

	body, _ = json.Marshal(map[string]any{"job_id": review.ID})
	w = serveRemote(t, server, queue, http.MethodPost, "/api/job/rerun", body)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	runUUID, members, synth := enqueueServerPanelRun(t, db, 2)
	markPanelMembersStatus(t, db, runUUID, storage.JobStatusDone)
	markJobStatus(t, db, synth.ID, storage.JobStatusDone)
	body, _ = json.Marshal(map[string]any{"job_id": synth.ID})
	w = serveRemote(t, server, queue, http.MethodPost, "/api/job/rerun", body)
	assert.Equal(t, http.StatusOK, w.Code, "panel of plain reviews reruns: "+w.Body.String())

	_, err = db.Exec("UPDATE review_jobs SET agentic = 1 WHERE id = ?", members[0].ID)
	require.NoError(t, err)
	w = serveRemote(t, server, queue, http.MethodPost, "/api/job/rerun", body)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, errorBody(t, w), "needs a local daemon")
}
