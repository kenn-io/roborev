//go:build kata && !windows

package kata_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
)

// TestAPIClientE2E runs the API client against a real Kata daemon in a
// disposable home, reached over its private Unix socket. It covers the calls
// the connection makes: health and project checks, create with an idempotency
// key and metadata, list, and show.
func TestAPIClientE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	k := katatest.StartReal(t, ctx)
	cfg := config.KataIntegrationConfig{
		Enabled: true, Endpoint: k.SocketEndpoint(t, ctx), DefaultProject: "e2etest", Teammate: "roborev",
	}
	c, err := kata.NewAPIClient(k.Repo, cfg)
	require.NoError(t, err)
	defer c.CloseIdleConnections()

	_, err = c.Info(ctx)
	require.NoError(t, err)

	b, err := c.Binding(ctx)
	require.NoError(t, err)
	assert.Equal(t, "e2etest", b.Project)

	req := kata.CreateReq{
		Title: "roborev connection test", Body: "created through the API", Labels: []string{"roborev"},
		IdempotencyKey: "e2e:api:1", Metadata: map[string]any{"roborev.test": "marker"},
	}
	created, err := c.Create(ctx, req)
	require.NoError(t, err)
	require.NotEmpty(t, created.ShortID)
	assert.False(t, created.Reused)
	assert.Equal(t, "roborev", created.Attribution.Author, "a local socket has no credential, so the requested actor is recorded")

	reused, err := c.Create(ctx, req)
	require.NoError(t, err)
	assert.True(t, reused.Reused)
	assert.Equal(t, created.ShortID, reused.ShortID)

	shown, err := c.Show(ctx, created.ShortID)
	require.NoError(t, err)
	assert.Equal(t, "roborev connection test", shown.Title)
	assert.Equal(t, "e2etest#"+created.ShortID, shown.QualifiedID)
	assert.Equal(t, []string{"roborev"}, shown.Labels)

	listed, err := c.List(ctx, kata.ListOpts{Unlimited: true})
	require.NoError(t, err)
	var found bool
	for _, issue := range listed {
		found = found || issue.ShortID == created.ShortID
	}
	assert.True(t, found)

	_, err = c.Show(ctx, "e2etest#zzzz")
	require.ErrorIs(t, err, kata.ErrNotFound)
}

// TestEvaluateE2E checks the status states a real Kata can produce.
func TestEvaluateE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	k := katatest.StartReal(t, ctx)
	cfg := config.KataIntegrationConfig{Enabled: true, Endpoint: k.SocketEndpoint(t, ctx), Teammate: "roborev"}

	status := kata.EvaluateWith(ctx, k.Repo, cfg, "")
	assert.Equal(t, kata.StateReady, status.State, status.Message)
	assert.Equal(t, "e2etest", status.Project)

	cfg.DefaultProject = "not-a-project"
	unbound := t.TempDir()
	status = kata.EvaluateWith(ctx, unbound, cfg, "")
	assert.Equal(t, kata.StateProjectNotFound, status.State)

	cfg.Endpoint = "unix://" + unbound + "/missing.sock"
	assert.Equal(t, kata.StateUnreachable, kata.EvaluateWith(ctx, k.Repo, cfg, "").State)
}

// TestAPIClientListsCompleteProjectOverBoundedDefaultE2E reads a project
// larger than the CLI's default bounded result from a real Kata.
func TestAPIClientListsCompleteProjectOverBoundedDefaultE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	k := katatest.StartReal(t, ctx)
	cfg := config.KataIntegrationConfig{Enabled: true, Endpoint: k.SocketEndpoint(t, ctx), DefaultProject: "e2etest"}
	c, err := kata.NewAPIClient(k.Repo, cfg)
	require.NoError(t, err)
	defer c.CloseIdleConnections()

	const total = 230 // larger than Kata's default bounded result of 200
	want := map[string]bool{}
	for i := range total {
		res, err := c.Create(ctx, kata.CreateReq{Title: fmt.Sprintf("issue %03d", i), IdempotencyKey: fmt.Sprintf("e2e:page:%d", i)})
		require.NoError(t, err)
		want[res.ShortID] = true
	}
	issues, err := c.List(ctx, kata.ListOpts{Unlimited: true})
	require.NoError(t, err)
	got := map[string]bool{}
	for _, issue := range issues {
		got[issue.ShortID] = true
	}
	assert.Len(t, issues, total, "every issue exactly once")
	assert.Equal(t, want, got)

	first, err := c.List(ctx, kata.ListOpts{})
	require.NoError(t, err)
	assert.Len(t, first, 200, "a bounded read stops at the CLI's default row limit")
}
