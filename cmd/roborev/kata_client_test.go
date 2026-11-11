package main

import (
	"context"
	"encoding/json"
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
	katagen "go.kenn.io/kata/pkg/client/generated"

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
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"), []byte("version = 1\n[project]\nname = \""+project+"\"\n"), 0o600))
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
	issue := fake.AddIssue(7, "aaa1", "In reviews", "the body", "bug")
	issue.Out.Metadata = map[string]any{"text": "value", "number": 42, "nested": map[string]any{"enabled": true}}
	fake.AddIssue(8, "bbb2", "In other", "")
	ep := kataDaemon(t, enabledKata(fake))
	var probes atomic.Int32
	ctx := t.Context()
	direct, err := kata.NewAPIClient("", enabledKata(fake), kata.WithoutWorkspace())
	require.NoError(t, err)
	t.Cleanup(direct.CloseIdleConnections)
	project, err := direct.ResolvePublicationProject(ctx, "reviews")
	require.NoError(t, err)
	full, err := direct.ShowPublicationIssue(ctx, project, "aaa1")
	require.NoError(t, err)
	assert.Equal(t, "value", full.Issue.Metadata["text"])
	assert.EqualValues(t, 42, full.Issue.Metadata["number"])
	result, err := ep.APIClient(0).KataShowIssue(ctx, &generated.KataShowIssueRequestOptions{Query: &generated.KataShowIssueQuery{Ref: "aaa1", Project: new("reviews")}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"text":"value","number":42,"nested":{"enabled":true}}`, string(result.Metadata))

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
	assert.NoFileExists(t, marker, "the kata CLI must not run")
}

func TestKataClientCommittedBindingUsesRegisteredAliasThroughDaemon(t *testing.T) {
	repo := testutil.NewTestRepo(t)
	repo.WriteFile(".kata.toml", "version = 1\n[project]\nname = \"reviews\"\n")
	repo.RunGit("remote", "add", "origin", "https://code.example/Team/Review-Repo.git")

	fake := katafake.New(t)
	fake.AddIssue(7, "abc4", "Old project name reused", "")
	fake.AddIssue(8, "abc4", "Renamed project", "")
	fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/projects/resolve" {
			return false
		}
		var req struct {
			Alias struct {
				Identity string `json:"identity"`
				Kind     string `json:"kind"`
			} `json:"alias"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "code.example/Team/Review-Repo", req.Alias.Identity)
		assert.Equal(t, "git", req.Alias.Kind)
		fake.Reply(w, katagen.ProjectResolveBody{
			Project: fake.Projects[1],
			Alias: katagen.ProjectAlias{
				AliasIdentity: req.Alias.Identity, AliasKind: req.Alias.Kind,
				ID: 1, ProjectID: fake.Projects[1].ID, CreatedAt: time.Unix(0, 0).UTC(),
			},
		})
		return true
	}
	ep := kataDaemon(t, enabledKata(fake))
	c := newKataClientAt(repo.Root, config.DefaultConfig(), func() (daemon.DaemonEndpoint, error) { return ep, nil })

	binding, err := c.Binding(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "other", binding.Project, "the committed alias supplies the canonical name for reference parsing")
	require.NotNil(t, binding.Alias)
	assert.Equal(t, "code.example/Team/Review-Repo", binding.Alias.Identity)
	assert.Equal(t, "git", binding.Alias.Kind)

	contextResult, err := kata.ResolveContext(t.Context(), c, kata.ModeCurrent, []string{"fix other#abc4"})
	require.NoError(t, err)
	require.Len(t, contextResult.Issues, 1)
	assert.Equal(t, "Renamed project", contextResult.Issues[0].Title,
		"a reused stale project name cannot hide a reference in the renamed project")

	issues, err := c.List(t.Context(), kata.ListOpts{})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Renamed project", issues[0].Title, "a reused old name cannot select the wrong project")

	shown, err := c.Show(t.Context(), "abc4")
	require.NoError(t, err)
	assert.Equal(t, "other#abc4", shown.QualifiedID)

	var paths []string
	for _, request := range fake.Requests() {
		paths = append(paths, request.Method+" "+request.Path)
	}
	// Each complete issue read verifies the selected project's stable UID first.
	assert.Equal(t, []string{
		"GET /api/v1/projects",
		"POST /api/v1/projects/resolve",
		"POST /api/v1/projects/resolve",
		"POST /api/v1/projects/resolve", "GET /api/v1/projects", "GET /api/v1/projects/8/issues/abc4",
		"POST /api/v1/projects/resolve", "GET /api/v1/projects/8/issues",
		"POST /api/v1/projects/resolve", "GET /api/v1/projects", "GET /api/v1/projects/8/issues/abc4",
	}, paths, "committed aliases resolve canonically")

	fake.Token = "rotated-credential"
	_, err = c.Binding(t.Context())
	require.EqualError(t, err, "kata: Kata rejected the credential, or no usable credential is configured.")
}

func TestKataClientProbesOnFirstUse(t *testing.T) {
	fake := katafake.New(t)
	ep := kataDaemon(t, enabledKata(fake))
	var probes atomic.Int32
	c := newKataClientAt(bindRepo(t, "reviews"), config.DefaultConfig(), fixedEndpoint(ep, &probes))
	assert.Zero(t, probes.Load(), "building a client contacts nothing; most commands never use Kata")
	_, err := c.Binding(t.Context())
	require.NoError(t, err)
	_, err = c.List(t.Context(), kata.ListOpts{})
	require.NoError(t, err)
	_, err = c.List(t.Context(), kata.ListOpts{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, probes.Load(), "the daemon is asked once per client")
}

func TestKataClientKeepsTheCLIWhenTheConnectionIsOff(t *testing.T) {
	var probes atomic.Int32
	t.Run("daemon reachable and off", func(t *testing.T) {
		ep := kataDaemon(t, config.KataIntegrationConfig{})
		c := newKataClientAt(bindRepo(t, "p"), config.DefaultConfig(), fixedEndpoint(ep, &probes))
		_, isCLI := c.(*lazyKataClient).resolve(t.Context()).(*kata.CLIClient)
		assert.True(t, isCLI)
	})
	t.Run("no daemon", func(t *testing.T) {
		c := newKataClientAt(bindRepo(t, "p"), config.DefaultConfig(), func() (daemon.DaemonEndpoint, error) {
			return daemon.DaemonEndpoint{}, os.ErrNotExist
		})
		_, isCLI := c.(*lazyKataClient).resolve(t.Context()).(*kata.CLIClient)
		assert.True(t, isCLI, "today's behavior when nothing enables the connection")
	})
	t.Run("nil config", func(t *testing.T) {
		c := newKataClientAt(bindRepo(t, "p"), nil, func() (daemon.DaemonEndpoint, error) { return daemon.DaemonEndpoint{}, os.ErrNotExist })
		_, isCLI := c.(*lazyKataClient).resolve(t.Context()).(*kata.CLIClient)
		assert.True(t, isCLI)
	})
}

func TestKataClientExplicitServerProbe(t *testing.T) {
	marker := kataShimMarker(t)
	previous, previousEndpoint := serverAddr, parsedServerEndpoint
	t.Cleanup(func() { serverAddr, parsedServerEndpoint = previous, previousEndpoint })
	serverAddr, parsedServerEndpoint = "http://127.0.0.1:1", nil

	for _, tc := range []struct {
		name    string
		handler http.Handler
		want    string
	}{
		{name: "status error", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"title":"Error","status":500,"detail":"synthetic daemon failure"}`))
		}), want: "synthetic daemon failure"},
		{name: "old daemon", handler: http.NotFoundHandler()},
		{name: "disabled", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"enabled":false,"state":"disabled","message":"disabled"}`))
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(tc.handler)
			defer ts.Close()
			serverAddr = ts.URL
			c := newKataClient(bindRepo(t, "p"), config.DefaultConfig())
			if tc.want == "" {
				assert.IsType(t, (*kata.CLIClient)(nil), c.(*lazyKataClient).resolve(t.Context()))
				return
			}
			_, err := c.List(t.Context(), kata.ListOpts{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
	serverAddr = "http://127.0.0.1:1"
	c := newKataClientAt(bindRepo(t, "p"), config.DefaultConfig(), func() (daemon.DaemonEndpoint, error) {
		return daemon.DaemonEndpoint{}, os.ErrNotExist
	})
	_, err := c.List(t.Context(), kata.ListOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not be reached")
	assert.NoFileExists(t, marker)
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
		ctx, cancel := context.WithCancel(t.Context())
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
	previous := serverAddr
	serverAddr = "http://127.0.0.1:1"
	t.Cleanup(func() { serverAddr = previous })
	synctest.Test(t, func(t *testing.T) {
		ep, err := daemon.ParseEndpoint("http://127.0.0.1:1")
		require.NoError(t, err)
		client := newKataClientAt(bindRepo(t, "p"), config.DefaultConfig(), func() (daemon.DaemonEndpoint, error) {
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

		ctx := t.Context()
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
	ctx := t.Context()

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
	_, err := c.List(t.Context(), kata.ListOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost")

	fake.Token = "rotated-credential"
	c = newKataClientAt(bindRepo(t, "reviews"), config.DefaultConfig(), fixedEndpoint(ep, &probes))
	_, err = c.List(t.Context(), kata.ListOpts{})
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
	_, err := c.Binding(t.Context())
	require.ErrorIs(t, err, kata.ErrNoBinding, "so the review is not given Kata context from nowhere")
	_, err = c.List(t.Context(), kata.ListOpts{})
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
			_, err = c.List(t.Context(), kata.ListOpts{})
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
	_, isCLI := c.(*lazyKataClient).resolve(t.Context()).(*kata.CLIClient)
	assert.True(t, isCLI)
	assert.NoFileExists(t, marker)
}
