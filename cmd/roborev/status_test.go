package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/version"
)

type statusJSONOutput struct {
	Running bool                 `json:"running"`
	WebURL  string               `json:"web_url,omitempty"`
	Daemon  storage.DaemonStatus `json:"daemon"`
	Jobs    []storage.ReviewJob  `json:"jobs,omitempty"`
	Error   string               `json:"error,omitempty"`
}

func TestStatusCmdShowsWebUIURL(t *testing.T) {
	md := NewMockDaemon(t, MockRefineHooks{})
	defer md.Close()

	withStatusWebRuntime(t, func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{WebOrigin: "https://reviews.example.com"}, nil
	})

	output := captureStdout(t, func() {
		require.NoError(t, statusCmd().Execute())
	})

	assert.Contains(t, output, "Web UI: https://reviews.example.com\n")
}

func TestStatusCmdShowsUnavailableWebUI(t *testing.T) {
	md := NewMockDaemon(t, MockRefineHooks{})
	defer md.Close()

	withStatusWebRuntime(t, func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{}, nil
	})

	output := captureStdout(t, func() {
		require.NoError(t, statusCmd().Execute())
	})

	assert.Contains(t, output, "Web UI: unavailable\n")
}

func TestStatusCmdJSONIncludesEmptyWebUIURLWhenUnavailable(t *testing.T) {
	md := NewMockDaemon(t, MockRefineHooks{})
	defer md.Close()

	withStatusWebRuntime(t, func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{}, nil
	})

	output := captureStdout(t, func() {
		cmd := statusCmd()
		cmd.SetArgs([]string{"--json"})
		require.NoError(t, cmd.Execute())
	})

	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(output), &parsed))
	assert.Contains(t, parsed, "web_url")
	assert.Empty(t, parsed["web_url"])
}

func TestStatusCmdJSONIncludesWebUIURL(t *testing.T) {
	md := NewMockDaemon(t, MockRefineHooks{})
	defer md.Close()

	withStatusWebRuntime(t, func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{WebOrigin: "https://reviews.example.com"}, nil
	})

	output := captureStdout(t, func() {
		cmd := statusCmd()
		cmd.SetArgs([]string{"--json"})
		require.NoError(t, cmd.Execute())
	})

	var parsed statusJSONOutput
	require.NoError(t, json.Unmarshal([]byte(output), &parsed))
	assert.Equal(t, "https://reviews.example.com", parsed.WebURL)
}

func TestDaemonStatusUsesSharedStatusOutput(t *testing.T) {
	md := NewMockDaemon(t, MockRefineHooks{})
	defer md.Close()

	withStatusWebRuntime(t, func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{WebOrigin: "https://reviews.example.com"}, nil
	})

	output := captureStdout(t, func() {
		cmd := daemonCmd()
		cmd.SetArgs([]string{"status"})
		require.NoError(t, cmd.Execute())
	})

	assert.Contains(t, output, "Daemon: running")
	assert.Contains(t, output, "Web UI: https://reviews.example.com\n")
}

func withStatusWebRuntime(t *testing.T, discover func() (*daemon.RuntimeInfo, error)) {
	t.Helper()
	original := statusDiscover
	statusDiscover = discover
	t.Cleanup(func() { statusDiscover = original })
}

func TestStatusCmdReportsAccessDeniedAsSandboxRestriction(t *testing.T) {
	origEnsure := statusEnsureDaemon
	statusEnsureDaemon = func() error { return daemon.ErrDaemonAccessDenied }
	t.Cleanup(func() { statusEnsureDaemon = origEnsure })

	output := captureStdout(t, func() {
		cmd := statusCmd()
		err := cmd.Execute()
		require.NoError(t, err)
	})

	assert.Contains(t, output, "Daemon: status unavailable")
	assert.Contains(t, output, "Web UI: unavailable")
	assert.Contains(t, output, "sandbox")
	assert.Contains(t, output, "loopback or Unix socket")
	assert.NotContains(t, output, "Daemon: not running")
	assert.NotContains(t, output, "Start with: roborev daemon start")
}

func TestStatusCmdJSONReportsAccessDeniedAsRunning(t *testing.T) {
	origEnsure := statusEnsureDaemon
	statusEnsureDaemon = func() error { return daemon.ErrDaemonAccessDenied }
	t.Cleanup(func() { statusEnsureDaemon = origEnsure })

	output := captureStdout(t, func() {
		cmd := statusCmd()
		cmd.SetArgs([]string{"--json"})
		err := cmd.Execute()
		require.NoError(t, err)
	})

	var parsed statusJSONOutput
	require.NoError(t, json.Unmarshal([]byte(output), &parsed))
	assert.True(t, parsed.Running)
	assert.Contains(t, parsed.Error, "sandbox")
	assert.Contains(t, parsed.Error, "loopback or Unix socket")
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

func TestStatusCmdReportsHTTPErrorAsUnavailable(t *testing.T) {
	md := NewMockDaemon(t, MockRefineHooks{
		OnStatus: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"detail": "snooze status unavailable",
			})
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
	assert.Contains(t, output, "500 Internal Server Error")
	assert.NotContains(t, output, "Jobs:")
}

func TestStatusCmdJSONReportsHTTPErrorAsUnavailable(t *testing.T) {
	md := NewMockDaemon(t, MockRefineHooks{
		OnStatus: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"detail": "snooze status unavailable",
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
	assert.Contains(t, parsed.Error, "500 Internal Server Error")
	assert.Empty(t, parsed.Jobs)
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

func TestStatusCmdJSONIncludesActiveSnoozes(t *testing.T) {
	until := time.Date(2026, 8, 10, 20, 30, 0, 0, time.UTC)
	snooze := storage.AgentHookSnooze{
		RepoName:     "roborev",
		RepoPath:     "/src/roborev",
		WorktreePath: "/worktrees/snooze-status",
		Branch:       "feature/snooze-status",
		SnoozedUntil: until,
	}
	md := NewMockDaemon(t, MockRefineHooks{
		OnStatus: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version":        version.Version,
				"active_snoozes": []storage.AgentHookSnooze{snooze},
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

	var parsed struct {
		Daemon struct {
			ActiveSnoozes []storage.AgentHookSnooze `json:"active_snoozes"`
		} `json:"daemon"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &parsed))
	require.Len(t, parsed.Daemon.ActiveSnoozes, 1)
	assert.Equal(t, snooze, parsed.Daemon.ActiveSnoozes[0])
}

func TestStatusCmdShowsActiveSnoozes(t *testing.T) {
	until := time.Date(2026, 8, 10, 20, 30, 0, 0, time.UTC)
	snooze := storage.AgentHookSnooze{
		RepoName:     "roborev",
		RepoPath:     "/src/roborev",
		WorktreePath: "/worktrees/snooze-status",
		Branch:       "feature/snooze-status",
		SnoozedUntil: until,
	}
	md := NewMockDaemon(t, MockRefineHooks{
		OnStatus: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			_ = json.NewEncoder(w).Encode(storage.DaemonStatus{
				Version:       version.Version,
				ActiveSnoozes: []storage.AgentHookSnooze{snooze},
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
		err := cmd.Execute()
		require.NoError(t, err)
	})

	assert.Contains(t, output, "Active Snoozes:")
	assert.Contains(t, output, "roborev")
	assert.Contains(t, output, "/worktrees/snooze-status")
	assert.Contains(t, output, "feature/snooze-status")
	assert.Contains(t, output, until.Local().Format("Jan 02 15:04 MST"))
}

func TestStatusCmdOmitsActiveSnoozesWhenEmpty(t *testing.T) {
	md := NewMockDaemon(t, MockRefineHooks{
		OnStatus: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			_ = json.NewEncoder(w).Encode(storage.DaemonStatus{
				Version:       version.Version,
				ActiveSnoozes: []storage.AgentHookSnooze{},
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
		err := cmd.Execute()
		require.NoError(t, err)
	})

	assert.NotContains(t, output, "Active Snoozes:")
}
