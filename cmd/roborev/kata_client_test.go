package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/client/generated"
)

const kataCLICredential = "synthetic-daemon-only-credential"

// kataDaemon starts a real daemon server whose Kata connection points at fake.
// The credential exists only in the daemon's configuration.
func kataDaemon(t *testing.T, kataCfg config.KataIntegrationConfig) daemon.DaemonEndpoint {
	t.Helper()
	return kataDaemonObserved(t, kataCfg, nil)
}

// kataDaemonObserved is kataDaemon with a hook that sees every request the
// daemon receives.
func kataDaemonObserved(t *testing.T, kataCfg config.KataIntegrationConfig, observe func(*http.Request)) daemon.DaemonEndpoint {
	t.Helper()
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	db := testutil.OpenTestDB(t)
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = kataCfg
	srv := daemon.NewServer(db, cfg, "")
	t.Cleanup(func() { _ = srv.Close() })
	handler := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if observe != nil {
			observe(r)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	ep, err := daemon.ParseEndpoint(ts.URL)
	require.NoError(t, err)
	return ep
}

func enabledKata(fake *katafake.Server) config.KataIntegrationConfig {
	return config.KataIntegrationConfig{
		Enabled: true, Endpoint: fake.URL(), APIKey: kataCLICredential, DefaultProject: "reviews", Teammate: "roborev",
	}
}

func fixedEndpoint(ep daemon.DaemonEndpoint, calls *atomic.Int32) func() (daemon.DaemonEndpoint, error) {
	return func() (daemon.DaemonEndpoint, error) {
		calls.Add(1)
		return ep, nil
	}
}

func bindRepo(t *testing.T, project string) string {
	t.Helper()
	dir := t.TempDir()
	if project != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("[project]\nname = \""+project+"\"\n"), 0o600))
	}
	return dir
}

func kataShimMarker(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the kata executable")
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "kata-was-run")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "kata"), []byte("#!/bin/sh\necho \"$@\" >> "+marker+"\nexit 1\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

// The CLI has no Kata credential and no [integrations.kata] of its own, as on a
// machine that talks to a remote daemon. The daemon owns the connection, so
// reads and writes reach Kata through it and never start the kata CLI.
func TestKataClientUsesTheDaemonsConnection(t *testing.T) {
	marker := kataShimMarker(t)
	fake := katafake.New(t)
	fake.AddIssue(7, "aaa1", "In reviews", "the body", "bug")
	fake.AddIssue(8, "bbb2", "In other", "")
	ep := kataDaemon(t, enabledKata(fake))
	var probes atomic.Int32
	ctx := context.Background()

	t.Run("unbound repository reads the daemon's default project", func(t *testing.T) {
		c := newKataClientAt(bindRepo(t, ""), config.DefaultConfig(), fixedEndpoint(ep, &probes))
		b, err := c.Binding(ctx)
		require.NoError(t, err)
		assert.Equal(t, "reviews", b.Project)
		issues, err := c.List(ctx, kata.ListOpts{})
		require.NoError(t, err)
		require.Len(t, issues, 1)
		assert.Equal(t, "aaa1", issues[0].ShortID)
		assert.Equal(t, []string{"bug"}, issues[0].Labels)
	})
	t.Run("the repository's own binding selects the project", func(t *testing.T) {
		c := newKataClientAt(bindRepo(t, "other"), config.DefaultConfig(), fixedEndpoint(ep, &probes))
		b, err := c.Binding(ctx)
		require.NoError(t, err)
		assert.Equal(t, "other", b.Project)
		issues, err := c.List(ctx, kata.ListOpts{Unlimited: true})
		require.NoError(t, err)
		require.Len(t, issues, 1)
		assert.Equal(t, "bbb2", issues[0].ShortID)
		shown, err := c.Show(ctx, "bbb2")
		require.NoError(t, err)
		assert.Equal(t, "other#bbb2", shown.QualifiedID)
		qualified, err := c.Show(ctx, "reviews#aaa1")
		require.NoError(t, err)
		assert.Equal(t, "the body", qualified.Body)
	})
	t.Run("a write goes through the daemon as roborev", func(t *testing.T) {
		c := newKataClientAt(bindRepo(t, "other"), config.DefaultConfig(), fixedEndpoint(ep, &probes))
		priority := 1
		res, err := c.Create(ctx, kata.CreateReq{Title: "filed from the CLI", Body: "b", Labels: []string{"x"}, Priority: &priority, IdempotencyKey: "cli:1"})
		require.NoError(t, err)
		assert.NotEmpty(t, res.ShortID)
		assert.Equal(t, "roborev", res.Attribution.Author)
		again, err := c.Create(ctx, kata.CreateReq{Title: "filed from the CLI", Body: "b", Labels: []string{"x"}, Priority: &priority, IdempotencyKey: "cli:1"})
		require.NoError(t, err)
		assert.True(t, again.Reused)
		var post katafake.Request
		for _, r := range fake.Requests() {
			if r.Method == "POST" {
				post = r
			}
		}
		assert.Equal(t, "/api/v1/projects/8/issues", post.Path)
		assert.Equal(t, "Bearer "+kataCLICredential, post.Header.Get("Authorization"))
		assert.Equal(t, "roborev", post.JSON(t)["actor"])
	})
	assert.NoFileExists(t, marker, "the kata CLI must not run")
}

func TestKataClientProbesOnFirstUse(t *testing.T) {
	fake := katafake.New(t)
	ep := kataDaemon(t, enabledKata(fake))
	var probes atomic.Int32
	c := newKataClientAt(bindRepo(t, "reviews"), config.DefaultConfig(), fixedEndpoint(ep, &probes))
	assert.Zero(t, probes.Load(), "building a client contacts nothing; most commands never use Kata")
	_, err := c.Binding(context.Background())
	require.NoError(t, err)
	_, err = c.List(context.Background(), kata.ListOpts{})
	require.NoError(t, err)
	_, err = c.List(context.Background(), kata.ListOpts{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, probes.Load(), "the daemon is asked once per client")
}

func TestKataClientKeepsTheCLIWhenTheConnectionIsOff(t *testing.T) {
	var probes atomic.Int32
	t.Run("daemon reachable and off", func(t *testing.T) {
		ep := kataDaemon(t, config.KataIntegrationConfig{})
		c := newKataClientAt(bindRepo(t, "p"), config.DefaultConfig(), fixedEndpoint(ep, &probes))
		_, isCLI := c.(*lazyKataClient).resolve(context.Background()).(*kata.CLIClient)
		assert.True(t, isCLI)
	})
	t.Run("no daemon", func(t *testing.T) {
		c := newKataClientAt(bindRepo(t, "p"), config.DefaultConfig(), func() (daemon.DaemonEndpoint, error) {
			return daemon.DaemonEndpoint{}, os.ErrNotExist
		})
		_, isCLI := c.(*lazyKataClient).resolve(context.Background()).(*kata.CLIClient)
		assert.True(t, isCLI, "today's behavior when nothing enables the connection")
	})
	t.Run("nil config", func(t *testing.T) {
		c := newKataClientAt(bindRepo(t, "p"), nil, func() (daemon.DaemonEndpoint, error) { return daemon.DaemonEndpoint{}, os.ErrNotExist })
		_, isCLI := c.(*lazyKataClient).resolve(context.Background()).(*kata.CLIClient)
		assert.True(t, isCLI)
	})
}

func TestKataClientFallsBackWhenTheStatusProbeStalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ep, err := daemon.ParseEndpoint("http://127.0.0.1:1")
		require.NoError(t, err)
		c := newKataClientAt(bindRepo(t, "p"), config.DefaultConfig(), func() (daemon.DaemonEndpoint, error) {
			return ep, nil
		})
		client, ok := c.(*lazyKataClient)
		require.True(t, ok)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		started := make(chan struct{})
		client.statusProbe = func(ctx context.Context) (*generated.KataStatusResponse, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		resolved := make(chan kata.Client, 1)
		go func() { resolved <- client.resolve(ctx) }()
		<-started

		// The timeout is two serial Kata requests (server info and project lookup),
		// each bounded by Kata v0.19.0's five-second request budget. synctest
		// advances this deadline without waiting on wall time.
		time.Sleep(2*kata.DefaultRequestTimeout + time.Millisecond)
		synctest.Wait()
		var got kata.Client
		select {
		case got = <-resolved:
		default:
		}
		require.NotNil(t, got, "a stalled status probe must finish at its own deadline")
		assert.IsType(t, (*kata.CLIClient)(nil), got, "connection-off configs fall back to the CLI")
	})
}

func TestKataStatusProbeTimeoutDoesNotCancelReviewContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ep, err := daemon.ParseEndpoint("http://127.0.0.1:1")
		require.NoError(t, err)
		cfg := config.DefaultConfig()
		cfg.Integrations.Kata.Enabled = true
		client := newKataClientAt(bindRepo(t, "p"), cfg, func() (daemon.DaemonEndpoint, error) {
			return ep, nil
		})
		lazy, ok := client.(*lazyKataClient)
		require.True(t, ok)

		started := make(chan struct{})
		lazy.statusProbe = func(ctx context.Context) (*generated.KataStatusResponse, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}

		ctx := context.Background()
		type resolution struct {
			result kata.ContextResult
			err    error
		}
		resolved := make(chan resolution, 1)
		go func() {
			result, err := kata.ResolveContext(ctx, client, kata.ModeOpen, nil)
			resolved <- resolution{result: result, err: err}
		}()
		<-started

		time.Sleep(kataStatusProbeTimeout + time.Millisecond)
		synctest.Wait()
		var got *resolution
		select {
		case result := <-resolved:
			got = &result
		default:
		}
		require.NotNil(t, got, "a stalled status probe must finish at its own deadline")
		require.NoError(t, got.err, "a failed optional Kata connection must not abort review context resolution")
		require.Len(t, got.result.Errs, 1)
		assert.Contains(t, got.result.Errs[0].Error(), "could not be reached")
		assert.NotErrorIs(t, got.result.Errs[0], context.DeadlineExceeded)
	})
}

// A machine whose own config enables the connection must not shell out when the
// daemon that holds the credential cannot be reached or has it off.
func TestKataClientNeverShellsOutWhenTheConnectionIsEnabledLocally(t *testing.T) {
	marker := kataShimMarker(t)
	local := config.DefaultConfig()
	local.Integrations.Kata.Enabled = true
	ctx := context.Background()

	t.Run("daemon unreachable", func(t *testing.T) {
		c := newKataClientAt(bindRepo(t, "p"), local, func() (daemon.DaemonEndpoint, error) { return daemon.DaemonEndpoint{}, os.ErrNotExist })
		_, err := c.List(ctx, kata.ListOpts{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "daemon")
		require.NotErrorIs(t, err, kata.ErrUnavailable, "a missing connection is an error, not an inert Kata")
		_, err = c.Show(ctx, "abc4")
		require.Error(t, err)
		_, err = c.Create(ctx, kata.CreateReq{Title: "t"})
		require.Error(t, err)
	})
	t.Run("daemon has the connection off", func(t *testing.T) {
		var probes atomic.Int32
		ep := kataDaemon(t, config.KataIntegrationConfig{})
		c := newKataClientAt(bindRepo(t, "p"), local, fixedEndpoint(ep, &probes))
		_, err := c.List(ctx, kata.ListOpts{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "disabled on the daemon")
	})
	assert.NoFileExists(t, marker)
}

func TestKataClientReportsWhatTheDaemonSaid(t *testing.T) {
	fake := katafake.New(t)
	cfg := enabledKata(fake)
	var probes atomic.Int32
	ep := kataDaemon(t, cfg)
	c := newKataClientAt(bindRepo(t, "ghost"), config.DefaultConfig(), fixedEndpoint(ep, &probes))
	_, err := c.List(context.Background(), kata.ListOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost")

	fake.Token = "rotated-credential"
	c = newKataClientAt(bindRepo(t, "reviews"), config.DefaultConfig(), fixedEndpoint(ep, &probes))
	_, err = c.List(context.Background(), kata.ListOpts{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), kataCLICredential)
	assert.NotContains(t, err.Error(), "rotated-credential")
}

func TestKataClientWithoutAnyProjectIsUnbound(t *testing.T) {
	fake := katafake.New(t)
	cfg := enabledKata(fake)
	cfg.DefaultProject = ""
	var probes atomic.Int32
	c := newKataClientAt(bindRepo(t, ""), config.DefaultConfig(), fixedEndpoint(kataDaemon(t, cfg), &probes))
	_, err := c.Binding(context.Background())
	require.ErrorIs(t, err, kata.ErrNoBinding, "so the review is not given Kata context from nowhere")
	_, err = c.List(context.Background(), kata.ListOpts{})
	require.ErrorIs(t, err, kata.ErrNoBinding)
}

// A daemon that is running but answers the status request with an error is not
// unreachable. One that predates the endpoint answers 404 and needs a restart.
func TestKataClientNamesWhyTheDaemonCannotProvideTheConnection(t *testing.T) {
	marker := kataShimMarker(t)
	local := config.DefaultConfig()
	local.Integrations.Kata.Enabled = true
	for _, tc := range []struct {
		name    string
		status  int
		wantIn  string
		notWant string
	}{
		{"predates the endpoint", http.StatusNotFound, "restart", "could not be reached"},
		{"predates the endpoint and answers in plain text", -http.StatusNotFound, "restart", "could not be reached"},
		{"refuses the request", http.StatusUnauthorized, "daemon answered", "could not be reached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A real old daemon has no such route, so its mux answers
			// "404 page not found" as plain text; a negative status marks that.
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"title":"Error","status":` + strconv.Itoa(tc.status) + `,"detail":"nope"}`))
			})
			if tc.status < 0 {
				handler = http.NotFound
			}
			ts := httptest.NewServer(handler)
			defer ts.Close()
			ep, err := daemon.ParseEndpoint(ts.URL)
			require.NoError(t, err)
			c := newKataClientAt(bindRepo(t, "p"), local, func() (daemon.DaemonEndpoint, error) { return ep, nil })
			_, err = c.List(context.Background(), kata.ListOpts{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantIn)
			assert.NotContains(t, err.Error(), tc.notWant)
		})
	}
	// Without a local opt-in the same daemon leaves the CLI in charge, as before.
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	ep, err := daemon.ParseEndpoint(ts.URL)
	require.NoError(t, err)
	c := newKataClientAt(bindRepo(t, "p"), config.DefaultConfig(), func() (daemon.DaemonEndpoint, error) { return ep, nil })
	_, isCLI := c.(*lazyKataClient).resolve(context.Background()).(*kata.CLIClient)
	assert.True(t, isCLI)
	assert.NoFileExists(t, marker)
}
