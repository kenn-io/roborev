//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// TestRemoteClientAgainstRealHandler joins the real remote client to the
// real remote handler: the enqueue gets 409 for a commit the daemon clone
// lacks, the client uploads a pack, the retry succeeds, and a repo filter by
// identity finds the job.
func TestRemoteClientAgainstRealHandler(t *testing.T) {
	withRemoteState(t)
	// The daemon stores resolved checkout paths, so register the clone
	// under its resolved path (t.TempDir is behind a symlink on macOS).
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	upstream := filepath.Join(root, "upstream.git")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", upstream)
	seed := newTestGitRepo(t)
	seed.CommitFile("base.txt", "base", "base")
	gitIn(t, seed.Dir, "push", "-q", upstream, "HEAD:refs/heads/main")
	daemonClone := filepath.Join(root, "daemon")
	laptop := filepath.Join(root, "laptop")
	gitIn(t, root, "clone", "-q", upstream, daemonClone)
	gitIn(t, root, "clone", "-q", upstream, laptop)
	require.NoError(t, os.WriteFile(filepath.Join(laptop, ".roborev-id"), []byte("example/project\n"), 0o644))
	gitIn(t, laptop, "add", ".roborev-id")
	gitIn(t, laptop, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "unpushed")
	target := gitIn(t, laptop, "rev-parse", "HEAD")
	require.False(t, gitCatFileOK(daemonClone, target), "the daemon clone starts without the commit")

	db := testutil.OpenTestDB(t)
	_, err = db.GetOrCreateRepo(daemonClone, "example/project")
	require.NoError(t, err)
	server := daemon.NewServer(db, config.DefaultConfig(), "")
	t.Cleanup(func() { _ = server.Close() })
	ts := server.NewRemoteTestServer(daemon.RemoteAccessQueue)
	t.Cleanup(ts.Close)
	ep := daemon.DaemonEndpoint{Network: "tcp", Address: strings.TrimPrefix(ts.URL, "http://")}

	status, body, err := remoteEnqueue(context.Background(), ep, ts.Client(), laptop,
		daemon.EnqueueRequest{GitRef: "HEAD", Branch: "main", Agent: "test"})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status, string(body))
	var job storage.ReviewJob
	require.NoError(t, json.Unmarshal(body, &job))
	assert := assert.New(t)
	assert.Equal(target, job.GitRef)
	assert.Equal(target, gitIn(t, daemonClone, "rev-parse", "refs/roborev/uploads/"+target),
		"the client uploaded the missing commit and the daemon pinned it")

	probeRemoteDaemon = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return &daemon.PingInfo{OK: true, Service: "roborev"}, nil
	}
	setRemoteEndpoint(&ep)
	chdir(t, laptop)
	out := captureStdout(t, func() {
		cmd := listCmd()
		cmd.SetArgs([]string{"--json"})
		require.NoError(t, cmd.Execute())
	})
	var jobs []storage.ReviewJob
	require.NoError(t, json.Unmarshal([]byte(out), &jobs), out)
	require.Len(t, jobs, 1)
	assert.Equal(job.ID, jobs[0].ID, "the identity filter resolves to the daemon checkout")
}
