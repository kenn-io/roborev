package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func TestRemoteRepoIdentityRejectsLocalFallback(t *testing.T) {
	repo := newTestGitRepo(t)
	_, err := remoteRepoIdentity(repo.Dir)
	require.ErrorContains(t, err, ".roborev-id")

	require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, ".roborev-id"), []byte("example/project\n"), 0o644))
	id, err := remoteRepoIdentity(repo.Dir)
	require.NoError(t, err)
	assert.Equal(t, "example/project", id)
}

func TestResolveRemoteGitRef(t *testing.T) {
	assert := assert.New(t)
	repo := newTestGitRepo(t)
	first := repo.CommitFile("a.txt", "a", "first")
	second := repo.CommitFile("b.txt", "b", "second")
	ctx := context.Background()

	got, err := resolveRemoteGitRef(ctx, repo.Dir, "HEAD")
	require.NoError(t, err)
	assert.Equal(second, got)

	got, err = resolveRemoteGitRef(ctx, repo.Dir, first+"^..HEAD")
	require.NoError(t, err)
	assert.Equal(first+"^.."+second, got)

	got, err = resolveRemoteGitRef(ctx, repo.Dir, "HEAD~1..HEAD")
	require.NoError(t, err)
	assert.Equal(first+".."+second, got)
}

// TestRemoteEnqueueUploadsFromForkRemote covers the case where the commit
// sits on a client remote-tracking ref for a remote the daemon lacks.
func TestRemoteEnqueueUploadsFromForkRemote(t *testing.T) {
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", upstream)
	seed := newTestGitRepo(t)
	seed.CommitFile("base.txt", "base", "base")
	gitIn(t, seed.Dir, "push", "-q", upstream, "HEAD:refs/heads/main")

	daemonClone := filepath.Join(root, "daemon")
	laptop := filepath.Join(root, "laptop")
	fork := filepath.Join(root, "fork.git")
	gitIn(t, root, "clone", "-q", upstream, daemonClone)
	gitIn(t, root, "clone", "-q", upstream, laptop)
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", fork)
	require.NoError(t, os.WriteFile(filepath.Join(laptop, ".roborev-id"), []byte("example/project\n"), 0o644))
	gitIn(t, laptop, "add", ".roborev-id")
	gitIn(t, laptop, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "on fork")
	gitIn(t, laptop, "remote", "add", "fork", fork)
	gitIn(t, laptop, "push", "-q", "fork", "HEAD:refs/heads/w")
	gitIn(t, laptop, "fetch", "-q", "fork")
	gitIn(t, laptop, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "unpushed")
	target := gitIn(t, laptop, "rev-parse", "HEAD")
	onFork := gitIn(t, laptop, "rev-parse", "HEAD~1")

	var mu sync.Mutex
	var enqueues []daemon.EnqueueRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		mu.Lock()
		enqueues = append(enqueues, req)
		mu.Unlock()
		if gitCatFileOK(daemonClone, target) {
			respondJSON(w, http.StatusCreated, map[string]any{"id": 7})
			return
		}
		haves := strings.Fields(gitIn(t, daemonClone, "for-each-ref", "--format=%(objectname)"))
		respondJSON(w, http.StatusConflict, daemon.MissingCommitsResponse{
			Error: "missing", Code: daemon.MissingCommitsCode, Missing: []string{target}, Have: haves,
		})
	})
	mux.HandleFunc(daemon.RemotePackPath, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "example/project", r.URL.Query().Get("repo_identity"))
		assert.Equal(t, []string{target}, r.URL.Query()["tip"])
		cmd := exec.Command("git", "-C", daemonClone, "index-pack", "--stdin")
		cmd.Stdin = r.Body
		out, err := cmd.CombinedOutput()
		assert.NoError(t, err, string(out))
		gitIn(t, daemonClone, "update-ref", "refs/roborev/uploads/"+target, target)
		respondJSON(w, http.StatusOK, map[string]any{"pinned": []string{target}})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	ep := daemon.DaemonEndpoint{Network: "tcp", Address: strings.TrimPrefix(ts.URL, "http://")}

	status, body, err := remoteEnqueue(context.Background(), ep, ts.Client(), laptop,
		daemon.EnqueueRequest{GitRef: "HEAD", Branch: "main", Source: "post_commit"})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status, string(body))
	require.Len(t, enqueues, 2)
	assert.Equal(t, "example/project", enqueues[0].RepoIdentity)
	assert.Empty(t, enqueues[0].RepoPath)
	assert.Equal(t, target, enqueues[0].GitRef)
	assert.True(t, gitCatFileOK(daemonClone, onFork), "fork-only ancestor was packed")
}

func gitCatFileOK(dir, sha string) bool {
	return exec.Command("git", "-C", dir, "cat-file", "-e", sha+"^{commit}").Run() == nil
}

// withRemoteMock puts the CLI in remote mode against a loopback server
// running mux. The ping probe is stubbed so ensureDaemon succeeds.
func withRemoteMock(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	withRemoteState(t)
	probeRemoteDaemon = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return &daemon.PingInfo{OK: true, Service: "roborev"}, nil
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	setRemoteEndpoint(&daemon.DaemonEndpoint{Network: "tcp", Address: strings.TrimPrefix(ts.URL, "http://")})
}

// captureRemoteEnqueues answers every enqueue with 201 and records it.
func captureRemoteEnqueues(t *testing.T, mux *http.ServeMux) chan daemon.EnqueueRequest {
	t.Helper()
	requests := make(chan daemon.EnqueueRequest, 10)
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		requests <- req
		respondJSON(w, http.StatusCreated, map[string]any{"id": 1})
	})
	return requests
}

func writeRoborevID(t *testing.T, repo *TestGitRepo) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, ".roborev-id"), []byte("example/project\n"), 0o644))
}

func TestReviewRemoteSendsIdentityAndFullSHA(t *testing.T) {
	assert := assert.New(t)
	mux := http.NewServeMux()
	requests := captureRemoteEnqueues(t, mux)
	withRemoteMock(t, mux)
	repo := newTestGitRepo(t)
	head := repo.CommitFile("file.txt", "content", "initial")
	writeRoborevID(t, repo)

	_, _, err := executeReviewCmd("--repo", repo.Dir, "--agent", "test", "--quiet")
	require.NoError(t, err)

	req := <-requests
	assert.Equal("example/project", req.RepoIdentity)
	assert.Empty(req.RepoPath)
	assert.Equal(head, req.GitRef)
}

func TestReviewRemoteBranchSendsTargetBranch(t *testing.T) {
	assert := assert.New(t)
	mux := http.NewServeMux()
	requests := captureRemoteEnqueues(t, mux)
	withRemoteMock(t, mux)
	repo := newTestGitRepo(t)
	repo.SetHeadBranch("main")
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	featureHead := repo.CommitFile("feature.txt", "feature", "feature")
	repo.Run("checkout", "-q", "main")
	writeRoborevID(t, repo)

	_, _, err := executeReviewCmd("--repo", repo.Dir, "--branch=feature", "--base", "main", "--agent", "test", "--quiet")
	require.NoError(t, err)

	req := <-requests
	assert.Equal("feature", req.Branch)
	assert.Equal(base+".."+featureHead, req.GitRef)
	assert.Equal("example/project", req.RepoIdentity)
}

func withHookLog(t *testing.T) string {
	t.Helper()
	logFile := filepath.Join(t.TempDir(), "post-commit.log")
	old := hookLogPath
	hookLogPath = logFile
	t.Cleanup(func() { hookLogPath = old })
	return logFile
}

func TestPostCommitRemoteEnqueues(t *testing.T) {
	assert := assert.New(t)
	logFile := withHookLog(t)
	mux := http.NewServeMux()
	requests := captureRemoteEnqueues(t, mux)
	withRemoteMock(t, mux)
	repo := newTestGitRepo(t)
	head := repo.CommitFile("file.txt", "content", "initial")
	writeRoborevID(t, repo)

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-requests
	assert.Equal("example/project", req.RepoIdentity)
	assert.Empty(req.RepoPath)
	assert.Equal(head, req.GitRef)
	assert.Equal("post_commit", req.Source)
	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(string(data), `"outcome":"ok"`)
}

func TestPostCommitRemoteLogsDaemonError(t *testing.T) {
	logFile := withHookLog(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "repo example/project is not registered", http.StatusNotFound)
	})
	withRemoteMock(t, mux)
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial")
	writeRoborevID(t, repo)

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"outcome":"fail"`)
	assert.Contains(t, string(data), "daemon returned 404")
}

// TestPostCommitRemoteTimeoutKeepsBatch checks that a stalled remote daemon
// is bounded by the hook timeout and leaves the batch unadvanced, so the
// next commit retries the whole range.
func TestPostCommitRemoteTimeoutKeepsBatch(t *testing.T) {
	assert := assert.New(t)
	logFile := withHookLog(t)
	var stall atomic.Bool
	stall.Store(true)
	requests := make(chan daemon.EnqueueRequest, 10)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		assert.NoError(json.NewDecoder(r.Body).Decode(&req))
		requests <- req
		if stall.Load() {
			<-r.Context().Done()
			return
		}
		respondJSON(w, http.StatusCreated, map[string]any{"id": 1})
	})
	withRemoteMock(t, mux)
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	writeRoborevID(t, repo)
	writeRoborevConfig(t, repo, "post_commit_batch_size = 2\nhook_timeout_seconds = 1\n")
	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.CommitFile("two.txt", "two", "two")

	// Wall clock: the hook waits on a real socket and git subprocesses,
	// which testing/synctest cannot observe.
	start := time.Now()
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	assert.Less(time.Since(start), 5*time.Second, "hook must stop at its timeout")
	<-requests
	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(string(data), `"outcome":"fail"`)

	stall.Store(false)
	head := repo.CommitFile("three.txt", "three", "three")
	_, _, err = executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	retried := <-requests
	assert.Equal(base+".."+head, retried.GitRef)
}
