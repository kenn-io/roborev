package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

const cliUseBody = `{"event":"app_opened","properties":{"surface":"cli"}}`

// clearCLIUseState forgets the observer, the once and any started post, as a new process would.
func clearCLIUseState() {
	daemon.SetClientResponseObserver(nil)
	cliUseOnce = sync.Once{}
	cliUseState.Store(nil)
}

// resetCLIUse gives a test a fresh CLI telemetry state with telemetry on and restores every seam afterwards.
func resetCLIUse(t *testing.T) {
	t.Helper()
	origEnabled, origTimeout, origPost := cliTelemetryEnabled, cliUseTimeout, cliUsePost
	origServer, origParsed, origVerbose, origFromSkill := serverAddr, parsedServerEndpoint, verbose, fromSkill
	clearCLIUseState()
	cliTelemetryEnabled = func() bool { return true }
	t.Cleanup(func() {
		waitCLIUse()
		clearCLIUseState()
		cliTelemetryEnabled, cliUseTimeout, cliUsePost = origEnabled, origTimeout, origPost
		serverAddr, parsedServerEndpoint, verbose, fromSkill = origServer, origParsed, origVerbose, origFromSkill
	})
}

type recordedTelemetry struct {
	method      string
	contentType string
	body        string
}

// cliUseRecorder records every request a mock daemon receives, in order, and the telemetry posts separately.
type cliUseRecorder struct {
	mu        sync.Mutex
	paths     []string
	telemetry []recordedTelemetry
}

func (r *cliUseRecorder) note(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, req.Method+" "+req.URL.Path)
}

func (r *cliUseRecorder) telemetryPosts() []recordedTelemetry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.telemetry)
}

// businessPaths lists the recorded requests other than telemetry posts.
func (r *cliUseRecorder) businessPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, p := range r.paths {
		if !strings.HasSuffix(p, " "+daemon.TelemetryEventsPath) {
			out = append(out, p)
		}
	}
	return out
}

func (r *cliUseRecorder) allPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.paths)
}

func (r *cliUseRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths, r.telemetry = nil, nil
}

type mockHook = func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool

// noting wraps a mock hook so the recorder sees the request before the hook or the default handler answers it.
func (r *cliUseRecorder) noting(next mockHook) mockHook {
	return func(w http.ResponseWriter, req *http.Request, state *mockRefineState) bool {
		r.note(req)
		return next != nil && next(w, req, state)
	}
}

// newCLIUseDaemon starts NewMockDaemon with every route recorded; telemetry posts reach onTelemetry, or get kit's queued answer when nil.
func newCLIUseDaemon(t *testing.T, hooks MockRefineHooks, onTelemetry http.HandlerFunc) (*MockDaemon, *cliUseRecorder) {
	t.Helper()
	rec := &cliUseRecorder{}
	unhandled := hooks.OnUnhandled
	hooks.OnGetJobs = rec.noting(hooks.OnGetJobs)
	hooks.OnEnqueue = rec.noting(hooks.OnEnqueue)
	hooks.OnReview = rec.noting(hooks.OnReview)
	hooks.OnComments = rec.noting(hooks.OnComments)
	hooks.OnPing = rec.noting(hooks.OnPing)
	hooks.OnStatus = rec.noting(hooks.OnStatus)
	hooks.OnComment = rec.noting(hooks.OnComment)
	hooks.OnReviewClose = rec.noting(hooks.OnReviewClose)
	hooks.OnUnhandled = func(w http.ResponseWriter, req *http.Request, state *mockRefineState) bool {
		rec.note(req)
		if req.URL.Path == daemon.TelemetryEventsPath {
			body, _ := io.ReadAll(req.Body)
			rec.mu.Lock()
			rec.telemetry = append(rec.telemetry, recordedTelemetry{
				method: req.Method, contentType: req.Header.Get("Content-Type"), body: string(body),
			})
			rec.mu.Unlock()
			if onTelemetry != nil {
				onTelemetry(w, req)
				return true
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, "{\"status\":\"queued\"}\n")
			return true
		}
		return unhandled != nil && unhandled(w, req, state)
	}
	return NewMockDaemon(t, hooks), rec
}

type cliRun struct {
	stdout string
	stderr string
	err    error
}

// captureStdoutStderr swaps both process streams, since captureStdout swaps stdout only.
func captureStdoutStderr(t *testing.T, fn func()) (string, string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	errR, errW, err := os.Pipe()
	require.NoError(t, err)
	read := func(r *os.File) <-chan string {
		ch := make(chan string, 1)
		go func() {
			var buf bytes.Buffer
			_, _ = io.Copy(&buf, r)
			ch <- buf.String()
		}()
		return ch
	}
	outCh, errCh := read(outR), read(errR)
	os.Stdout, os.Stderr = outW, errW
	defer func() {
		os.Stdout, os.Stderr = origOut, origErr
		_ = outR.Close()
		_ = errR.Close()
	}()
	fn()
	os.Stdout, os.Stderr = origOut, origErr
	_ = outW.Close()
	_ = errW.Close()
	return <-outCh, <-errCh
}

// runCLI runs the real root command the way main() does, waiting for the telemetry post after Execute.
func runCLI(t *testing.T, args ...string) cliRun {
	t.Helper()
	root := newRootCmd()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	var run cliRun
	stdout, stderr := captureStdoutStderr(t, func() {
		run.err = root.Execute()
		waitCLIUse()
	})
	run.stdout = stdout + outBuf.String()
	run.stderr = stderr + errBuf.String()
	return run
}

// runCLIFresh runs one command as its own process would: fresh telemetry state and telemetry on or off.
func runCLIFresh(t *testing.T, telemetryOn bool, args ...string) cliRun {
	t.Helper()
	clearCLIUseState()
	cliTelemetryEnabled = func() bool { return telemetryOn }
	return runCLI(t, args...)
}

func stubStatusWebRuntime(t *testing.T) {
	t.Helper()
	withStatusWebRuntime(t, func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{}, nil
	})
}

func TestCLIUseLeavesOutputAndExitUnchanged(t *testing.T) {
	assert := assert.New(t)
	resetCLIUse(t)
	stubStatusWebRuntime(t)
	// The post crosses the mock daemon's loopback TCP socket, so the deadline is a short real timer.
	cliUseTimeout = 50 * time.Millisecond

	release := make(chan struct{})
	var handlerReturned atomic.Bool
	mode := atomic.Value{}
	mode.Store("hang")
	md, rec := newCLIUseDaemon(t, MockRefineHooks{}, func(w http.ResponseWriter, _ *http.Request) {
		if mode.Load() == "404" {
			http.NotFound(w, nil)
			return
		}
		<-release
		handlerReturned.Store(true)
	})
	// Registered after the mock, so it runs first and lets the hanging handler return before the server closes.
	t.Cleanup(func() { close(release) })

	off := runCLIFresh(t, false, "status", "--server", md.Server.URL)
	require.NoError(t, off.err)
	require.Contains(t, off.stdout, "Daemon: running")
	assert.Empty(rec.telemetryPosts())

	hang := runCLIFresh(t, true, "status", "--server", md.Server.URL)
	require.NoError(t, hang.err)
	assert.Equal(off.stdout, hang.stdout)
	assert.Equal(off.stderr, hang.stderr)
	assert.Len(rec.telemetryPosts(), 1)
	assert.False(handlerReturned.Load(), "waitCLIUse must return while the telemetry route still hangs")

	mode.Store("404")
	notFound := runCLIFresh(t, true, "status", "--server", md.Server.URL)
	require.NoError(t, notFound.err)
	assert.Equal(off.stdout, notFound.stdout)
	assert.Equal(off.stderr, notFound.stderr)
	assert.Len(rec.telemetryPosts(), 2)
}

type startupSeamCounts struct {
	getAny, probe, cleanup, restart, start atomic.Int32
}

func (c *startupSeamCounts) snapshot() [5]int32 {
	return [5]int32{c.getAny.Load(), c.probe.Load(), c.cleanup.Load(), c.restart.Load(), c.start.Load()}
}

func (c *startupSeamCounts) reset() {
	c.getAny.Store(0)
	c.probe.Store(0)
	c.cleanup.Store(0)
	c.restart.Store(0)
	c.start.Store(0)
}

// stubStartupSeams replaces every daemon discovery and startup seam with a counting stub that finds and starts nothing.
func stubStartupSeams(t *testing.T) *startupSeamCounts {
	t.Helper()
	counts := &startupSeamCounts{}
	origGetAny, origProbe, origCleanup := getAnyRunningDaemon, probeDaemonForEnsure, cleanupZombieDaemons
	origRestart, origStart := restartDaemonForEnsure, startDaemonForEnsure
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		counts.getAny.Add(1)
		return nil, os.ErrNotExist
	}
	probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		counts.probe.Add(1)
		return nil, errors.New("connection refused")
	}
	cleanupZombieDaemons = func(daemon.DaemonEndpoint) int {
		counts.cleanup.Add(1)
		return 0
	}
	restartDaemonForEnsure = func() error {
		counts.restart.Add(1)
		return errors.New("restart failed")
	}
	startDaemonForEnsure = func() error {
		counts.start.Add(1)
		return errors.New("start failed")
	}
	t.Cleanup(func() {
		getAnyRunningDaemon, probeDaemonForEnsure, cleanupZombieDaemons = origGetAny, origProbe, origCleanup
		restartDaemonForEnsure, startDaemonForEnsure = origRestart, origStart
	})
	return counts
}

func closedLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func TestCLIUseWithoutDaemonStartsNothing(t *testing.T) {
	t.Run("observer posts only to the answering endpoint", func(t *testing.T) {
		assert := assert.New(t)
		resetCLIUse(t)
		counts := stubStartupSeams(t)

		var mu sync.Mutex
		var seen []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.Method+" "+r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		}))
		t.Cleanup(srv.Close)
		ep, err := daemon.ParseEndpoint(srv.URL)
		require.NoError(t, err)

		observeCLIUse(ep, httptest.NewRequest(http.MethodGet, "/api/jobs", nil), &http.Response{StatusCode: http.StatusOK})
		waitCLIUse()
		assert.Equal([5]int32{}, counts.snapshot())
		mu.Lock()
		assert.Equal([]string{"POST " + daemon.TelemetryEventsPath}, seen)
		mu.Unlock()

		clearCLIUseState()
		closed, err := daemon.ParseEndpoint("http://" + closedLoopbackAddr(t))
		require.NoError(t, err)
		observeCLIUse(closed, httptest.NewRequest(http.MethodGet, "/api/jobs", nil), &http.Response{StatusCode: http.StatusOK})
		waitCLIUse()
		assert.Equal([5]int32{}, counts.snapshot())
	})

	t.Run("daemon down end to end", func(t *testing.T) {
		assert := assert.New(t)
		resetCLIUse(t)
		t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
		counts := stubStartupSeams(t)
		server := "http://" + closedLoopbackAddr(t)

		off := runCLIFresh(t, false, "list", "--server", server)
		offCounts := counts.snapshot()
		counts.reset()
		on := runCLIFresh(t, true, "list", "--server", server)

		require.Error(t, off.err)
		require.Error(t, on.err)
		assert.Equal(off.err.Error(), on.err.Error())
		assert.Equal(off.stdout, on.stdout)
		assert.Equal(off.stderr, on.stderr)
		assert.Equal(offCounts, counts.snapshot())
		assert.Nil(cliUseState.Load(), "no post may start without an answering daemon")
	})
}

func TestCLIUseReportsAppOpenedAfterDaemonCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		business string
	}{
		{name: "status", args: []string{"status"}, business: "GET /api/status"},
		{name: "list", args: []string{"list"}, business: "GET /api/jobs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			resetCLIUse(t)
			stubStatusWebRuntime(t)
			md, rec := newCLIUseDaemon(t, MockRefineHooks{}, nil)

			run := runCLI(t, append(tc.args, "--server", md.Server.URL)...)
			require.NoError(t, run.err)

			posts := rec.telemetryPosts()
			require.Len(t, posts, 1)
			assert.Equal(http.MethodPost, posts[0].method)
			assert.Equal("application/json", posts[0].contentType)
			assert.JSONEq(cliUseBody, posts[0].body)
			paths := rec.allPaths()
			businessAt := slices.Index(paths, tc.business)
			telemetryAt := slices.Index(paths, "POST "+daemon.TelemetryEventsPath)
			require.GreaterOrEqual(t, businessAt, 0)
			assert.Greater(telemetryAt, businessAt)
		})
	}
}

func TestCLIUseCommandSet(t *testing.T) {
	assert := assert.New(t)
	want := []string{
		"roborev review", "roborev wait", "roborev status", "roborev doctor", "roborev show", "roborev list", "roborev search",
		"roborev comment", "roborev respond", "roborev close", "roborev cancel", "roborev fix", "roborev refine",
		"roborev run", "roborev prompt", "roborev analyze", "roborev compact", "roborev insights", "roborev summary",
		"roborev cost", "roborev stream", "roborev snooze", "roborev pause", "roborev unpause",
		"roborev export reviews", "roborev export ci-metrics", "roborev export ci-costs", "roborev sync now",
		"roborev init", "roborev log", "roborev log clean", "roborev repo list", "roborev repo show",
		"roborev repo rename", "roborev repo move", "roborev repo delete", "roborev repo merge",
		"roborev legacy-reviews convert", "roborev legacy-reviews export", "roborev legacy-reviews import",
		"roborev backfill-verdicts", "roborev backfill-tokens", "roborev sync status",
	}
	got := make([]string, 0, len(cliUseCommands))
	for path, counted := range cliUseCommands {
		require.True(t, counted)
		got = append(got, path)
	}
	assert.ElementsMatch(want, got)

	root := newRootCmd()
	for path := range cliUseCommands {
		found, _, err := root.Find(strings.Fields(path)[1:])
		require.NoError(t, err, path)
		assert.Equal(path, found.CommandPath())
	}

	for _, excluded := range []string{
		"roborev post-commit", "roborev enqueue", "roborev remap", "roborev quickstart", "roborev tui",
		"roborev ui", "roborev daemon run", "roborev daemon start", "roborev daemon stop",
		"roborev daemon restart", "roborev update",
	} {
		assert.False(cliUseCommands[excluded], excluded)
	}
	for path := range cliUseCommands {
		assert.False(strings.HasPrefix(path, "roborev agent-hook"), path)
		assert.False(strings.HasPrefix(path, "roborev mcp"), path)
	}

	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, child := range cmd.Commands() {
			flag := child.InheritedFlags().Lookup("from-skill")
			require.NotNil(t, flag, child.CommandPath())
			assert.True(flag.Hidden, child.CommandPath())
			walk(child)
		}
	}
	walk(root)

	t.Run("post-commit sends nothing", func(t *testing.T) {
		resetCLIUse(t)
		md, rec := newCLIUseDaemon(t, MockRefineHooks{
			OnEnqueue: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
				respondJSON(w, http.StatusCreated, storage.ReviewJob{ID: 1, GitRef: "abc123", Agent: "test"})
				return true
			},
		}, nil)
		repo := newTestGitRepo(t)
		repo.CommitFile("file.txt", "content", "initial commit")

		run := runCLI(t, "post-commit", "--repo", repo.Dir, "--server", md.Server.URL)
		require.NoError(t, run.err)
		require.Contains(t, rec.businessPaths(), "POST /api/enqueue")
		require.Empty(t, rec.telemetryPosts())
	})
}

func TestCLIUseSkipsWhenTelemetryOff(t *testing.T) {
	resetCLIUse(t)
	stubStatusWebRuntime(t)
	cliTelemetryEnabled = func() bool { return false }
	md, rec := newCLIUseDaemon(t, MockRefineHooks{}, nil)

	run := runCLI(t, "status", "--server", md.Server.URL)
	require.NoError(t, run.err)
	require.Contains(t, rec.businessPaths(), "GET /api/status")
	assert.Empty(t, rec.telemetryPosts())
}

func TestCLIUseReportsOnDaemonSuccessNotExit(t *testing.T) {
	t.Run("failing review verdict still reports the enqueue", func(t *testing.T) {
		resetCLIUse(t)
		setupFastPolling(t)
		mux := http.NewServeMux()
		md := daemonFromHandler(t, mux)
		var posts atomic.Int32
		mux.HandleFunc(daemon.TelemetryEventsPath, func(w http.ResponseWriter, _ *http.Request) {
			posts.Add(1)
			w.WriteHeader(http.StatusAccepted)
		})
		repo := newTestGitRepo(t)
		repo.CommitFile("file.txt", "content", "initial commit")
		mockWaitableReview(t, mux, "Found 1 issue", 0)

		run := runCLI(t, "review", "--repo", repo.Dir, "--wait", "--quiet", "--server", md.Server.URL)
		var exitErr *exitError
		require.ErrorAs(t, run.err, &exitErr)
		assert.Equal(t, 1, exitErr.code)
		assert.Equal(t, int32(1), posts.Load())
	})

	t.Run("local review sends nothing", func(t *testing.T) {
		resetCLIUse(t)
		md, rec := newCLIUseDaemon(t, MockRefineHooks{}, nil)
		repo := newTestGitRepo(t)
		repo.CommitFile("file.txt", "content", "initial commit")

		run := runCLI(t, "review", "--local", "--agent", "test", "--repo", repo.Dir, "--quiet", "--server", md.Server.URL)
		require.NoError(t, run.err)
		assert.Empty(t, rec.telemetryPosts())
		assert.Nil(t, cliUseState.Load())
	})

	t.Run("status with a failing status route sends nothing", func(t *testing.T) {
		resetCLIUse(t)
		stubStatusWebRuntime(t)
		md, rec := newCLIUseDaemon(t, MockRefineHooks{
			OnStatus: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
				http.Error(w, "boom", http.StatusInternalServerError)
				return true
			},
		}, nil)

		run := runCLI(t, "status", "--server", md.Server.URL)
		require.NoError(t, run.err)
		assert.Contains(t, run.stdout, "Status: unavailable")
		assert.Empty(t, rec.telemetryPosts())
	})

	t.Run("list with a failing jobs route sends nothing", func(t *testing.T) {
		assert := assert.New(t)
		resetCLIUse(t)
		md, rec := newCLIUseDaemon(t, MockRefineHooks{
			OnGetJobs: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
				http.Error(w, "boom", http.StatusInternalServerError)
				return true
			},
		}, nil)

		off := runCLIFresh(t, false, "list", "--server", md.Server.URL)
		on := runCLIFresh(t, true, "list", "--server", md.Server.URL)
		require.Error(t, on.err)
		require.Error(t, off.err)
		assert.Equal(off.err.Error(), on.err.Error())
		assert.Equal(off.stdout, on.stdout)
		assert.Empty(rec.telemetryPosts())
	})
}

func TestCLIUseOncePerProcess(t *testing.T) {
	resetCLIUse(t)
	stubStatusWebRuntime(t)
	md, rec := newCLIUseDaemon(t, MockRefineHooks{}, nil)

	run := runCLI(t, "status", "--server", md.Server.URL)
	require.NoError(t, run.err)
	paths := rec.businessPaths()
	require.Contains(t, paths, "GET /api/status")
	require.Contains(t, paths, "GET /api/jobs")
	assert.Len(t, rec.telemetryPosts(), 1)
}

func TestCLIUseRecoveryShutdownDoesNotCount(t *testing.T) {
	t.Run("eligibility", func(t *testing.T) {
		for _, tc := range []struct {
			method string
			path   string
			status int
			want   bool
		}{
			{http.MethodGet, "/api/ping", 200, false},
			{http.MethodPost, "/api/shutdown", 200, false},
			{http.MethodPost, "/api/update/prepare", 200, false},
			{http.MethodPost, "/api/update/renew", 200, false},
			{http.MethodPost, "/api/update/release", 200, false},
			{http.MethodPost, daemon.TelemetryEventsPath, 202, false},
			{http.MethodGet, "/api/status", 200, true},
			{http.MethodGet, "/api/jobs", 200, true},
			{http.MethodPost, "/api/enqueue", 201, true},
			{http.MethodPost, "/api/comment", 201, true},
			{http.MethodPost, "/api/review/close", 200, true},
			{http.MethodGet, "/api/stream/events", 200, true},
			{http.MethodGet, "/api/status", 304, false},
			{http.MethodGet, "/api/status", 404, false},
			{http.MethodGet, "/api/jobs", 500, false},
		} {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			got := cliUseEligible(req, &http.Response{StatusCode: tc.status})
			assert.Equal(t, tc.want, got, "%s %s %d", tc.method, tc.path, tc.status)
		}
	})

	t.Run("restart against the old daemon leaves the report to the new one", func(t *testing.T) {
		assert := assert.New(t)
		resetCLIUse(t)
		stubStatusWebRuntime(t)
		mdA, recA := newCLIUseDaemon(t, MockRefineHooks{
			OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
				if r.URL.Path == "/api/shutdown" {
					w.WriteHeader(http.StatusOK)
					return true
				}
				return false
			},
		}, nil)
		mdB, recB := newCLIUseDaemon(t, MockRefineHooks{}, nil)
		epA, err := daemon.ParseEndpoint(mdA.Server.URL)
		require.NoError(t, err)

		origEnsure := statusEnsureDaemon
		t.Cleanup(func() { statusEnsureDaemon = origEnsure })
		statusEnsureDaemon = func() error {
			if _, err := daemon.ProbeDaemonPing(epA, 2*time.Second); err != nil {
				return err
			}
			resp, err := epA.APIClient(0).ShutdownRaw(context.Background())
			if err != nil {
				return err
			}
			_ = resp.Body.Close()
			return nil
		}

		run := runCLI(t, "status", "--server", mdB.Server.URL)
		require.NoError(t, run.err)
		assert.Equal([]string{"GET /api/ping", "POST /api/shutdown"}, recA.businessPaths())
		assert.Empty(recA.telemetryPosts())
		assert.Len(recB.telemetryPosts(), 1)
	})
}

func TestCLIUseSkipsSkillMarkedCommands(t *testing.T) {
	resetCLIUse(t)
	md, rec := newCLIUseDaemon(t, MockRefineHooks{}, nil)
	md.State.mu.Lock()
	md.State.jobs[1] = &storage.ReviewJob{ID: 1, GitRef: "abc123", Agent: "test", Status: storage.JobStatusDone}
	md.State.reviews["abc123"] = &storage.Review{ID: 1, JobID: 1, Agent: "test", Output: "No issues found."}
	md.State.mu.Unlock()

	for _, tc := range []struct {
		name   string
		marked []string
		plain  []string
	}{
		{
			name:   "show",
			marked: []string{"show", "--from-skill", "--job", "1", "--json"},
			plain:  []string{"show", "--job", "1", "--json"},
		},
		{
			name:   "comment",
			marked: []string{"comment", "--from-skill", "--commenter", "roborev-fix", "--job", "1", "-m", "checked"},
			plain:  []string{"comment", "--commenter", "roborev-fix", "--job", "1", "-m", "checked"},
		},
		{
			name:   "close",
			marked: []string{"close", "--from-skill", "1"},
			plain:  []string{"close", "1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			rec.reset()
			marked := runCLIFresh(t, true, append(tc.marked, "--server", md.Server.URL)...)
			markedBusiness := rec.businessPaths()
			assert.Empty(rec.telemetryPosts())

			rec.reset()
			plain := runCLIFresh(t, true, append(tc.plain, "--server", md.Server.URL)...)
			require.NoError(t, marked.err)
			require.NoError(t, plain.err)
			assert.Len(rec.telemetryPosts(), 1)
			assert.Equal(plain.stdout, marked.stdout)
			assert.Equal(plain.stderr, marked.stderr)
			assert.Equal(rec.businessPaths(), markedBusiness)
			assert.NotEmpty(markedBusiness)
		})
	}
}

func TestCLIUseWaitSharesPostDeadline(t *testing.T) {
	t.Run("hung post ends at the deadline", func(t *testing.T) {
		resetCLIUse(t)
		synctest.Test(t, func(t *testing.T) {
			var deadline time.Time
			var hasDeadline bool
			cliUsePost = func(ctx context.Context, _ *http.Client, _, _ string) {
				deadline, hasDeadline = ctx.Deadline()
				<-ctx.Done()
			}
			start := time.Now()
			ep := daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}
			observeCLIUse(ep, httptest.NewRequest(http.MethodGet, "/api/status", nil), &http.Response{StatusCode: http.StatusOK})
			waitCLIUse()
			synctest.Wait()

			assert.Equal(t, time.Second, time.Since(start))
			require.True(t, hasDeadline)
			assert.Equal(t, start.Add(time.Second), deadline)
			// The context belongs to this bubble; cleanup's waitCLIUse runs outside it.
			cliUseState.Store(nil)
		})
	})

	t.Run("finished post ends the wait at once", func(t *testing.T) {
		resetCLIUse(t)
		synctest.Test(t, func(t *testing.T) {
			cliUsePost = func(context.Context, *http.Client, string, string) {}
			start := time.Now()
			ep := daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}
			observeCLIUse(ep, httptest.NewRequest(http.MethodGet, "/api/status", nil), &http.Response{StatusCode: http.StatusOK})
			waitCLIUse()

			assert.Equal(t, time.Duration(0), time.Since(start))
			cliUseState.Store(nil)
		})
	})
}
