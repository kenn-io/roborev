//go:build kata

package kata_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
)

// TestCLIClientE2E exercises the real kata binary against an isolated
// KATA_HOME so it never touches a developer's production kata data or
// daemon. Run with: go test -tags kata ./internal/kata/
//
// kata namespaces its database, runtime directory, and daemon socket under
// KATA_HOME (<KATA_HOME>/kata.db and <KATA_HOME>/runtime/<dbhash>), so a
// KATA_HOME-scoped `kata daemon stop` only stops the daemon this test starts.
func TestCLIClientE2E(t *testing.T) {
	if _, err := exec.LookPath("kata"); err != nil {
		t.Skip("kata not on PATH")
	}
	home := t.TempDir()
	repo := t.TempDir()
	env := []string{"KATA_HOME=" + home}

	t.Cleanup(func() {
		// Fresh bounded context: the test's ctx may already be expired or
		// canceled by the time cleanup runs.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		stop := exec.CommandContext(stopCtx, "kata", "daemon", "stop")
		stop.Dir = repo
		stop.Env = append(os.Environ(), env...)
		_ = stop.Run() // best-effort
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Bootstrap: register the project and write the committed .kata.toml
	// binding. kata create rejects an unregistered project, so this must run
	// before any client call. Bounded by ctx so a hung kata cannot block until
	// the global test timeout.
	initCmd := exec.CommandContext(ctx, "kata", "init", "--project", "e2etest")
	initCmd.Dir = repo
	initCmd.Env = append(os.Environ(), env...)
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "kata init: %s", out)

	c := kata.NewCLIClientWithEnv(repo, env)

	// Binding reads the .kata.toml that `kata init` wrote.
	b, err := c.Binding(ctx)
	require.NoError(t, err)
	assert.Equal(t, "e2etest", b.Project)

	// Create, then create again with the same idempotency key.
	req := kata.CreateReq{Title: "seed task", Body: "do the thing", Project: "e2etest", IdempotencyKey: "e2e:1"}
	res, err := c.Create(ctx, req)
	require.NoError(t, err)
	require.NotEmpty(t, res.ShortID)
	assert.False(t, res.Reused)

	res2, err := c.Create(ctx, req)
	require.NoError(t, err)
	assert.True(t, res2.Reused, "same idempotency-key must reuse")
	assert.Equal(t, res.ShortID, res2.ShortID)

	// List returns the created issue.
	issues, err := c.List(ctx, kata.ListOpts{Status: "open"})
	require.NoError(t, err)
	var found bool
	for _, iss := range issues {
		if iss.ShortID == res.ShortID {
			found = true
		}
	}
	assert.True(t, found, "created issue should appear in open list")

	// Show returns the issue detail.
	iss, err := c.Show(ctx, res.ShortID)
	require.NoError(t, err)
	assert.Equal(t, "seed task", iss.Title)
	assert.Contains(t, iss.Body, "do the thing")
}
