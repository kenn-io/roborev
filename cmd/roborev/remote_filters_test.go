package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/cmd/roborev/tui"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

func TestRepoFilterValue(t *testing.T) {
	withRemoteState(t)
	serverAddr = ""
	require.NoError(t, validateServerFlag())
	repo := newTestGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, ".roborev-id"), []byte("example/project\n"), 0o644))

	got, err := repoFilterValue(repo.Dir)
	require.NoError(t, err)
	assert.Equal(t, repo.Dir, got, "local mode sends the path")

	serverAddr = "http://daemon-host.example:7474"
	require.NoError(t, validateServerFlag())
	got, err = repoFilterValue(repo.Dir)
	require.NoError(t, err)
	assert.Equal(t, "example/project", got, "remote mode sends the identity")
}

func TestListRemoteSendsIdentity(t *testing.T) {
	mux := http.NewServeMux()
	queries := make(chan []string, 1)
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		queries <- r.URL.Query()["repo"]
		respondJSON(w, http.StatusOK, map[string]any{"jobs": []storage.ReviewJob{}})
	})
	withRemoteMock(t, mux)
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial")
	writeRoborevID(t, repo)
	chdir(t, repo.Dir)

	cmd := listCmd()
	cmd.SetArgs([]string{})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, []string{"example/project"}, <-queries)
}

func TestRemoteRepoRoot(t *testing.T) {
	tests := []struct {
		name    string
		repos   []storage.RepoWithCount
		want    string
		wantErr string
	}{
		{
			name: "one match",
			repos: []storage.RepoWithCount{
				{Name: "project", RootPath: "/srv/project", Identity: "example/project"},
				{Name: "other", RootPath: "/srv/other", Identity: "example/other"},
			},
			want: "/srv/project",
		},
		{
			name:    "no match",
			repos:   []storage.RepoWithCount{{Name: "other", RootPath: "/srv/other", Identity: "example/other"}},
			wantErr: "repo example/project is not registered on the remote daemon",
		},
		{
			name: "two matches",
			repos: []storage.RepoWithCount{
				{Name: "project", RootPath: "/srv/project", Identity: "example/project"},
				{Name: "project", RootPath: "/srv/project-copy", Identity: "example/project"},
			},
			wantErr: "repo example/project matches several remote daemon checkouts: /srv/project, /srv/project-copy",
		},
	}
	repo := newTestGitRepo(t)
	writeRoborevID(t, repo)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/repos", func(w http.ResponseWriter, r *http.Request) {
				respondJSON(w, http.StatusOK, map[string]any{"repos": tt.repos})
			})
			withRemoteMock(t, mux)

			got, err := remoteRepoRoot(context.Background(), getDaemonEndpoint(), repo.Dir)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMCPRemoteProbesRemoteEndpoint(t *testing.T) {
	withRemoteState(t)
	serverAddr = "http://daemon-host.example:7474"
	require.NoError(t, validateServerFlag())
	startDaemonForEnsure = func() error { panic("remote MCP must not start a local daemon") }
	restartDaemonForEnsure = func() error { panic("remote MCP must not restart a local daemon") }
	probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		panic("remote MCP must not probe a local daemon")
	}
	var probed []string
	probeRemoteDaemon = func(ep daemon.DaemonEndpoint, _ time.Duration) (*daemon.PingInfo, error) {
		probed = append(probed, ep.Address)
		return nil, errors.New("connection refused")
	}

	err := ensureMCPDaemon()
	require.ErrorContains(t, err, "remote daemon at http://daemon-host.example:7474 is not reachable")
	assert.Equal(t, []string{"daemon-host.example:7474"}, probed)
}

func TestRemoteFilterCommandsSendIdentity(t *testing.T) {
	tests := []struct {
		name string
		path string
		cmd  func() *cobra.Command
		args func(repoDir string) []string
	}{
		{"cost", "/api/cost", costCmd, func(string) []string { return nil }},
		{"summary", "/api/summary", summaryCmd, func(string) []string { return nil }},
		{"stream", "/api/stream/events", streamCmd, func(dir string) []string { return []string{"--repo", dir} }},
		{"search", "/api/search", searchCmd, func(dir string) []string { return []string{"needle", "--repo", dir} }},
		{"wait", "/api/jobs", waitCmd, func(string) []string { return nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			queries := make(chan []string, 1)
			mux.HandleFunc(tt.path, func(w http.ResponseWriter, r *http.Request) {
				queries <- r.URL.Query()["repo"]
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			withRemoteMock(t, mux)
			repo := newTestGitRepo(t)
			repo.CommitFile("file.txt", "content", "initial")
			writeRoborevID(t, repo)
			chdir(t, repo.Dir)

			cmd := tt.cmd()
			cmd.SetArgs(tt.args(repo.Dir))
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			// The mock answers 503, so the command fails after the request.
			_ = cmd.Execute()
			require.Len(t, queries, 1, "the command never reached %s", tt.path)
			assert.Equal(t, []string{"example/project"}, <-queries)
		})
	}
}

func TestListRemoteQuery(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantRepo []string
	}{
		{name: "outside a repo lists every repo", args: nil},
		{name: "daemon root path passes through", args: []string{"--repo", "/srv/project"}, wantRepo: []string{"/srv/project"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			queries := make(chan url.Values, 1)
			mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
				queries <- r.URL.Query()
				respondJSON(w, http.StatusOK, map[string]any{"jobs": []storage.ReviewJob{}})
			})
			withRemoteMock(t, mux)
			chdir(t, t.TempDir())

			cmd := listCmd()
			cmd.SetArgs(tt.args)
			require.NoError(t, cmd.Execute())
			q := <-queries
			assert.False(t, q.Has("repo_prefix"), "remote mode must not send a path prefix")
			assert.Equal(t, tt.wantRepo, q["repo"])
		})
	}
}

// withCapturedTUI replaces the TUI with a recorder of its config.
func withCapturedTUI(t *testing.T) *tui.Config {
	t.Helper()
	orig := runTUI
	t.Cleanup(func() { runTUI = orig })
	var got tui.Config
	runTUI = func(cfg tui.Config) error {
		got = cfg
		return nil
	}
	return &got
}

func TestTUIRemoteRepo(t *testing.T) {
	registered := []storage.RepoWithCount{{Name: "project", RootPath: "/srv/project", Identity: "example/project"}}
	tests := []struct {
		name           string
		args           []string
		autoFilter     bool
		repos          []storage.RepoWithCount
		wantRepo       string
		wantRemoteRoot string
		wantErr        string
		wantRepoCalls  int32
	}{
		{name: "local checkout maps to the daemon root", args: []string{"--repo=CHECKOUT"}, repos: registered, wantRepo: "/srv/project", wantRepoCalls: 1},
		{name: "daemon root path passes through", args: []string{"--repo=/srv/other"}, wantRepo: "/srv/other"},
		{name: "relative non-checkout is refused", args: []string{"--repo=example/project"}, wantErr: "neither a local checkout nor an absolute daemon root path"},
		{name: "current branch needs a local checkout", args: []string{"--repo=/srv/other", "--branch"}, wantErr: "use --branch=<name>"},
		{name: "daemon root path with a named branch", args: []string{"--repo=/srv/other", "--branch=main"}, wantRepo: "/srv/other"},
		{name: "auto filter finds the daemon root", autoFilter: true, repos: registered, wantRemoteRoot: "/srv/project", wantRepoCalls: 1},
		{name: "auto filter on an unregistered repo starts unfiltered", autoFilter: true, wantRepoCalls: 1},
		{name: "no auto filter skips the repo lookup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			mux := http.NewServeMux()
			var repoCalls atomic.Int32
			mux.HandleFunc("/api/repos", func(w http.ResponseWriter, r *http.Request) {
				repoCalls.Add(1)
				respondJSON(w, http.StatusOK, map[string]any{"repos": tt.repos})
			})
			withRemoteMock(t, mux)
			if tt.autoFilter {
				require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("ROBOREV_DATA_DIR"), "config.toml"),
					[]byte("auto_filter_repo = true\n"), 0o644))
			}
			got := withCapturedTUI(t)
			repo := newTestGitRepo(t)
			repo.CommitFile("file.txt", "content", "initial")
			writeRoborevID(t, repo)
			chdir(t, repo.Dir)

			args := make([]string, len(tt.args))
			for i, arg := range tt.args {
				args[i] = strings.ReplaceAll(arg, "CHECKOUT", repo.Dir)
			}
			cmd := tuiCmd()
			cmd.SetArgs(args)
			err := cmd.Execute()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.True(got.Remote)
			assert.Equal(tt.wantRepo, got.RepoFilter)
			assert.Equal(tt.wantRemoteRoot, got.RemoteRepoRoot)
			assert.Equal(tt.wantRepoCalls, repoCalls.Load())
		})
	}
}
