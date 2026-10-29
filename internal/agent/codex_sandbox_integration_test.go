//go:build integration

package agent

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/sandbox"
)

func TestCodexSandboxIntegration(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
	}
	for _, command := range []string{"nono", "codex"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("%s is required", command)
		}
	}
	codex, err := exec.LookPath("codex")
	require.NoError(t, err)
	codex, err = filepath.EvalSymlinks(codex)
	require.NoError(t, err)

	work, secrets := t.TempDir(), t.TempDir()
	t.Chdir(filepath.Dir(work))
	repo, gitDir := filepath.Join(work, "repo"), filepath.Join(t.TempDir(), "repo.git")
	require.NoError(t, os.Mkdir(repo, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "trust-policy.json"), []byte("invalid repo policy"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("sandbox fixture\n"), 0o600))
	for _, args := range [][]string{
		{"init", "--separate-git-dir=" + gitDir, repo},
		{"-C", repo, "add", "README.md"},
		{"-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "commit", "-m", "fixture"},
	} {
		out, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	for _, name := range []string{"auth.json", "home-secret"} {
		require.NoError(t, os.WriteFile(filepath.Join(secrets, name), []byte("private fixture"), 0o600))
	}
	t.Setenv("CODEX_HOME", secrets)

	marker := filepath.Join(work, "probe-ran")
	probe := fmt.Sprintf(`#!/bin/bash
failed=0
check() {
  name=$1 expected=$2; shift 2
  "$@" >/dev/null 2>&1; actual=$?
  if { [ "$expected" = allow ] && [ "$actual" -ne 0 ]; } || { [ "$expected" = deny ] && [ "$actual" -eq 0 ]; }; then
    echo "BAD $name"; failed=1
  else
    echo "OK $name"
  fi
}
check codex-home allow bash -c '[[ "$CODEX_HOME" == "$1"/.codex-home-* && -d "$CODEX_HOME" ]]' probe '%s'
check login-read deny cat '%s/auth.json'
check home-read deny cat "$HOME/home-secret"
touch '%s'
exit "$failed"
`, work, secrets, marker)

	const key = "sandbox-test-key"
	var calls, unauthorized atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+key {
			unauthorized.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var request struct {
			Input []struct {
				Type   string `json:"type"`
				Output string `json:"output"`
			} `json:"input"`
			Tools []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(event string, data any) {
			encoded, err := json.Marshal(data)
			assert.NoError(t, err) //nolint:testifylint // Runs in the HTTP server goroutine.
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
			assert.NoError(t, err)
		}
		send("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_test"}})
		var outputs []string
		for _, input := range request.Input {
			if strings.HasSuffix(input.Type, "_call_output") {
				outputs = append(outputs, input.Output)
			}
		}
		functionTools := false
		for _, tool := range request.Tools {
			assert.NotEqual(t, "web_search", tool.Type)
			if tool.Name == "exec_command" {
				functionTools = true
			}
		}
		item := sandboxProxyItem(t, outputs, functionTools)
		send("response.output_item.done", map[string]any{"type": "response.output_item.done", "item": item})
		send("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_test", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, os.WriteFile(filepath.Join(work, "probe.sh"), []byte(probe), 0o700))

	job := sandbox.Job{
		WorkDir: filepath.Base(work), ReadPaths: []string{gitDir, filepath.Dir(filepath.Dir(codex))},
		ProxyPort: port, MemoryMax: 8 << 30,
		Env: []string{"PATH=/usr/bin:/bin", "HOME=" + secrets, "TMPDIR=" + work},
	}
	if _, err := os.Stat("/etc/codex"); err == nil {
		job.ReadPaths = append(job.ReadPaths, "/etc/codex")
	} else {
		require.True(t, os.IsNotExist(err), "%v", err)
	}
	a := NewCodexAgent(codex).WithSandbox(&CodexSandbox{Job: job, ProxyKey: key})
	a.IgnoreUserConfig = true
	a.ConfigOverrides = []string{`web_search="live"`, `model_provider="wrong-provider"`}
	a.Model = "test-model"
	var stream bytes.Buffer
	result, err := a.ReviewWithSchema(context.Background(), filepath.Join(filepath.Base(work), "repo"), "HEAD", "Run the supplied probe.", jsontext.Value(`{"type":"object","properties":{"probe":{"type":"string"}},"required":["probe"],"additionalProperties":false}`), &stream)
	require.NoError(t, err, "%s", stream.String())
	assert.NotContains(t, string(result), "BAD ")
	for _, name := range []string{"codex-home", "login-read", "home-read"} {
		assert.Contains(t, string(result), "OK "+name)
	}
	assert.FileExists(t, marker)
	assert.GreaterOrEqual(t, calls.Load(), int32(2))
	assert.Zero(t, unauthorized.Load())
	homes, err := filepath.Glob(filepath.Join(work, ".codex-home-*"))
	require.NoError(t, err)
	assert.Empty(t, homes)
}

func sandboxProxyItem(t *testing.T, outputs []string, functionTools bool) map[string]any {
	t.Helper()
	name := "exec_command"
	args := map[string]any{"cmd": "bash ../probe.sh", "yield_time_ms": 1000}
	if len(outputs) > 0 {
		session := regexp.MustCompile(`Process running with session ID ([0-9]+)`).FindStringSubmatch(outputs[len(outputs)-1])
		if session == nil {
			final, err := json.Marshal(map[string]string{"probe": strings.Join(outputs, "\n")})
			assert.NoError(t, err)
			return map[string]any{"type": "message", "role": "assistant", "id": "msg_test", "content": []any{map[string]any{"type": "output_text", "text": string(final)}}}
		}
		name = "write_stdin"
		args = map[string]any{"session_id": jsontext.Value(session[1]), "chars": "", "yield_time_ms": 1000}
	}
	encoded, err := json.Marshal(args)
	assert.NoError(t, err)
	id := fmt.Sprintf("tool_%d", len(outputs))
	if functionTools {
		return map[string]any{"type": "function_call", "id": id, "call_id": id, "name": name, "arguments": string(encoded)}
	}
	return map[string]any{"type": "custom_tool_call", "id": id, "call_id": id, "name": "exec", "input": "text(await tools." + name + "(" + string(encoded) + "));"}
}
