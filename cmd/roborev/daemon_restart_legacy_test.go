package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func TestDaemonRestartInterruptUpdateAPI(t *testing.T) {
	if os.Getenv("ROBOREV_TEST_LEGACY_DAEMON") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}

	stubDaemonRestartOutput(t)
	child := exec.CommandContext(
		t.Context(), os.Args[0], "-test.run=^TestDaemonRestartInterruptUpdateAPI$", "daemon", "run",
	)
	child.Env = append(os.Environ(), "ROBOREV_TEST_LEGACY_DAEMON=1")
	input, err := child.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	var stopOnce sync.Once
	stopChild := func() { stopOnce.Do(func() { _ = input.Close(); _ = child.Wait() }) }
	t.Cleanup(stopChild)

	var mu sync.Mutex
	var requests []string
	endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/ping":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "roborev", "version": "v0.75.0", "pid": child.Process.Pid})
		case "/api/update/prepare":
			var body struct {
				Policy string `json:"policy"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			requests = append(requests, "prepare:"+body.Policy)
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"interrupt","expires_at":"2030-01-01T00:00:00Z","running_jobs":1,"targeted_running_jobs":1,"active_workers":1,"recovering":false}`)
		case "/api/update/renew":
			requests = append(requests, "renew")
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"interrupt","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
		case "/api/shutdown/interrupt":
			requests = append(requests, "interrupt")
			http.NotFound(w, r)
		case "/api/shutdown":
			requests = append(requests, "shutdown")
			stopChild()
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	oldList := listAllRuntimes
	t.Cleanup(func() { listAllRuntimes = oldList })
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return []*daemon.RuntimeInfo{{PID: child.Process.Pid, Network: endpoint.Network, Address: endpoint.Address, SourcePath: t.TempDir() + "/runtime"}}, nil
	}
	started := false
	withDaemonCommandDependencies(t, func() error { started = true; return nil }, func() error { return nil }, func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist })
	cmd := daemonCmd()
	cmd.SetArgs([]string{"restart", "--running=interrupt"})
	err = cmd.ExecuteContext(t.Context())
	require.NoError(t, err)
	assert.True(t, started)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"interrupt", "prepare:interrupt", "renew", "shutdown"}, requests)
}

func TestInterruptDaemonWithUpdateAPIReleasesLeaseAfterRenewFailure(t *testing.T) {
	var requests []string
	endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/update/prepare":
			requests = append(requests, "prepare")
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"interrupt","expires_at":"2030-01-01T00:00:00Z","running_jobs":1,"targeted_running_jobs":1,"active_workers":1,"recovering":false}`)
		case "/api/update/renew":
			requests = append(requests, "renew")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"title":"Unavailable","status":503}`)
		case "/api/update/release":
			requests = append(requests, "release")
			_, _ = io.WriteString(w, `{"released":true}`)
		default:
			http.NotFound(w, r)
		}
	}))

	err := interruptDaemonWithUpdateAPI(t.Context(), endpoint)
	require.Error(t, err)
	assert.Equal(t, []string{"prepare", "renew", "release"}, requests)
}
