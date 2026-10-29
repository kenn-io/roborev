//go:build integration

package agent

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	configDir := filepath.Join(work, "config")
	profiles := filepath.Join(configDir, "nono", "profiles")
	require.NoError(t, os.MkdirAll(profiles, 0o700))
	planted, err := json.Marshal(map[string]any{"filesystem": map[string]any{"read_file": []string{filepath.Join(secrets, "home-secret")}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(profiles, "default.json"), planted, 0o600))
	repo, gitDir := filepath.Join(work, "repo"), filepath.Join(t.TempDir(), "repo.git")
	require.NoError(t, os.Mkdir(repo, 0o700))
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
	t.Setenv("ROBOREV_SANDBOX_TOKEN", "must-be-absent")

	other, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer other.Close()
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
check scratch-copy allow bash -c 'cp -a . ../scratch && echo changed >> ../scratch/README.md'
check home-write deny touch "$HOME/write"
check shared-tmp-write deny bash -c 'p=/tmp/roborev-probe-$$; touch "$p" && rm "$p"'
check git-read allow git status --porcelain
check git-commit deny bash -c 'echo changed >> README.md && git add README.md && git -c user.name=Test -c user.email=test@example.com commit -m probe'
check git-config deny git config --local sandbox.probe changed
check login-read deny cat '%s/auth.json'
check home-read deny cat "$HOME/home-secret"
check env-filter allow bash -c '[[ ! -v ROBOREV_SANDBOX_TOKEN ]]'
check dbus-connect deny /usr/bin/python3 -c 'import socket; s=socket.socket(socket.AF_UNIX); s.connect("%s/bus")'
check dbus-systemd deny env XDG_RUNTIME_DIR='%s' DBUS_SESSION_BUS_ADDRESS='unix:path=%s/bus' systemd-run --user --wait -q /usr/bin/true
check external-domain deny curl -fsS https://example.com
check external-ip deny bash -c ': >/dev/tcp/1.1.1.1/443'
check other-loopback deny bash -c ': >/dev/tcp/127.0.0.1/%d'
check proxy-port allow bash -c ': >/dev/tcp/127.0.0.1/PROXY_PORT'
touch '%s'
exit "$failed"
`, secrets, os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("XDG_RUNTIME_DIR"), other.Addr().(*net.TCPAddr).Port, marker)

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
		var item map[string]any
		for _, input := range request.Input {
			if strings.HasSuffix(input.Type, "_call_output") {
				final, err := json.Marshal(map[string]string{"probe": input.Output})
				assert.NoError(t, err)
				item = map[string]any{"type": "message", "role": "assistant", "id": "msg_test", "content": []any{map[string]any{"type": "output_text", "text": string(final)}}}
			}
		}
		if item == nil {
			args, err := json.Marshal(map[string]any{"cmd": "bash ../probe.sh", "yield_time_ms": 1000})
			assert.NoError(t, err)
			item = map[string]any{"type": "custom_tool_call", "id": "tool_test", "call_id": "call_test", "name": "exec", "input": "text(await tools.exec_command(" + string(args) + "));"}
			for _, tool := range request.Tools {
				if tool.Name == "exec_command" {
					item = map[string]any{"type": "function_call", "id": "tool_test", "call_id": "call_test", "name": "exec_command", "arguments": string(args)}
				}
			}
		}
		send("response.output_item.done", map[string]any{"type": "response.output_item.done", "item": item})
		send("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_test", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	probe = strings.ReplaceAll(probe, "PROXY_PORT", fmt.Sprint(port))
	require.NoError(t, os.WriteFile(filepath.Join(work, "probe.sh"), []byte(probe), 0o700))

	job := sandbox.Job{
		WorkDir: work, ReadPaths: []string{gitDir, filepath.Dir(filepath.Dir(codex)), "/etc/codex"},
		ProxyPort: port, MemoryMax: 8 << 30,
		Env: []string{"PATH=/usr/bin:/bin", "HOME=" + secrets, "TMPDIR=" + work, "XDG_CONFIG_HOME=" + configDir},
	}
	a := NewCodexAgent(codex).WithSandbox(&CodexSandbox{Job: job, ProxyKey: key})
	a.IgnoreUserConfig = true
	a.ConfigOverrides = []string{`web_search="live"`, `model_provider="wrong-provider"`}
	a.Model = "test-model"
	var stream bytes.Buffer
	result, err := a.ReviewWithSchema(context.Background(), repo, "HEAD", "Run the supplied probe.", jsontext.Value(`{"type":"object","properties":{"probe":{"type":"string"}},"required":["probe"],"additionalProperties":false}`), &stream)
	require.NoError(t, err, "%s", stream.String())
	assert.NotContains(t, string(result), "BAD ")
	for _, name := range []string{"scratch-copy", "home-write", "shared-tmp-write", "git-read", "git-commit", "git-config", "login-read", "home-read", "env-filter", "dbus-connect", "dbus-systemd", "external-domain", "external-ip", "other-loopback", "proxy-port"} {
		assert.Contains(t, string(result), "OK "+name)
	}
	assert.FileExists(t, marker)
	assert.GreaterOrEqual(t, calls.Load(), int32(2))
	assert.Zero(t, unauthorized.Load())
	homes, err := filepath.Glob(filepath.Join(work, ".codex-home-*"))
	require.NoError(t, err)
	assert.Empty(t, homes)

	require.NoError(t, os.Remove(marker))
	calls.Store(0)
	job.ProxyPort = 0
	_, err = a.WithSandbox(&CodexSandbox{Job: job, ProxyKey: key}).Review(t.Context(), repo, "HEAD", "Run the supplied probe.", io.Discard)
	require.ErrorContains(t, err, "open_port")
	assert.Zero(t, calls.Load())
	assert.NoFileExists(t, marker)
}
