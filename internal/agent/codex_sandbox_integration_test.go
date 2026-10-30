//go:build integration

package agent

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/sandbox"
	"go.kenn.io/roborev/internal/testutil"
)

func TestCodexSandboxIntegration(t *testing.T) {
	testutil.RequireSandboxIntegration(t)
	codex, err := exec.LookPath("codex")
	require.NoError(t, err)
	codex, err = filepath.EvalSymlinks(codex)
	require.NoError(t, err)

	work, secrets := t.TempDir(), t.TempDir()
	repo, gitDir := filepath.Join(work, "repo"), filepath.Join(t.TempDir(), "repo.git")
	testutil.InitSeparateGitRepo(t, repo, gitDir)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "trust-policy.json"), []byte("invalid repo policy"), 0o600))
	t.Setenv("CODEX_HOME", secrets)
	require.NoError(t, os.WriteFile(filepath.Join(secrets, "auth.json"), []byte(`{"tokens":{"access_token":"sandbox-test-token"}}`), 0o600))

	probe := fmt.Sprintf("#!/bin/bash\n[[ \"$CODEX_HOME\" == %q/.codex-home-* && -d \"$CODEX_HOME\" ]] && echo OK codex-home\n", work)

	const key = "sandbox-test-token"
	var unauthorized atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			unauthorized.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
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
		for _, tool := range request.Tools {
			assert.NotEqual(t, "web_search", tool.Type)
		}
		item := map[string]any{"type": "function_call", "id": "tool_test", "call_id": "tool_test", "name": "exec_command", "arguments": `{"cmd":"bash ../probe.sh","yield_time_ms":10000}`}
		if len(outputs) > 0 {
			final, err := json.Marshal(map[string]string{"probe": strings.Join(outputs, "\n")})
			assert.NoError(t, err)
			item = map[string]any{"type": "message", "role": "assistant", "id": "msg_test", "content": []any{map[string]any{"type": "output_text", "text": string(final)}}}
		}
		send("response.output_item.done", map[string]any{"type": "response.output_item.done", "item": item})
		send("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_test", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
	}))
	defer server.Close()
	require.NoError(t, os.WriteFile(filepath.Join(work, "probe.sh"), []byte(probe), 0o700))

	job := sandbox.Job{
		WorkDir: work, ReadPaths: []string{gitDir, filepath.Dir(filepath.Dir(codex))},
		MemoryMax: 8 << 30,
		Env:       []string{"PATH=/usr/bin:/bin", "HOME=" + secrets, "TMPDIR=" + work},
	}
	if _, err := os.Stat("/etc/codex"); err == nil {
		job.ReadPaths = append(job.ReadPaths, "/etc/codex")
	} else {
		require.True(t, os.IsNotExist(err), "%v", err)
	}
	a := NewCodexAgent(codex).WithSandbox(&CodexSandbox{Job: job, Upstream: server.URL})
	a.IgnoreUserConfig = true
	a.ConfigOverrides = []string{`web_search="live"`, `model_provider="wrong-provider"`, `model_providers.roborev-route.base_url="http://127.0.0.1:1"`}
	a.Model = "test-model"
	var stream bytes.Buffer
	result, err := a.ReviewWithSchema(t.Context(), repo, "HEAD", "Run the supplied probe.", jsontext.Value(`{"type":"object","properties":{"probe":{"type":"string"}},"required":["probe"],"additionalProperties":false}`), &stream)
	require.NoError(t, err, "%s", stream.String())
	assert.Contains(t, string(result), "OK codex-home")
	assert.Zero(t, unauthorized.Load())
	homes, err := filepath.Glob(filepath.Join(work, ".codex-home-*"))
	require.NoError(t, err)
	assert.Empty(t, homes)
}
