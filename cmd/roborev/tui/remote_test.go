package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func TestRemoteAutoFilterUsesDaemonRoot(t *testing.T) {
	assert := assert.New(t)
	const daemonRoot = "/srv/checkouts/project"
	m := newModel(localhostEndpoint,
		withExternalIODisabled(),
		withAutoFilterRepo("/home/someone/project"),
		withRemote(),
		withRemoteRepoRoot(daemonRoot),
	)

	assert.Equal([]string{daemonRoot}, m.activeRepoFilter)
	assert.True(m.autoRepoFilter)
	assert.True(m.isJobVisible(makeJob(1, withRepoPath(daemonRoot))))
	assert.False(m.isJobVisible(makeJob(2, withRepoPath("/home/someone/project"))))
}

func TestRemoteAutoFilterUnresolvedStartsUnfiltered(t *testing.T) {
	m := newModel(localhostEndpoint,
		withExternalIODisabled(),
		withAutoFilterRepo("/home/someone/project"),
		withRemote(),
	)
	assert.Empty(t, m.activeRepoFilter)
}

func TestRemoteReconnectKeepsEndpoint(t *testing.T) {
	remote := daemon.DaemonEndpoint{Network: "tcp", Address: "daemon-host.example:7474"}
	m := newModel(remote, withExternalIODisabled(), withRemote())

	msg, ok := m.tryReconnect()().(reconnectMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, remote, msg.endpoint)
}
