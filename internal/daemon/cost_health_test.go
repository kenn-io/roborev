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
// locate it, even when a configured HTTP provider takes precedence.
func installCostSourceFixture(t *testing.T, dir string) string {
	t.Helper()
	marker := filepath.Join(dir, "executed")
	name := "agentsview"
	script := fmt.Sprintf("#!/bin/sh\nprintf executed > %q\nexit 1\n", marker)
	if runtime.GOOS == "windows" {
		name += ".cmd"
		script = fmt.Sprintf("@echo off\r\necho executed > \"%s\"\r\nexit /b 1\r\n", marker)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755))
	return marker
}

func TestCostSourceHealth(t *testing.T) {
	for _, cli := range []bool{false, true} {
		for _, provider := range []string{"none", "agentsview", "endpoint", "both"} {
			t.Run(fmt.Sprintf("cli=%t/provider=%s", cli, provider), func(t *testing.T) {
				assert := assert.New(t)
				dir := t.TempDir()
				t.Setenv("PATH", dir)
				marker := filepath.Join(dir, "executed")
				if cli {
					marker = installCostSourceFixture(t, dir)
				}
				var requests atomic.Int32
				httpSource := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					requests.Add(1)
				}))
				defer httpSource.Close()
				cfg := config.DefaultConfig()
				if provider == "agentsview" || provider == "both" {
					cfg.AgentsView.URL = httpSource.URL
					cfg.AgentsView.TokenFile = filepath.Join(dir, "missing-token")
				}
				if provider == "endpoint" || provider == "both" {
					cfg.Cost.Endpoint = httpSource.URL + "/{session_id}"
				}
				health := costSourceHealth(cfg)
				assert.Equal("cost", health.Name)
				assert.Equal(cli || provider != "none", health.Healthy)
				if health.Healthy {
					assert.Empty(health.Message)
				} else {
					assert.Contains(health.Message, "no usage source")
					assert.Contains(health.Message, "install agentsview")
					assert.Contains(health.Message, "agentsview.url or cost.endpoint")
				}
				assert.NoFileExists(marker)
				assert.Zero(requests.Load())
			})
		}
	}
}

func TestCostSourceHealthRequiresExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not use Unix executable permission bits")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agentsview"), nil, 0o600))
	assert.False(t, costSourceHealth(config.DefaultConfig()).Healthy)
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
		assert.Equal(t, provider != "none", health.Ready)
		assert.Contains(t, health.Components, costSourceHealth(cfg))
		assert.Empty(t, health.RecentErrors, "health requests must not log repeated warnings")
	}
}

func TestCostSourceStartupWarning(t *testing.T) {
	for _, provider := range []string{"none", "cli", "agentsview", "endpoint"} {
		t.Run(provider, func(t *testing.T) {
			testenv.SetDataDir(t)
			setShortRuntimeDir(t)
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			marker := filepath.Join(dir, "executed")
			if provider == "cli" {
				marker = installCostSourceFixture(t, dir)
			}
			cfg := config.DefaultConfig()
			cfg.DefaultAgent = "test"
			cfg.ServerAddr = "127.0.0.1:0"
			if provider == "agentsview" {
				cfg.AgentsView.URL = "https://usage.example.test"
			}
			if provider == "endpoint" {
				cfg.Cost.Endpoint = "https://usage.example.test/{session_id}"
			}
			var output bytes.Buffer
			original := log.Writer()
			log.SetOutput(&output)
			t.Cleanup(func() { log.SetOutput(original) })
			server := newServerWithLogs(testutil.OpenTestDB(t), cfg, "", newTestErrorLog(), newTestActivityLog())
			errCh, _ := startServerAndWaitForRuntime(t, server)
			for range 2 {
				executeHealthCheck(t.Context(), server, http.MethodGet)
			}
			stopTestServer(t, server, errCh)
			log.SetOutput(original)
			want := 0
			if provider == "none" {
				want = 1
				assert.Contains(t, output.String(), costSourceHealth(cfg).Message)
			}
			assert.Equal(t, want, strings.Count(output.String(), "Warning: no usage source:"))
			var costErrors []ErrorEntry
			for _, entry := range server.errorLog.RecentN(100) {
				if entry.Component == "cost" {
					costErrors = append(costErrors, entry)
				}
			}
			assert.Len(t, costErrors, want)
			assert.NoFileExists(t, marker)
		})
	}
}
