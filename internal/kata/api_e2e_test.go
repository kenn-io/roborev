//go:build kata && !windows

package kata_test

import (
	"context"
	"os/exec"
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
// key, list, and show.
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
		Title: "roborev connection test", Body: "created through the API", Labels: []string{"roborev", "retry-check"},
		Priority: new(1), IdempotencyKey: "e2e:api:1",
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

	// Retry semantics belong to the real producer, including fields the old
	// hand-written fake omitted from its fingerprint.
	for _, change := range []struct {
		name  string
		apply func(*kata.CreateReq)
	}{
		{"title", func(r *kata.CreateReq) { r.Title = "different review" }},
		{"body", func(r *kata.CreateReq) { r.Body = "different body" }},
		{"priority", func(r *kata.CreateReq) { r.Priority = new(2) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := req
			change.apply(&changed)
			_, err := c.Create(ctx, changed)
			require.ErrorIs(t, err, kata.ErrConflict)
			assert.Equal(t, "idempotency_mismatch", kata.ErrorCode(err))
		})
	}

	// Register a second project in the same disposable daemon without changing
	// this client's workspace binding. Keys must be scoped to their project.
	initOther := exec.CommandContext(ctx, "kata", "init", "--project", "e2eother")
	initOther.Env, initOther.Dir = k.Env, t.TempDir()
	out, err := initOther.CombinedOutput()
	require.NoError(t, err, "register second disposable project: %s", out)
	other := req
	other.Project = "e2eother"
	elsewhere, err := c.Create(ctx, other)
	require.NoError(t, err)
	assert.False(t, elsewhere.Reused, "the same key in another project creates there")
	otherIssues, err := c.Show(ctx, "e2eother#"+elsewhere.ShortID)
	require.NoError(t, err)
	assert.Equal(t, "e2eother#"+elsewhere.ShortID, otherIssues.QualifiedID)

	shown, err := c.Show(ctx, created.ShortID)
	require.NoError(t, err)
	assert.Equal(t, "roborev connection test", shown.Title)
	assert.Equal(t, "e2etest#"+created.ShortID, shown.QualifiedID)
	assert.ElementsMatch(t, []string{"roborev", "retry-check"}, shown.Labels)

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
