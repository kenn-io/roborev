package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/safefileio"
	"go.kenn.io/kit/secretref"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/version"
)

func TestGetDaemonEndpointAvoidsDefaultDaemonPortInTests(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	if !isGoTestBinaryPath(exe) {
		t.Skipf("expected go test binary path, got %q", exe)
	}

	origServerAddr := serverAddr
	origParsed := parsedServerEndpoint
	origGetAnyRunningDaemon := getAnyRunningDaemon
	serverAddr = ""
	parsedServerEndpoint = nil
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() {
		serverAddr = origServerAddr
		parsedServerEndpoint = origParsed
		getAnyRunningDaemon = origGetAnyRunningDaemon
	})

	got := getDaemonEndpoint()
	assert.Equal(t, "tcp", got.Network)
	assert.Equal(t, "127.0.0.1:1", got.Address)
}

func TestGetDaemonEndpointIgnoresCachedDefaultFromEmptyServerFlagInTests(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	if !isGoTestBinaryPath(exe) {
		t.Skipf("expected go test binary path, got %q", exe)
	}

	origServerAddr := serverAddr
	origParsed := parsedServerEndpoint
	origGetAnyRunningDaemon := getAnyRunningDaemon
	serverAddr = ""
	parsedServerEndpoint = nil
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() {
		serverAddr = origServerAddr
		parsedServerEndpoint = origParsed
		getAnyRunningDaemon = origGetAnyRunningDaemon
	})

	require.NoError(t, validateServerFlag())

	got := getDaemonEndpoint()
	assert.Equal(t, "tcp", got.Network)
	assert.Equal(t, "127.0.0.1:1", got.Address)
}

func TestEnsureDaemonPrefersLiveDaemonVersionOverRuntimeMetadata(t *testing.T) {
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ping":
			_ = json.NewEncoder(w).Encode(daemon.PingInfo{
				OK:      true,
				Service: "roborev",
				Version: "v-other-daemon",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	origGetAnyRunningDaemon := getAnyRunningDaemon
	origRestartDaemon := restartDaemonForEnsure
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{
			PID:     1234,
			Address: strings.TrimPrefix(server.URL, "http://"),
			Version: version.Version,
		}, nil
	}
	restartCalls := 0
	restartDaemonForEnsure = func() error {
		restartCalls++
		return nil
	}
	t.Cleanup(func() {
		getAnyRunningDaemon = origGetAnyRunningDaemon
		restartDaemonForEnsure = origRestartDaemon
	})

	if err := ensureDaemon(); err != nil {
		require.NoError(t, err, "ensureDaemon returned error: %v")
	}
	assert.Equal(t, 1, restartCalls)
}

func TestEnsureDaemonRestartsWhenLiveProbeFailsDespiteRuntimeVersion(t *testing.T) {
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")

	origGetAnyRunningDaemon := getAnyRunningDaemon
	origRestartDaemon := restartDaemonForEnsure
	origRetryDelay := ensureProbeRetryDelay
	ensureProbeRetryDelay = time.Millisecond
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{
			PID:     1234,
			Address: "127.0.0.1:1",
			Version: version.Version,
		}, nil
	}
	restartCalls := 0
	restartDaemonForEnsure = func() error {
		restartCalls++
		return nil
	}
	t.Cleanup(func() {
		getAnyRunningDaemon = origGetAnyRunningDaemon
		restartDaemonForEnsure = origRestartDaemon
		ensureProbeRetryDelay = origRetryDelay
	})

	if err := ensureDaemon(); err != nil {
		require.NoError(t, err, "ensureDaemon returned error: %v")
	}
	assert.Equal(t, 1, restartCalls)
}

func TestEnsureDaemonCleansZombiesBeforeColdStart(t *testing.T) {
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")

	origServerAddr := serverAddr
	origParsed := parsedServerEndpoint
	origGetAnyRunningDaemon := getAnyRunningDaemon
	origCleanupZombieDaemons := cleanupZombieDaemons
	origStartDaemon := startDaemonForEnsure
	origRetryDelay := ensureProbeRetryDelay
	serverAddr = ""
	parsedServerEndpoint = nil
	ensureProbeRetryDelay = time.Millisecond
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, os.ErrNotExist
	}

	var calls []string
	cleanupZombieDaemons = func(target daemon.DaemonEndpoint) int {
		calls = append(calls, "cleanup:"+target.Address)
		return 1
	}
	startDaemonForEnsure = func() error {
		calls = append(calls, "start")
		return nil
	}
	t.Cleanup(func() {
		serverAddr = origServerAddr
		parsedServerEndpoint = origParsed
		getAnyRunningDaemon = origGetAnyRunningDaemon
		cleanupZombieDaemons = origCleanupZombieDaemons
		startDaemonForEnsure = origStartDaemon
		ensureProbeRetryDelay = origRetryDelay
	})

	require.NoError(t, ensureDaemon())
	assert.Equal(t, []string{"cleanup:127.0.0.1:1", "start"}, calls)
}

func TestEnsureDaemonDoesNotRecoverFromAccessDeniedDiscovery(t *testing.T) {
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")

	origGet := getAnyRunningDaemon
	origProbe := probeDaemonForEnsure
	origCleanup := cleanupZombieDaemons
	origRestart := restartDaemonForEnsure
	origStart := startDaemonForEnsure
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, daemon.ErrDaemonAccessDenied
	}
	probeCalls, cleanupCalls, restartCalls, startCalls := 0, 0, 0, 0
	probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		probeCalls++
		return nil, nil
	}
	cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { cleanupCalls++; return 0 }
	restartDaemonForEnsure = func() error { restartCalls++; return nil }
	startDaemonForEnsure = func() error { startCalls++; return nil }
	t.Cleanup(func() {
		getAnyRunningDaemon = origGet
		probeDaemonForEnsure = origProbe
		cleanupZombieDaemons = origCleanup
		restartDaemonForEnsure = origRestart
		startDaemonForEnsure = origStart
	})

	err := ensureDaemon()
	require.ErrorIs(t, err, daemon.ErrDaemonAccessDenied)
	assert.Zero(t, probeCalls)
	assert.Zero(t, cleanupCalls)
	assert.Zero(t, restartCalls)
	assert.Zero(t, startCalls)
}

func TestEnsureDaemonDoesNotRestartAfterAccessDeniedVersionProbe(t *testing.T) {
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")

	origGet := getAnyRunningDaemon
	origProbe := probeDaemonForEnsure
	origRestart := restartDaemonForEnsure
	origStart := startDaemonForEnsure
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{Network: "tcp", Address: "127.0.0.1:7373"}, nil
	}
	probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EPERM}
	}
	restartCalls, startCalls := 0, 0
	restartDaemonForEnsure = func() error { restartCalls++; return nil }
	startDaemonForEnsure = func() error { startCalls++; return nil }
	t.Cleanup(func() {
		getAnyRunningDaemon = origGet
		probeDaemonForEnsure = origProbe
		restartDaemonForEnsure = origRestart
		startDaemonForEnsure = origStart
	})

	err := ensureDaemon()
	require.True(t, daemon.IsDaemonAccessError(err))
	assert.Zero(t, restartCalls)
	assert.Zero(t, startCalls)
}

func TestEnsureDaemonDiscoveryErrors(t *testing.T) {
	for _, tc := range []struct {
		name          string
		discoveryErr  error
		wantAccessErr bool
		wantStarts    int
	}{
		{
			name:          "access denied blocks startup",
			discoveryErr:  &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EACCES},
			wantAccessErr: true,
		},
		{
			// Something answered, but its certificate did not verify.
			name:          "certificate failure blocks startup",
			discoveryErr:  &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
			wantAccessErr: true,
		},
		{
			name:         "missing runtime starts a daemon",
			discoveryErr: os.ErrNotExist,
			wantStarts:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")

			origServerAddr := serverAddr
			origParsed := parsedServerEndpoint
			origGet := getAnyRunningDaemon
			origProbe := probeDaemonForEnsure
			origCleanup := cleanupZombieDaemons
			origStart := startDaemonForEnsure
			serverAddr = ""
			parsedServerEndpoint = nil
			getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, tc.discoveryErr }
			probeCalls := 0
			probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
				probeCalls++
				return nil, daemon.ErrGuessedEndpointAuth
			}
			startCalls := 0
			cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { return 0 }
			startDaemonForEnsure = func() error { startCalls++; return nil }
			t.Cleanup(func() {
				serverAddr = origServerAddr
				parsedServerEndpoint = origParsed
				getAnyRunningDaemon = origGet
				probeDaemonForEnsure = origProbe
				cleanupZombieDaemons = origCleanup
				startDaemonForEnsure = origStart
			})

			err := ensureDaemon()
			assert.Equal(t, tc.wantAccessErr, daemon.IsDaemonAccessError(err))
			assert.Equal(t, tc.wantStarts, startCalls)
			assert.Zero(t, probeCalls)
		})
	}
}

func TestStartDaemonUsesAlternateAwareDiscoveryInsideStartLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are not supported on Windows")
	}
	testenv.SetDataDir(t)

	primary := daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:1"}
	alternate := daemon.DaemonEndpoint{
		Network: "unix",
		Address: filepath.Join(t.TempDir(), "daemon.sock"),
	}
	require.NoError(t, daemon.WriteRuntime(primary, &alternate, version.Version, nil))

	origGet := getAnyRunningDaemonForStart
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{
			PID:     os.Getpid(),
			Network: alternate.Network,
			Address: alternate.Address,
			Version: version.Version,
		}, nil
	}
	t.Cleanup(func() { getAnyRunningDaemonForStart = origGet })

	require.NoError(t, startDaemon())
}

func TestRestartDaemonDoesNotStartAfterGracefulStopFailure(t *testing.T) {
	origStop := stopDaemonForRestart
	origStart := startDaemonAfterRestart
	t.Cleanup(func() {
		stopDaemonForRestart = origStop
		startDaemonAfterRestart = origStart
	})

	stopErr := errors.New("graceful shutdown unavailable")
	stopDaemonForRestart = func() error { return stopErr }
	startCalls := 0
	startDaemonAfterRestart = func() error {
		startCalls++
		return nil
	}

	err := restartDaemon()

	require.ErrorIs(t, err, stopErr)
	assert.Zero(t, startCalls)
}

func TestStartDaemonDoesNotSpawnAfterAccessDeniedDiscoveryInsideStartLock(t *testing.T) {
	testenv.SetDataDir(t)

	origGet := getAnyRunningDaemonForStart
	origStart := startDaemonDetached
	spawnCalls := 0
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
		return nil, daemon.ErrDaemonAccessDenied
	}
	startDaemonDetached = func(context.Context, detachedDaemonOptions) error {
		spawnCalls++
		return nil
	}
	t.Cleanup(func() {
		getAnyRunningDaemonForStart = origGet
		startDaemonDetached = origStart
	})

	err := startDaemon()
	require.ErrorIs(t, err, daemon.ErrDaemonAccessDenied)
	assert.Zero(t, spawnCalls)
}

func TestStartDaemonUsesAlternateAwareDiscoveryWhileWaiting(t *testing.T) {
	testenv.SetDataDir(t)

	origGet := getAnyRunningDaemonForStart
	origStart := startDaemonDetached
	discoveryCalls := 0
	spawnCalls := 0
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
		discoveryCalls++
		if discoveryCalls <= 2 {
			return nil, os.ErrNotExist
		}
		return &daemon.RuntimeInfo{
			PID:     os.Getpid(),
			Network: "unix",
			Address: filepath.Join(t.TempDir(), "daemon.sock"),
			Version: version.Version,
		}, nil
	}
	startDaemonDetached = func(context.Context, detachedDaemonOptions) error {
		spawnCalls++
		return nil
	}
	t.Cleanup(func() {
		getAnyRunningDaemonForStart = origGet
		startDaemonDetached = origStart
	})

	require.NoError(t, startDaemon())
	assert.Equal(t, 1, spawnCalls)
}

func TestDiscoverDaemonForStartHonorsCanceledContext(t *testing.T) {
	testenv.SetDataDir(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	ready, err := discoverDaemonForStart(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, ready)
}

func TestStartDaemonReportsStartupProgress(t *testing.T) {
	for _, tc := range []struct {
		name        string
		readyAfter  time.Duration
		startupLog  string
		wantMessage string
		wantTimeout bool
	}{
		{name: "fast startup", readyAfter: time.Second},
		{
			name: "database initialization", readyAfter: 30 * time.Second,
			startupLog:  "Opening database\nRestoring archived reviews\n",
			wantMessage: "Restoring archived reviews",
		},
		{
			name: "startup failure", readyAfter: 3 * time.Minute,
			startupLog:  "Opening database\nError: invalid daemon configuration\n",
			wantMessage: "Error: invalid daemon configuration", wantTimeout: true,
		},
		{name: "timeout without new logs", readyAfter: 3 * time.Minute, wantTimeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := testenv.SetDataDir(t)
			logPath := filepath.Join(dataDir, "logs", "daemon.stderr.log")
			require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
			require.NoError(t, os.WriteFile(logPath, []byte("Error: previous startup failure\n"), 0o600))
			origGet, origStart, origOut := getAnyRunningDaemonForStart, startDaemonDetached, lifecycleOut
			t.Cleanup(func() {
				getAnyRunningDaemonForStart, startDaemonDetached, lifecycleOut = origGet, origStart, origOut
			})

			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				var progress bytes.Buffer
				lifecycleOut = &progress
				var readyAt time.Time
				getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
					if readyAt.IsZero() || time.Now().Before(readyAt) {
						return nil, os.ErrNotExist
					}
					return &daemon.RuntimeInfo{PID: os.Getpid()}, nil
				}
				spawnCalls := 0
				startDaemonDetached = func(_ context.Context, opts detachedDaemonOptions) error {
					spawnCalls++
					// Database initialization may consume SQLite's 30-second busy
					// wait before the daemon can publish its runtime.
					readyAt = time.Now().Add(tc.readyAfter)
					_, err := io.WriteString(opts.Stderr, tc.startupLog)
					return err
				}

				err := startDaemon()
				if tc.wantTimeout {
					require.ErrorIs(t, err, context.DeadlineExceeded)
					assert.Contains(err.Error(), "2m0s")
					assert.Contains(err.Error(), logPath)
					assert.NotContains(err.Error(), "previous startup failure")
					if tc.wantMessage != "" {
						assert.Contains(err.Error(), tc.wantMessage)
					}
				} else {
					require.NoError(t, err)
				}
				assert.Equal(1, spawnCalls)
				if tc.readyAfter == time.Second {
					assert.Empty(progress.String())
				} else {
					assert.Contains(progress.String(), "Still waiting for daemon startup")
					assert.Contains(progress.String(), logPath)
					assert.NotContains(progress.String(), "previous startup failure")
					if tc.wantMessage != "" {
						assert.Contains(progress.String(), tc.wantMessage)
					}
				}
			})
		})
	}
}

func TestDaemonSearchOpensDerivedSidecarWithoutEmbeddingsAndClosesIt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reviews.db")
	db, err := storage.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	search, err := newDaemonSearch(t.Context(), db, dbPath, config.DefaultConfig())
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(filepath.Dir(dbPath), "reviews.search.db"), search.path)
	assert.FileExists(t, search.path)

	result, err := search.service.Search(t.Context(), searchindex.SearchParams{
		Query: "needle", Mode: searchindex.ModeLexical, Limit: 20,
	})
	require.NoError(t, err)
	assert.Empty(t, result.Hits)
	assert.Equal(t, searchindex.ModeLexical, result.Mode)

	require.NoError(t, search.Close())
	_, _, err = search.index.ActiveGeneration(t.Context())
	require.Error(t, err)
}

func TestDaemonSearchClosesSidecarOnConstructionFailures(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*config.Config)
		wantError  string
		notInError string
	}{
		{
			name: "partial config",
			configure: func(cfg *config.Config) {
				cfg.Search.Embeddings = &embedconfig.Embedder{BaseURL: "https://embeddings.example"}
			},
			wantError: "search.embeddings: embed base_url, model, and positive dims",
		},
		{
			name: "invalid embedding client",
			configure: func(cfg *config.Config) {
				cfg.Search.Embeddings = &embedconfig.Embedder{
					BaseURL: "file:///tmp/provider", Model: "model", Dims: 2,
				}
			},
			wantError: "embed endpoint must use http or https",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "reviews")
			db, err := storage.Open(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })

			originalClose := closeDaemonSearchIndex
			closeCalls := 0
			closeDaemonSearchIndex = func(index *searchindex.Index) error {
				closeCalls++
				return originalClose(index)
			}
			t.Cleanup(func() { closeDaemonSearchIndex = originalClose })

			cfg := config.DefaultConfig()
			tc.configure(cfg)
			_, err = newDaemonSearch(t.Context(), db, dbPath, cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
			if tc.notInError != "" {
				assert.NotContains(t, err.Error(), tc.notInError)
			}
			assert.Equal(t, 1, closeCalls)
		})
	}
}

type fakeDaemonLifecycle struct {
	startErr error
	stopErr  error
	starts   int
	stops    int
}

func (f *fakeDaemonLifecycle) Start(context.Context) error {
	f.starts++
	return f.startErr
}

func (f *fakeDaemonLifecycle) Stop() error {
	f.stops++
	return f.stopErr
}

type fakeSearchCloser struct {
	err   error
	calls int
}

func (f *fakeSearchCloser) Close() error {
	f.calls++
	return f.err
}

func TestRunDaemonWithSearchClosesSidecarOnEveryExit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		startErr error
		stopErr  error
		closeErr error
	}{
		{name: "normal shutdown"},
		{name: "startup failure", startErr: errors.New("listen failed")},
		{name: "shutdown failure", stopErr: errors.New("drain failed")},
		{name: "sidecar close failure", closeErr: errors.New("close failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &fakeDaemonLifecycle{startErr: tc.startErr, stopErr: tc.stopErr}
			search := &fakeSearchCloser{err: tc.closeErr}

			err := runDaemonWithSearch(t.Context(), server, search)
			assert.Equal(t, 1, server.starts)
			assert.Equal(t, 1, server.stops)
			assert.Equal(t, 1, search.calls)
			for _, expected := range []error{tc.startErr, tc.stopErr, tc.closeErr} {
				if expected != nil {
					require.ErrorIs(t, err, expected)
				}
			}
		})
	}
}

func TestDaemonSearchMissingCredentialStartsLexical(t *testing.T) {
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(401) }))
	defer provider.Close()
	t.Setenv("ROBOREV_TEST_EMBEDDING_KEY", "")
	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(original) })
	dbPath := filepath.Join(t.TempDir(), "reviews.db")
	db, err := storage.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "abc123", Agent: "test"})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("test-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: []byte(`{"schema_version":1,"summary":"needle review","findings":[]}`), Verdict: storage.VerdictPass}))
	cfg := config.DefaultConfig()
	cfg.Search.Embeddings = &embedconfig.Embedder{BaseURL: provider.URL, Model: "test", Dims: 2, APIKey: secretref.Ref{Env: "ROBOREV_TEST_EMBEDDING_KEY"}, TrustPrivateNetwork: true}
	search, err := newDaemonSearch(t.Context(), db, dbPath, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, search.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- search.reconciler.Run(ctx) }()
	require.Eventually(t, func() bool { return search.reconciler.Health().MirrorComplete }, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	health := search.reconciler.Health()
	assert.Equal(t, "missing", health.Credential)
	assert.Equal(t, "env:ROBOREV_TEST_EMBEDDING_KEY", health.CredentialSource)
	result, err := search.service.Search(t.Context(), searchindex.SearchParams{Query: "needle"})
	require.NoError(t, err)
	require.Len(t, result.Hits, 1)
	assert.Equal(t, job.ID, result.Hits[0].JobID)
	assert.Equal(t, searchindex.ModeLexical, result.Mode)
	assert.True(t, result.Degraded)
	assert.Contains(t, result.DegradedReason, "no embedding API key")
	assert.Contains(t, result.DegradedReason, "ROBOREV_TEST_EMBEDDING_KEY")
	assert.True(t, result.Coverage.EmbeddingsConfigured)
	assert.Equal(t, searchindex.VectorUnavailable, result.Coverage.VectorState)
	for _, mode := range []searchindex.SearchMode{searchindex.ModeSemantic, searchindex.ModeHybrid} {
		_, err = search.service.Search(t.Context(), searchindex.SearchParams{Query: "needle", Mode: mode})
		var modeErr *searchindex.ModeError
		require.ErrorAs(t, err, &modeErr)
		assert.Equal(t, 503, modeErr.Status)
		assert.Equal(t, result.DegradedReason, modeErr.Reason)
	}
	assert.Zero(t, requests.Load())
	assert.NotContains(t, logs.String(), "embedding API key")
}

func TestDaemonSearchCredentialFiles(t *testing.T) {
	for _, tc := range []struct {
		name, contents, credential, reason string
		mode                               os.FileMode
	}{
		{"private", "example-key\n", "ok", "", 0o600},
		{"empty", "\n", "missing", "file is empty", 0o600},
		{"insecure", "example-key", "missing", "must be private", 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && tc.name == "insecure" {
				t.Skip("Unix permissions")
			}
			path := filepath.Join(t.TempDir(), "embedding.key")
			file, err := safefileio.CreatePrivateFile(path)
			require.NoError(t, err)
			_, err = file.WriteString(tc.contents)
			require.NoError(t, err)
			require.NoError(t, file.Close())
			if tc.mode != 0o600 {
				require.NoError(t, os.Chmod(path, tc.mode))
			}
			dbPath := filepath.Join(t.TempDir(), "reviews.db")
			db, err := storage.Open(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			cfg := config.DefaultConfig()
			cfg.Search.Embeddings = &embedconfig.Embedder{BaseURL: "https://api.example.test/v1", Model: "test", Dims: 2, APIKey: secretref.Ref{File: path}}
			search, err := newDaemonSearch(t.Context(), db, dbPath, cfg)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, search.Close()) })
			health := search.reconciler.Health()
			assert.Equal(t, tc.credential, health.Credential)
			assert.Equal(t, "file:"+path, health.CredentialSource)
			if tc.reason != "" {
				assert.Contains(t, health.CredentialReason, tc.reason)
			} else {
				assert.Empty(t, health.CredentialReason)
			}
			assert.NotContains(t, health.CredentialReason, "example-key")
		})
	}
}
