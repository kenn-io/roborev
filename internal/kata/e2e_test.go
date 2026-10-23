//go:build kata

package kata_test

import (
	"context"
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
)

// TestCLIClientE2EIgnoresInheritedKataRouting proves the isolation: with
// KATA_SERVER pointing at a closed port and a token and identity in the
// environment, the full client flow still reaches only the disposable daemon.
func TestCLIClientE2EIgnoresInheritedKataRouting(t *testing.T) {
	if _, err := exec.LookPath("kata"); err != nil {
		t.Skip("kata not on PATH")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closedAddr := listener.Addr().String()
	require.NoError(t, listener.Close())

	t.Setenv("KATA_SERVER", "http://"+closedAddr)
	t.Setenv("KATA_AUTH_TOKEN", "inherited-token")
	t.Setenv("KATA_AUTHOR", "inherited-author")
	t.Setenv("KATA_TEAMMATE", "inherited-teammate")
	t.Setenv("KATA_HOME", t.TempDir())
	runCLIClientE2E(t)
}

func runCLIClientE2E(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	k := katatest.StartReal(t, ctx)
	repo, env := k.Repo, k.Env

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
