package katatest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RealKata is a disposable Kata home and workspace bound to project e2etest.
type RealKata struct {
	Env  []string // complete, allowlisted environment for every kata subprocess
	Repo string   // workspace holding the .kata.toml binding
}

// StartReal initializes a disposable Kata and stops its daemon at cleanup,
// skipping the test when kata is not installed. Setup and cleanup both run with
// the allowlisted environment (IsolatedEnv), so neither can reach a Kata the
// caller has selected through the process environment.
func StartReal(t *testing.T, ctx context.Context) RealKata {
	t.Helper()
	if _, err := exec.LookPath("kata"); err != nil {
		t.Skip("kata not on PATH")
	}
	home, kataHome, repo := t.TempDir(), t.TempDir(), t.TempDir()
	env := IsolatedEnv(os.Environ(), home, kataHome)

	t.Cleanup(func() {
		// Fresh bounded context: the test's ctx may already be expired or
		// canceled by the time cleanup runs.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		stop := exec.CommandContext(stopCtx, "kata", "daemon", "stop")
		stop.Dir = repo
		stop.Env = env
		out, err := stop.CombinedOutput()
		assert.NoError(t, err, "kata daemon stop must reach the disposable daemon: %s", out)
	})

	// Bootstrap: register the project and write the committed .kata.toml
	// binding. kata create rejects an unregistered project, so this must run
	// before any client call. Bounded by ctx so a hung kata cannot block until
	// the global test timeout.
	initCmd := exec.CommandContext(ctx, "kata", "init", "--project", "e2etest")
	initCmd.Dir = repo
	initCmd.Env = env
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "kata init: %s", out)
	return RealKata{Env: env, Repo: repo}
}

// SocketEndpoint returns the unix:// endpoint of the disposable daemon.
func (k RealKata) SocketEndpoint(t *testing.T, ctx context.Context) string {
	t.Helper()
	locate := exec.CommandContext(ctx, "kata", "daemon", "locate")
	locate.Dir = k.Repo
	locate.Env = k.Env
	out, err := locate.Output()
	require.NoError(t, err, "kata daemon locate")
	endpoint := strings.TrimSpace(string(out))
	require.True(t, strings.HasPrefix(endpoint, "unix://"), "unexpected endpoint %q", endpoint)
	return endpoint
}
