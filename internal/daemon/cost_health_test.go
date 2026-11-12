package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

// The executable writes a marker if invoked. Availability checks must only
// locate it.
func installCostSourceFixture(t *testing.T, dir string) string {
	t.Helper()
	marker := filepath.Join(dir, "executed")
	script := fmt.Sprintf("#!/bin/sh\nprintf executed > %q\nexit 1\n", marker)
	if runtime.GOOS == "windows" {
		script = fmt.Sprintf("@echo off\r\necho executed > \"%s\"\r\nexit /b 1\r\n", marker)
	}
	t.Cleanup(testutil.MockBinaryInPath(t, "agentsview", script))
	return marker
}

func TestCostSourceHealth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		healthy bool
	}{
		{name: "missing"},
		{name: "non-executable"},
		{name: "cli", healthy: true},
		{name: "agentsview", healthy: true},
		{name: "endpoint", healthy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			cfg := config.DefaultConfig()
			switch tc.name {
			case "non-executable":
				if runtime.GOOS == "windows" {
					t.Skip("Windows does not use Unix executable permission bits")
				}
				require.NoError(t, os.WriteFile(filepath.Join(dir, "agentsview"), nil, 0o600))
			case "cli":
				marker := installCostSourceFixture(t, dir)
				t.Cleanup(func() { assert.NoFileExists(marker) })
			case "agentsview", "endpoint":
				var requests atomic.Int32
				httpSource := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					requests.Add(1)
				}))
				t.Cleanup(func() {
					httpSource.Close()
					assert.Zero(requests.Load())
				})
				if tc.name == "agentsview" {
					cfg.AgentsView.URL = httpSource.URL
				} else {
					cfg.Cost.Endpoint = httpSource.URL + "/{session_id}"
				}
			}

			health := costSourceHealth(cfg)
			assert.Equal("cost", health.Name)
			assert.Equal(tc.healthy, health.Healthy)
			if tc.healthy {
				assert.Empty(health.Message)
			} else {
				assert.Contains(health.Message, "no usage source")
				assert.Contains(health.Message, "install agentsview")
				assert.Contains(health.Message, "agentsview.url or cost.endpoint")
			}
		})
	}
}

func TestHealthReportsMissingCostSourceAndConfigChanges(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	server := setupTestServer(t)
	// Keep agent availability independent of the deliberately empty PATH.
	server.agentHealth.run = func(_ context.Context, _ *config.Config) storage.ComponentHealth {
		return storage.ComponentHealth{Name: "agents", Healthy: true}
	}
	for _, provider := range []string{"none", "agentsview", "endpoint", "none"} {
		cfg := config.DefaultConfig()
		if provider == "agentsview" {
			cfg.AgentsView.URL = "https://usage.example.test"
		}
		if provider == "endpoint" {
			cfg.Cost.Endpoint = "https://usage.example.test/{session_id}"
		}
		server.configWatcher.cfgMu.Lock()
		server.configWatcher.cfg = cfg
		server.configWatcher.cfgMu.Unlock()
		response := executeHealthCheck(t.Context(), server, http.MethodGet)
		require.Equal(t, http.StatusOK, response.Code)
		health := decodeHealthStatus(t, response)
		assert.Equal(t, provider != "none", health.Healthy)
		assert.True(t, health.Ready, "missing accounting does not block review work")
		if provider == "none" {
			assertMissingCostComponent(t, health.Components)
		} else {
			assert.Contains(t, health.Components, storage.ComponentHealth{Name: "cost", Healthy: true})
		}
	}
}

func assertMissingCostComponent(t *testing.T, components []storage.ComponentHealth) {
	t.Helper()
	i := slices.IndexFunc(components, func(component storage.ComponentHealth) bool { return component.Name == "cost" })
	require.NotEqual(t, -1, i, "cost health must be present")
	assert.False(t, components[i].Healthy)
	assert.Contains(t, components[i].Message, "no usage source")
}

func TestHealthMissingCostSourcePreservesCIRecovery(t *testing.T) {
	_, server, _ := recoveringCIHealth(t)
	t.Setenv("PATH", t.TempDir())
	cfg := *server.configWatcher.Config()
	cfg.Cost.Endpoint = ""
	server.configWatcher.cfgMu.Lock()
	server.configWatcher.cfg = &cfg
	server.configWatcher.cfgMu.Unlock()
	server.agentHealth.run = func(_ context.Context, _ *config.Config) storage.ComponentHealth {
		return storage.ComponentHealth{Name: "agents", Healthy: true}
	}
	health, ci := readCIHealth(t, server)
	assertMissingCostComponent(t, health.Components)
	require.NotNil(t, ci.Recovery, "missing accounting must not hide review retry recovery")
}

func TestCostSourceStartupWarning(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		warnings int
	}{
		{name: "none", warnings: 1},
		{name: "endpoint", endpoint: "https://usage.example.test/{session_id}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testenv.SetDataDir(t)
			setShortRuntimeDir(t)
			t.Setenv("PATH", t.TempDir())
			cfg := config.DefaultConfig()
			cfg.DefaultAgent = "test"
			cfg.ServerAddr = "127.0.0.1:0"
			cfg.Cost.Endpoint = tc.endpoint
			var output bytes.Buffer
			original := log.Writer()
			log.SetOutput(&output)
			t.Cleanup(func() {
				log.SetOutput(original)
				assert.Equal(t, tc.warnings, strings.Count(output.String(), "Warning: no usage source:"))
			})
			server := newServerWithLogs(testutil.OpenTestDB(t), cfg, "", newTestErrorLog(), newTestActivityLog())
			errCh, _ := startServerAndWaitForRuntime(t, server)
			for range 2 {
				executeHealthCheck(t.Context(), server, http.MethodGet)
			}
			stopTestServer(t, server, errCh)
		})
	}
}
