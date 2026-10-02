package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
)

func TestAuthDaemonRunRejectsBrokenConfig(t *testing.T) {
	for _, tc := range []struct{ name, contents string }{
		{"syntax", `auth_key = secret-never-print`},
		{"invalid key", `auth_key = "bad key"`},
		{"short key", `auth_key = "a"`},
		{"missing", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ROBOREV_DATA_DIR", dir)
			path := filepath.Join(dir, "daemon.toml")
			if tc.name != "missing" {
				require.NoError(t, os.WriteFile(path, []byte(tc.contents), 0o600))
			}
			cmd := daemonRunCmd()
			cmd.SetArgs([]string{"--config", path, "--db", dir}) // Directory prevents starting if config is ignored.
			err := cmd.Execute()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "config")
			assert.NotContains(t, err.Error(), "secret-never-print")
		})
	}
}

func TestAuthCLIJobLookup(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`), 0o600))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"id":23,"status":"done"}]}`))
	}))
	defer server.Close()
	oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
	serverAddr = strings.TrimPrefix(server.URL, "http://")
	parsedServerEndpoint = nil
	t.Cleanup(func() { serverAddr = oldAddr; parsedServerEndpoint = oldEndpoint })
	job, err := findJobForCommit(t.TempDir(), "abc123")
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.EqualValues(t, 23, job.ID)
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"`), 0o600))
	_, err = findJobForCommit(t.TempDir(), "abc123")
	require.ErrorContains(t, err, "401 Unauthorized")
}

func TestAuthExplicitURLHelpers(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`), 0o600))
	var recovered atomic.Bool
	patchFixDaemonRetryForTest(t, func() error {
		recovered.Store(true)
		return errors.New("unexpected daemon recovery")
	})
	fixDaemonRecoveryWait = 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/review":
			_, _ = w.Write([]byte(`{"job_id":23,"output":"review"}`))
		case "/api/review/close":
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	review, err := fetchReview(ctx, server.URL, 23)
	require.NoError(t, err)
	require.NotNil(t, review)
	assert.EqualValues(t, 23, review.JobID)
	require.NoError(t, markJobClosed(ctx, server.URL, 23))
	assert.False(t, recovered.Load())
}

func TestAuthConfigDenialDoesNotRecoverDaemon(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = broken-secret`), 0o600))
	var recovered atomic.Bool
	patchFixDaemonRetryForTest(t, func() error {
		recovered.Store(true)
		return errors.New("unexpected daemon recovery")
	})
	fixDaemonRecoveryWait = 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	_, err := fetchReview(context.Background(), server.URL, 23)
	require.Error(t, err)
	require.ErrorIs(t, err, daemon.ErrClientConfig)
	assert.NotContains(t, err.Error(), "access denied")
	assert.False(t, recovered.Load())
	assert.NotContains(t, err.Error(), "broken-secret")
}

func TestAuthConfigDenialStopsEnqueueProbe(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "bad key"`), 0o600))
	patchFixDaemonRetryForTest(t, nil)
	enqueueIfNeededProbeAttempts = 3
	var sleepCalls atomic.Int32
	fixDaemonSleep = func(time.Duration) { sleepCalls.Add(1) }
	var probeCalls atomic.Int32
	var enqueueCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			probeCalls.Add(1)
		case "/api/enqueue":
			enqueueCalls.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	repo := createTestRepo(t, map[string]string{"f.txt": "x"})
	sha := repo.Run("rev-parse", "HEAD")
	err := enqueueIfNeeded(context.Background(), server.URL, repo.Dir, sha)
	require.ErrorIs(t, err, daemon.ErrClientConfig)
	assert.True(t, daemon.IsDaemonAccessError(err))
	assert.Zero(t, sleepCalls.Load())
	assert.Zero(t, probeCalls.Load())
	assert.Zero(t, enqueueCalls.Load())
}

func TestAuthCloseAndCancelMapUnauthorized(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	for _, tc := range []struct {
		name string
		call func(string) error
	}{
		{"close", func(serverAddr string) error { return markJobClosed(context.Background(), serverAddr, 23) }},
		{"cancel", func(serverAddr string) error { return cancelJob(serverAddr, 23) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"daemon authentication required"}`))
			}))
			defer server.Close()

			err := tc.call(server.URL)
			require.ErrorIs(t, err, daemon.ErrDaemonAccessDenied)
		})
	}
}

func TestAuthOriginDenialIsNotAConnectionFailure(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	endpoint, err := daemon.ParseEndpoint("127.0.0.1:7373")
	require.NoError(t, err)
	_, err = endpoint.HTTPClient(time.Second).Get("http://127.0.0.1:7374/api/ping")
	require.Error(t, err)
	assert.False(t, isConnectionError(err))
}

func TestAuthEnqueueStopsOnAccessErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		denyAt     int32
		badConfig  bool
		wantProbes int32
		wantPosts  int32
		wantWaits  int
	}{
		{name: "first probe", denyAt: 1, wantProbes: 1},
		{name: "final probe", denyAt: 2, wantProbes: 2, wantWaits: 1},
		{name: "enqueue", denyAt: 3, wantProbes: 2, wantPosts: 1, wantWaits: 1},
		{name: "config", badConfig: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			if tc.badConfig {
				require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = broken-secret`), 0o600))
			}
			var recoveries, probes, posts atomic.Int32
			patchFixDaemonRetryForTest(t, func() error {
				recoveries.Add(1)
				return errors.New("unexpected daemon recovery")
			})
			waits := 0
			fixDaemonSleep = func(time.Duration) { waits++ }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/jobs":
					if probes.Add(1) == tc.denyAt {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					_, _ = w.Write([]byte(`{"jobs":[]}`))
				case "/api/enqueue":
					posts.Add(1)
					w.WriteHeader(http.StatusUnauthorized)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			err := enqueueIfNeeded(t.Context(), server.URL, t.TempDir(), "abc123")
			wantErr := daemon.ErrDaemonAccessDenied
			if tc.badConfig {
				wantErr = daemon.ErrClientConfig
			}
			require.ErrorIs(t, err, wantErr)
			a := assert.New(t)
			a.Equal(tc.wantProbes, probes.Load())
			a.Equal(tc.wantPosts, posts.Load())
			a.Equal(tc.wantWaits, waits)
			a.Zero(recoveries.Load())
		})
	}
}

func TestAuthFixRecoveryStopsOnAccessErrors(t *testing.T) {
	for _, accessErr := range []error{daemon.ErrDaemonAccessDenied, daemon.ErrClientConfig} {
		t.Run(accessErr.Error(), func(t *testing.T) {
			calls := 0
			patchFixDaemonRetryForTest(t, func() error {
				calls++
				return accessErr
			})
			synctest.Test(t, func(t *testing.T) {
				_, err := recoverFixDaemonAddr(t.Context())
				require.ErrorIs(t, err, accessErr)
				assert.Equal(t, 1, calls)
			})
		})
	}
}

func TestAuthHookKeepsCapturedEndpoint(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	repo.CommitFile("file.txt", "content", "initial")
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`), 0o600))
	var received atomic.Bool
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		received.Store(true)
		assert.Equal(t, "Bearer 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":23}`))
	})
	other := httptest.NewServer(http.NotFoundHandler())
	defer other.Close()
	original := hookHTTPClient
	hookHTTPClient = func(endpoint daemon.DaemonEndpoint, timeout time.Duration) *http.Client {
		oldAddr, oldParsed := serverAddr, parsedServerEndpoint
		serverAddr, parsedServerEndpoint = strings.TrimPrefix(other.URL, "http://"), nil
		defer func() { serverAddr, parsedServerEndpoint = oldAddr, oldParsed }()
		return original(endpoint, timeout)
	}
	t.Cleanup(func() { hookHTTPClient = original })
	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)
	assert.True(t, received.Load())
}

func TestAuthFixStopsOnDeniedRequests(t *testing.T) {
	for _, mode := range []string{"batch", "direct"} {
		for _, tc := range []struct {
			name, path string
			legacy     bool
		}{
			{"job", "/api/jobs", false},
			{"review", "/api/review", false},
			{"comments", "/api/comments", false},
			{"legacy comments", "/api/comments", true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
				require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`), 0o600))
				repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == tc.path && (!tc.legacy || r.URL.Query().Get("commit_id") != "") {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/jobs":
						_, _ = w.Write([]byte(`{"jobs":[{"id":23,"status":"done","agent":"test","commit_id":7}]}`))
					case "/api/review":
						_, _ = w.Write([]byte(`{"job_id":23,"output":"## Issues\n- A high severity bug."}`))
					default:
						_, _ = w.Write([]byte(`{"responses":[]}`))
					}
				}))
				defer server.Close()
				patchServerAddr(t, server.URL)
				tester := agent.NewTestAgent()
				tracker := &fixSessionTracker{base: tester, out: io.Discard}
				cmd, _ := newTestCmd(t)
				var err error
				if mode == "batch" {
					err = processFixBatch(context.Background(), cmd, currentRepoRoots{worktreeRoot: repo.Dir, mainRepoRoot: repo.Dir}, []int64{23}, 0, fixOptions{agentName: "test", quiet: true}, tracker)
				} else {
					err = fixSingleJob(cmd, repo.Dir, 23, fixOptions{agentName: "test", quiet: true}, tracker)
				}
				require.ErrorIs(t, err, daemon.ErrDaemonAccessDenied)
				assert.Empty(t, tester.Calls())
			})
		}
	}
}

func TestAuthStartUsesAuthenticatedDiscovery(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`), 0o600))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, "Bearer 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", r.Header.Get("Authorization"))
		fmt.Fprintf(w, `{"ok":true,"service":"roborev","pid":%d}`, os.Getpid())
	}))
	defer server.Close()
	ep, err := daemon.ParseEndpoint(server.Listener.Addr().String())
	require.NoError(t, err)
	require.NoError(t, daemon.WriteRuntime(ep, nil, "test-version", nil))
	require.NoError(t, startDaemon())
	assert.EqualValues(t, 1, requests.Load())
}

func TestAuthStatusReportsConfigFailure(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = broken-secret`), 0o600))
	cmd := statusCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	require.ErrorIs(t, cmd.Execute(), daemon.ErrClientConfig)
}
