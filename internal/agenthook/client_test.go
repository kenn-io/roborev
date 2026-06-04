package agenthook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/version"
)

func TestWriteRuntimeIdentifiesAgentHookService(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())

	path, err := WriteRuntime(kitdaemon.Endpoint{Network: "tcp", Address: "127.0.0.1:0"})

	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var info kitdaemon.RuntimeRecord
	require.NoError(t, json.Unmarshal(data, &info))
	assert.Equal(t, ServiceName, info.Service)
	assert.Equal(t, "tcp", info.Network)
	assert.Equal(t, "127.0.0.1:0", info.Address)
	assert.Equal(t, version.Version, info.Version)
}

func TestAgentHookDaemonManagerRejectsOtherServices(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	pid := os.Getpid()
	addr := startPingServer(t, "roborev", version.Version, pid)
	_, err := runtimeStore().Write(kitdaemon.RuntimeRecord{
		PID:     pid,
		Network: "tcp",
		Address: addr,
		Service: "roborev",
		Version: version.Version,
	})
	require.NoError(t, err)

	_, _, ok, err := agentHookDaemonManager().Find(context.Background())

	require.NoError(t, err)
	assert.False(t, ok)
}

func TestAgentHookDaemonManagerRejectsIncompatibleVersion(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	pid := os.Getpid()
	addr := startPingServer(t, ServiceName, "v0.0.0-stale", pid)
	_, err := runtimeStore().Write(kitdaemon.RuntimeRecord{
		PID:     pid,
		Network: "tcp",
		Address: addr,
		Service: ServiceName,
		Version: "v0.0.0-stale",
	})
	require.NoError(t, err)

	_, _, ok, err := agentHookDaemonManager().Find(context.Background())

	require.NoError(t, err)
	assert.False(t, ok, "a daemon reporting a different version must be treated as incompatible")
}

func TestAgentHookDaemonManagerAcceptsMatchingDaemon(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	pid := os.Getpid()
	addr := startPingServer(t, ServiceName, version.Version, pid)
	_, err := runtimeStore().Write(kitdaemon.RuntimeRecord{
		PID:     pid,
		Network: "tcp",
		Address: addr,
		Service: ServiceName,
		Version: version.Version,
	})
	require.NoError(t, err)

	rec, info, ok, err := agentHookDaemonManager().Find(context.Background())

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, pid, rec.PID)
	assert.Equal(t, version.Version, info.Version)
}

// startPingServer launches a daemon ping endpoint that reports the given
// service, version, and PID, returning its host:port address.
func startPingServer(t *testing.T, service, ver string, pid int) string {
	t.Helper()
	body := fmt.Sprintf(`{"ok":true,"service":%q,"version":%q,"pid":%d}`, service, ver, pid)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

func TestProbeDaemonChecksServiceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{name: "matching", body: `{"ok":true,"service":"roborev-agent-hook","version":"` + version.Version + `"}`, want: true},
		{name: "wrong service", body: `{"ok":true,"service":"roborev","version":"` + version.Version + `"}`, want: false},
		{name: "same service wrong version", body: `{"ok":true,"service":"roborev-agent-hook","version":"old"}`, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			addr := strings.TrimPrefix(server.URL, "http://")

			got := ProbeDaemon(kitdaemon.Endpoint{Network: "tcp", Address: addr})

			assert.Equal(t, tc.want, got)
		})
	}
}
