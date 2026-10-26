//go:build kata

package main

import (
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
)

// TestKataThroughTheDaemonE2E runs the CLI's Kata client against a real daemon
// server connected to a real Kata in a disposable home. The CLI's own config
// has no Kata settings: the daemon alone knows the socket.
func TestKataThroughTheDaemonE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the real Kata daemon E2E requires private Unix socket support")
	}

	ctx := t.Context()
	k := katatest.StartReal(t, ctx)
	ep := kataDaemon(t, config.KataIntegrationConfig{
		Enabled: true, Endpoint: k.SocketEndpoint(t, ctx), DefaultProject: "e2etest", Teammate: "roborev",
	})
	var probes atomic.Int32
	c := newKataClientAt(k.Repo, config.DefaultConfig(), fixedEndpoint(ep, &probes))

	b, err := c.Binding(ctx)
	require.NoError(t, err)
	assert.Equal(t, "e2etest", b.Project, "the workspace binding written by kata init")

	req := kata.CreateReq{Title: "created through the daemon", Body: "details", Labels: []string{"roborev"}, IdempotencyKey: "e2e:daemon:1"}
	created, err := c.Create(ctx, req)
	require.NoError(t, err)
	require.NotEmpty(t, created.ShortID)
	assert.False(t, created.Reused)
	assert.Equal(t, "roborev", created.Attribution.Author)
	again, err := c.Create(ctx, req)
	require.NoError(t, err)
	assert.True(t, again.Reused)

	shown, err := c.Show(ctx, created.ShortID)
	require.NoError(t, err)
	assert.Equal(t, "created through the daemon", shown.Title)
	assert.Equal(t, "e2etest#"+created.ShortID, shown.QualifiedID)

	listed, err := c.List(ctx, kata.ListOpts{Unlimited: true})
	require.NoError(t, err)
	var found bool
	for _, issue := range listed {
		found = found || issue.ShortID == created.ShortID
	}
	assert.True(t, found)

	status, err := newDoctorDaemon(ep).KataStatus(ctx, "e2etest")
	require.NoError(t, err)
	assert.Equal(t, "ready", status.State, status.Message)
	assert.Equal(t, "e2etest", status.Project)
	assert.True(t, status.SupportsReplies)
	assert.EqualValues(t, 1, probes.Load())
}
