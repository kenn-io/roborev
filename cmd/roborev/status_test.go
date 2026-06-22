package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/version"
)

type statusJSONOutput struct {
	Running bool                 `json:"running"`
	Daemon  storage.DaemonStatus `json:"daemon"`
	Jobs    []storage.ReviewJob  `json:"jobs,omitempty"`
}

func TestStatusCmdDoesNotReportNotRunningWhenStatusRequestTimesOut(t *testing.T) {
	md := NewMockDaemon(t, MockRefineHooks{
		OnStatus: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			time.Sleep(3 * time.Second)
			return true
		},
	})
	defer md.Close()

	output := captureStdout(t, func() {
		cmd := statusCmd()
		err := cmd.Execute()
		require.NoError(t, err)
	})

	assert.Contains(t, output, "Daemon: running")
	assert.Contains(t, output, "Status: unavailable")
	assert.NotContains(t, output, "Daemon: not running")
}

func TestStatusCmdJSONIncludesDaemonEndpoint(t *testing.T) {
	md := NewMockDaemon(t, MockRefineHooks{
		OnStatus: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			_ = json.NewEncoder(w).Encode(storage.DaemonStatus{
				Version: version.Version,
				Network: "tcp",
				Address: "127.0.0.1:7373",
				Port:    7373,
			})
			return true
		},
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/health" {
				return false
			}
			_ = json.NewEncoder(w).Encode(storage.HealthStatus{
				Healthy: true,
				Version: version.Version,
			})
			return true
		},
	})
	defer md.Close()

	output := captureStdout(t, func() {
		cmd := statusCmd()
		cmd.SetArgs([]string{"--json"})
		err := cmd.Execute()
		require.NoError(t, err)
	})

	var parsed statusJSONOutput
	require.NoError(t, json.Unmarshal([]byte(output), &parsed))
	assert.True(t, parsed.Running)
	assert.Equal(t, version.Version, parsed.Daemon.Version)
	assert.Equal(t, "tcp", parsed.Daemon.Network)
	assert.Equal(t, "127.0.0.1:7373", parsed.Daemon.Address)
	assert.Equal(t, 7373, parsed.Daemon.Port)
}
