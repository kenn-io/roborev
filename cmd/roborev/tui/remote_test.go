package tui

import (
	"errors"
	"testing"
	"time"

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

func TestRemoteReconnectPingsDaemon(t *testing.T) {
	remote := daemon.DaemonEndpoint{Network: "tcp", Address: "daemon-host.example:7474"}
	orig := probeRemoteDaemon
	t.Cleanup(func() { probeRemoteDaemon = orig })
	m := newModel(remote, withExternalIODisabled(), withRemote())

	var probed []daemon.DaemonEndpoint
	probeRemoteDaemon = func(ep daemon.DaemonEndpoint, _ time.Duration) (*daemon.PingInfo, error) {
		probed = append(probed, ep)
		return nil, errors.New("connection refused")
	}
	msg, ok := m.tryReconnect()().(reconnectMsg)
	require.True(t, ok)
	require.ErrorContains(t, msg.err, "connection refused", "a down remote daemon is not a reconnect")

	probeRemoteDaemon = func(ep daemon.DaemonEndpoint, _ time.Duration) (*daemon.PingInfo, error) {
		probed = append(probed, ep)
		return &daemon.PingInfo{OK: true, Service: "roborev", Version: "v9.9.9"}, nil
	}
	msg, ok = m.tryReconnect()().(reconnectMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, remote, msg.endpoint)
	assert.Equal(t, "v9.9.9", msg.version)
	assert.Equal(t, []daemon.DaemonEndpoint{remote, remote}, probed)
}
