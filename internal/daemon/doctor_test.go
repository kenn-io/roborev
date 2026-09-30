package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
)

func TestDoctorAgentsReportsDaemonView(t *testing.T) {
	binDir := t.TempDir()
	gooseBin := filepath.Join(binDir, "goose-acp")
	script := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		gooseBin += ".cmd"
		script = "@echo off\r\nexit /b 0\r\n"
	}
	require.NoError(t, os.WriteFile(gooseBin, []byte(script), 0o755))
	t.Setenv("PATH", binDir)

	server, _, _ := newTestServer(t)
	get := func(repo string) (int, DoctorAgentsOutput) {
		req := httptest.NewRequest(http.MethodGet, "/api/doctor/agents?repo="+url.QueryEscape(repo), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		var out DoctorAgentsOutput
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out.Body))
		return w.Code, out
	}

	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".roborev.toml"),
		[]byte("[acp.goose]\ncommand = \"goose-acp\"\n"), 0o600))
	code, out := get(repo)
	require.Equal(t, http.StatusOK, code)
	assert := assert.New(t)
	assert.Equal(binDir, out.Body.PathEnv)
	assert.Empty(out.Body.RepoConfigError)
	byName := map[string]agent.Diagnosis{}
	for _, d := range out.Body.Agents {
		byName[d.Name] = d
	}
	assert.True(byName["acp.goose"].Available, "repo ACP agent resolves on the daemon PATH")
	assert.False(byName["codex"].Available, "codex is not on the daemon PATH")

	broken := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(broken, ".roborev.toml"), []byte("agent = [\n"), 0o600))
	code, out = get(broken)
	require.Equal(t, http.StatusOK, code)
	assert.NotEmpty(out.Body.RepoConfigError)
}
