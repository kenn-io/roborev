package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

// runGit runs git without failing the test, for HTTP handler goroutines
// where require cannot stop the test.
func runGit(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// handlerErrors records failures in HTTP handler goroutines. The test
// goroutine asserts them at cleanup, after the servers have closed.
type handlerErrors struct {
	mu   sync.Mutex
	errs []error
}

func newHandlerErrors(t *testing.T) *handlerErrors {
	t.Helper()
	h := &handlerErrors{}
	t.Cleanup(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		assert.Empty(t, h.errs, "HTTP handler errors")
	})
	return h
}

// add records err if it is not nil and reports whether it was nil.
func (h *handlerErrors) add(err error) bool {
	if err == nil {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errs = append(h.errs, err)
	return false
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

func TestResolveRemoteGitRefRejectsSymmetricRange(t *testing.T) {
	// A directory that is not a repo: any git call would fail with a
	// different error, so this proves the check runs before git.
	_, err := resolveRemoteGitRef(context.Background(), t.TempDir(), "main...feature")
	require.ErrorContains(t, err, `"main...feature"`)
	assert.ErrorContains(t, err, "symmetric ranges (A...B) are not supported with a remote daemon")
}

func TestResolveRemoteGitRefReportsGitFailures(t *testing.T) {
	// Not a repo: rev-parse exits 128, which is not "not a commit".
	_, err := resolveRemoteGitRef(context.Background(), t.TempDir(), "HEAD")
	require.ErrorContains(t, err, "exit status 128")
	assert.NotContains(t, err.Error(), "not a commit")

	repo := newTestGitRepo(t)
	repo.CommitFile("a.txt", "a", "first")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = resolveRemoteGitRef(ctx, repo.Dir, "HEAD")
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), "not a commit")
}

func TestResolveRemoteGitRefRejectsBadSides(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("a.txt", "a", "first")
	tests := []struct {
		ref     string
		wantErr string
	}{
		{ref: "..HEAD", wantErr: "empty side"},
		{ref: "HEAD..", wantErr: "empty side"},
		{ref: "^..HEAD", wantErr: "empty side"},
		{ref: "--all", wantErr: "resolve --all: not a commit"},
		{ref: "HEAD..--all", wantErr: "resolve --all: not a commit"},
		{ref: "--output=out.txt", wantErr: "not a commit"},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			_, err := resolveRemoteGitRef(context.Background(), repo.Dir, tt.ref)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
	assert.NoFileExists(t, filepath.Join(repo.Dir, "out.txt"))
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

	// A daemon-only branch: the daemon reports its tip as a have, and the
	// laptop never fetched it, so the client must leave it out of the pack.
	gitIn(t, daemonClone, "checkout", "-q", "-b", "daemon-only")
	gitIn(t, daemonClone, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "daemon only")
	daemonOnly := gitIn(t, daemonClone, "rev-parse", "HEAD")
	gitIn(t, daemonClone, "checkout", "-q", "main")
	require.False(t, gitCatFileOK(laptop, daemonOnly))

	herrs := newHandlerErrors(t)
	var mu sync.Mutex
	var enqueues []daemon.EnqueueRequest
	var packQueries []url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		if !herrs.add(json.NewDecoder(r.Body).Decode(&req)) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		enqueues = append(enqueues, req)
		mu.Unlock()
		if gitCatFileOK(daemonClone, target) {
			respondJSON(w, http.StatusCreated, map[string]any{"id": 7})
			return
		}
		refs, err := runGit(daemonClone, "for-each-ref", "--format=%(objectname)")
		if !herrs.add(err) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		respondJSON(w, http.StatusConflict, daemon.MissingCommitsResponse{
			Error: "missing", Code: daemon.MissingCommitsCode, Missing: []string{target}, Have: strings.Fields(refs),
		})
	})
	mux.HandleFunc(daemon.RemotePackPath, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		packQueries = append(packQueries, r.URL.Query())
		mu.Unlock()
		// Import the way the daemon does: index the pack, then require
		// every object reachable from the tip, so an incomplete pack fails.
		cmd := exec.Command("git", "-C", daemonClone, "index-pack", "--stdin")
		cmd.Stdin = r.Body
		out, err := cmd.CombinedOutput()
		if err != nil {
			herrs.add(fmt.Errorf("index-pack: %w: %s", err, out))
			http.Error(w, "bad pack", http.StatusBadRequest)
			return
		}
		if _, err := runGit(daemonClone, "rev-list", "--quiet", "--objects", target, "--not", "--all"); err != nil {
			herrs.add(fmt.Errorf("pack is incomplete: %w", err))
			http.Error(w, "incomplete pack", http.StatusConflict)
			return
		}
		if _, err := runGit(daemonClone, "update-ref", "refs/roborev/uploads/"+target, target); !herrs.add(err) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"pinned": []string{target}})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	ep := daemon.DaemonEndpoint{Network: "tcp", Address: strings.TrimPrefix(ts.URL, "http://")}

	status, body, err := remoteEnqueue(context.Background(), ep, ts.Client(), laptop,
		daemon.EnqueueRequest{GitRef: "HEAD", Branch: "main", Source: "post_commit"})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status, string(body))
	assert := assert.New(t)
	require.Len(t, enqueues, 2)
	assert.Equal("example/project", enqueues[0].RepoIdentity)
	assert.Empty(enqueues[0].RepoPath)
	assert.Equal(target, enqueues[0].GitRef)
	require.Len(t, packQueries, 1)
	assert.Equal("example/project", packQueries[0].Get("repo_identity"))
	assert.Equal([]string{target}, packQueries[0]["tip"])
	assert.True(gitCatFileOK(daemonClone, onFork), "fork-only ancestor was packed")
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
	return captureRemoteEnqueuesWith(t, mux, func(w http.ResponseWriter) {
		respondJSON(w, http.StatusCreated, map[string]any{"id": 1})
	})
}

// captureRemoteEnqueuesWith records every enqueue and answers with reply.
func captureRemoteEnqueuesWith(
	t *testing.T, mux *http.ServeMux, reply func(http.ResponseWriter),
) chan daemon.EnqueueRequest {
	t.Helper()
	herrs := newHandlerErrors(t)
	requests := make(chan daemon.EnqueueRequest, 10)
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		if !herrs.add(json.NewDecoder(r.Body).Decode(&req)) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		requests <- req
		reply(w)
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
	assert := assert.New(t)
	logFile := withHookLog(t)
	mux := http.NewServeMux()
	requests := captureRemoteEnqueuesWith(t, mux, func(w http.ResponseWriter) {
		http.Error(w, "repo example/project is not registered", http.StatusNotFound)
	})
	withRemoteMock(t, mux)
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial")
	writeRoborevID(t, repo)

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-requests
	assert.Equal("example/project", req.RepoIdentity)
	assert.Empty(req.RepoPath)
	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(string(data), `"outcome":"fail"`)
	assert.Contains(string(data), "daemon returned 404")
}

// inProcessTransport serves each request with handler in this process, so
// a synctest bubble owns every timer and channel the request touches.
type inProcessTransport struct{ handler http.Handler }

func (tr inProcessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil {
		req.Body = http.NoBody
	}
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.handler.ServeHTTP(rec, req)
	}()
	select {
	case <-done:
		return rec.Result(), nil
	case <-req.Context().Done():
		<-done
		return nil, req.Context().Err()
	}
}

// withInProcessRemote puts the CLI in remote mode and sends hook requests
// to handler in process. The ping probe is stubbed so ensureDaemon succeeds.
func withInProcessRemote(t *testing.T, handler http.Handler) daemon.DaemonEndpoint {
	t.Helper()
	withRemoteState(t)
	probeRemoteDaemon = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return &daemon.PingInfo{OK: true, Service: "roborev"}, nil
	}
	ep := daemon.DaemonEndpoint{Network: "tcp", Address: "daemon-host.example:7474"}
	setRemoteEndpoint(&ep)
	orig := hookHTTPClient
	hookHTTPClient = func(timeout time.Duration) *http.Client {
		return &http.Client{Timeout: timeout, Transport: inProcessTransport{handler: handler}}
	}
	t.Cleanup(func() { hookHTTPClient = orig })
	return ep
}

// TestPostCommitRemoteTimeoutKeepsBatch checks that a stalled remote daemon
// is bounded by the hook timeout and leaves the batch unadvanced, so the
// next commit retries the whole range.
func TestPostCommitRemoteTimeoutKeepsBatch(t *testing.T) {
	assert := assert.New(t)
	logFile := withHookLog(t)
	herrs := newHandlerErrors(t)
	var stall atomic.Bool
	stall.Store(true)
	requests := make(chan daemon.EnqueueRequest, 10)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		if !herrs.add(json.NewDecoder(r.Body).Decode(&req)) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		requests <- req
		if stall.Load() {
			<-r.Context().Done()
			return
		}
		respondJSON(w, http.StatusCreated, map[string]any{"id": 1})
	})
	withInProcessRemote(t, mux)
	repo := newTestGitRepo(t)
	base := repo.CommitFile("base.txt", "base", "base")
	repo.CheckoutNewBranch("feature")
	writeRoborevID(t, repo)
	writeRoborevConfig(t, repo, "post_commit_batch_size = 2\nhook_timeout_seconds = 1\n")
	repo.CommitFile("one.txt", "one", "one")
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	repo.CommitFile("two.txt", "two", "two")

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		_, _, err := executePostCommitCmd("--repo", repo.Dir)
		require.NoError(t, err)
		assert.Equal(time.Second, time.Since(start), "hook must stop at its timeout")
	})
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

// TestPostCommitRemoteSharesOneDeadline checks that the enqueue, the pack
// upload, and the retry share one hook timeout. Each step alone fits in the
// timeout, so the retry would run if each request had its own.
func TestPostCommitRemoteSharesOneDeadline(t *testing.T) {
	herrs := newHandlerErrors(t)
	const step = 600 * time.Millisecond
	wait := func(r *http.Request) {
		select {
		case <-time.After(step):
		case <-r.Context().Done():
		}
	}
	var enqueues, uploads atomic.Int32
	repo := newTestGitRepo(t)
	head := repo.CommitFile("file.txt", "content", "initial")
	writeRoborevID(t, repo)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		if enqueues.Add(1) > 1 {
			respondJSON(w, http.StatusCreated, map[string]any{"id": 1})
			return
		}
		wait(r)
		respondJSON(w, http.StatusConflict, daemon.MissingCommitsResponse{
			Error: "missing", Code: daemon.MissingCommitsCode, Missing: []string{head},
		})
	})
	mux.HandleFunc(daemon.RemotePackPath, func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		_, err := io.Copy(io.Discard, r.Body)
		herrs.add(err)
		wait(r)
		respondJSON(w, http.StatusOK, map[string]any{"pinned": []string{head}})
	})
	ep := withInProcessRemote(t, mux)

	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		start := time.Now()
		_, _, err := postCommitEnqueue(context.Background(), ep, true, time.Second, repo.Dir,
			daemon.EnqueueRequest{GitRef: "HEAD", Source: "post_commit"})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(time.Second, time.Since(start), "the deadline covers the whole sequence")
		assert.Equal(int32(1), uploads.Load(), "the upload must start")
		assert.Equal(int32(1), enqueues.Load(), "the retry must not run past the shared deadline")
	})
}
