package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
