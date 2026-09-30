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
	get := func(repo string, agents ...string) (int, DoctorAgentsOutput) {
		query := url.Values{"repo": {repo}, "agent": agents}
		req := httptest.NewRequest(http.MethodGet, "/api/doctor/agents?"+query.Encode(), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		var out DoctorAgentsOutput
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out.Body))
		return w.Code, out
	}

	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".roborev.toml"),
		[]byte("[acp.goose]\ncommand = \"goose-acp\"\n\n[[hooks]]\nevent = \"review.failed\"\ntype = \"kata\"\n"), 0o600))
	code, out := get(repo, "acp.goose", "no-such-agent")
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

	require.Len(t, out.Body.Requested, 2)
	assert.True(out.Body.Requested[0].Available)
	assert.Equal("no-such-agent", out.Body.Requested[1].Name)
	assert.Contains(out.Body.Requested[1].Error, "unknown agent")

	require.Len(t, out.Body.HookTools, 1)
	assert.Equal("kata", out.Body.HookTools[0].Name)
	assert.False(out.Body.HookTools[0].Available, "kata is not on the daemon PATH")

	broken := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(broken, ".roborev.toml"), []byte("agent = [\n"), 0o600))
	code, out = get(broken)
	require.Equal(t, http.StatusOK, code)
	assert.NotEmpty(out.Body.RepoConfigError)
}
