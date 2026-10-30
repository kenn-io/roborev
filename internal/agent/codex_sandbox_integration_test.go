//go:build integration

package agent

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
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
	"go.kenn.io/roborev/internal/testutil"
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
	for _, commandType := range []string{"relative", "symlink"} {
		t.Run(commandType, func(t *testing.T) {
			codex, err := exec.LookPath("codex")
			require.NoError(t, err)
			codex, err = filepath.EvalSymlinks(codex)
			require.NoError(t, err)

			work, secrets := t.TempDir(), t.TempDir()
			t.Chdir(filepath.Dir(work))
			repo, gitDir := filepath.Join(work, "repo"), filepath.Join(t.TempDir(), "repo.git")
			testutil.InitSeparateGitRepo(t, repo, gitDir)
			require.NoError(t, os.WriteFile(filepath.Join(repo, "trust-policy.json"), []byte("invalid repo policy"), 0o600))
			t.Setenv("CODEX_HOME", secrets)
			require.NoError(t, os.WriteFile(filepath.Join(secrets, "auth.json"), []byte(`{"tokens":{"access_token":"sandbox-test-token"}}`), 0o600))

			marker := filepath.Join(work, "probe-ran")
			probe := fmt.Sprintf("#!/bin/bash\n"+testutil.SandboxShellChecks+`
check codex-home allow bash -c '[[ "$CODEX_HOME" == "$1"/.codex-home-* && -d "$CODEX_HOME" ]]' probe '%s'
touch '%s'
exit "$failed"
`, work, marker)

			const key = "sandbox-test-token"
			var calls, unauthorized atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
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
				item := sandboxRouteItem(t, outputs)
				send("response.output_item.done", map[string]any{"type": "response.output_item.done", "item": item})
				send("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_test", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
			}))
			defer server.Close()
			require.NoError(t, os.WriteFile(filepath.Join(work, "probe.sh"), []byte(probe), 0o700))

			job := sandbox.Job{
				WorkDir: filepath.Base(work), ReadPaths: []string{gitDir, filepath.Dir(filepath.Dir(codex))},
				MemoryMax: 8 << 30,
				Env:       []string{"PATH=/usr/bin:/bin", "HOME=" + secrets, "TMPDIR=" + work},
			}
			if _, err := os.Stat("/etc/codex"); err == nil {
				job.ReadPaths = append(job.ReadPaths, "/etc/codex")
			} else {
				require.True(t, os.IsNotExist(err), "%v", err)
			}
			var command string
			require.NoError(t, os.Mkdir(filepath.Join(work, "tools"), 0o700))
			if commandType == "relative" {
				data, err := os.ReadFile(codex)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(work, "tools", "codex"), data, 0o700))
				command = "./" + filepath.Join(filepath.Base(work), "tools", "codex")
			} else {
				launcher := filepath.Join(work, "tools", "launcher")
				script := fmt.Sprintf("#!/bin/sh\n[ \"${0##*/}\" = codex ] || { echo 'launcher requires basename codex' >&2; exit 1; }\nexec %q \"$@\"\n", codex)
				require.NoError(t, os.WriteFile(launcher, []byte(script), 0o700))
				require.NoError(t, os.Symlink(launcher, filepath.Join(work, "tools", "codex")))
				t.Setenv("PATH", filepath.Join(work, "tools")+string(os.PathListSeparator)+os.Getenv("PATH"))
				command = "codex"
			}
			a := NewCodexAgent(command).WithSandbox(&CodexSandbox{Job: job, upstream: server.URL})
			a.IgnoreUserConfig = true
			a.ConfigOverrides = []string{`web_search="live"`, `model_provider="wrong-provider"`}
			a.Model = "test-model"
			var stream bytes.Buffer
			result, err := a.Review(context.Background(), filepath.Join(filepath.Base(work), "repo"), "HEAD", "Run the supplied probe.", &stream)
			require.NoError(t, err, "%s", stream.String())
			assert.NotContains(t, string(result), "BAD ")
			assert.Contains(t, string(result), "OK codex-home")
			assert.FileExists(t, marker)
			assert.GreaterOrEqual(t, calls.Load(), int32(2))
			assert.Zero(t, unauthorized.Load())
			homes, err := filepath.Glob(filepath.Join(work, ".codex-home-*"))
			require.NoError(t, err)
			assert.Empty(t, homes)
		})
	}
}

func TestReviewSandboxStructuredOutput(t *testing.T) {
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
	repo, gitDir := filepath.Join(work, "repo"), filepath.Join(t.TempDir(), "repo.git")
	testutil.InitSeparateGitRepo(t, repo, gitDir)
	t.Setenv("CODEX_HOME", secrets)
	require.NoError(t, os.WriteFile(filepath.Join(secrets, "auth.json"), []byte(`{"tokens":{"access_token":"synthetic-token"}}`), 0o600))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n")
		fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg_test\",\"content\":[{\"type\":\"output_text\",\"text\":\"{\\\"ok\\\":true}\"}]}}\n\n")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	defer server.Close()
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
	a := NewCodexAgent(codex).WithSandbox(&CodexSandbox{Job: job, upstream: server.URL})
	a.Model = "test-model"
	var output bytes.Buffer
	plain, err := a.Review(t.Context(), repo, "HEAD", "Return the object.", &output)
	require.NoError(t, err, "%s", output.String())
	require.JSONEq(t, `{"ok":true}`, plain)
	output.Reset()
	result, err := a.ReviewWithSchema(t.Context(), repo, "HEAD", "Return the object.", jsontext.Value(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`), &output)
	require.NoError(t, err, "%s", output.String())
	assert.JSONEq(t, `{"ok":true}`, string(result))
}

func TestCodexSandboxLauncherReuse(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
	}
	work, secrets := t.TempDir(), t.TempDir()
	repo := filepath.Join(work, "repo")
	require.NoError(t, os.Mkdir(repo, 0o700))
	t.Setenv("CODEX_HOME", secrets)
	marker := filepath.Join(secrets, "host-write")
	launcher := filepath.Join(work, "codex")
	replacement := filepath.Join(work, "replacement")
	const reply = `echo '{"type":"item.completed","item":{"type":"agent_message","text":"contained"}}'`
	help := "case \" $* \" in *' --help '*) echo '--thread-source --dangerously-bypass-approvals-and-sandbox'; exit 0;; esac\n"
	require.NoError(t, os.WriteFile(replacement, []byte(fmt.Sprintf("#!/bin/sh\ntouch %q 2>/dev/null || :\n%s%s\n", marker, help, reply)), 0o700))
	require.NoError(t, os.WriteFile(launcher, []byte(fmt.Sprintf("#!/bin/sh\n%s{\ncp %q \"$0\"\n%s\nexit\n}\n", help, replacement, reply)), 0o700))
	a := NewCodexAgent(launcher).WithSandbox(&CodexSandbox{Job: sandbox.Job{
		WorkDir: work, MemoryMax: 1 << 30,
		Env: []string{"PATH=/usr/bin:/bin", "HOME=" + secrets, "TMPDIR=" + work},
	}})
	for range 2 {
		result, err := a.Review(t.Context(), repo, "HEAD", "review", nil)
		require.NoError(t, err)
		assert.Equal(t, "contained", result)
		assert.NoFileExists(t, marker)
	}
}

func TestCodexSandboxRejectsLoginReadPaths(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
	}
	for _, grant := range []string{"home", "parent", "symlink", "relative"} {
		t.Run(grant, func(t *testing.T) {
			work, secrets := t.TempDir(), t.TempDir()
			repo, hostHome := filepath.Join(work, "repo"), filepath.Join(secrets, "codex")
			require.NoError(t, os.Mkdir(repo, 0o700))
			require.NoError(t, os.Mkdir(hostHome, 0o700))
			t.Setenv("CODEX_HOME", hostHome)
			auth := filepath.Join(hostHome, "auth.json")
			require.NoError(t, os.WriteFile(auth, []byte(`{"tokens":{"access_token":"synthetic-token"}}`), 0o600))
			readPath := hostHome
			switch grant {
			case "parent":
				readPath = secrets
			case "symlink":
				readPath = filepath.Join(secrets, "link")
				require.NoError(t, os.Symlink(hostHome, readPath))
			case "relative":
				t.Chdir(secrets)
				readPath = "codex"
				t.Setenv("CODEX_HOME", "codex")
			}
			marker := filepath.Join(work, "exposed-login")
			launcher := filepath.Join(work, "codex")
			script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in *' --help '*) echo '--thread-source --dangerously-bypass-approvals-and-sandbox'; exit 0;; esac\ncat %q > %q\necho '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"review\"}}'\n", auth, marker)
			require.NoError(t, os.WriteFile(launcher, []byte(script), 0o700))
			a := NewCodexAgent(launcher).WithSandbox(&CodexSandbox{Job: sandbox.Job{
				WorkDir: work, ReadPaths: []string{readPath}, MemoryMax: 1 << 30,
				Env: []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "TMPDIR=" + work},
			}})
			_, err := a.Review(t.Context(), repo, "HEAD", "review", nil)
			assert.ErrorContains(t, err, "contains the Codex login")
			assert.NoFileExists(t, marker)
		})
	}
}

func sandboxRouteItem(t *testing.T, outputs []string) map[string]any {
	t.Helper()
	name := "exec_command"
	args := map[string]any{"cmd": "bash ../probe.sh", "yield_time_ms": 10000}
	if len(outputs) > 0 {
		final, err := json.Marshal(map[string]string{"probe": strings.Join(outputs, "\n")})
		assert.NoError(t, err)
		return map[string]any{"type": "message", "role": "assistant", "id": "msg_test", "content": []any{map[string]any{"type": "output_text", "text": string(final)}}}
	}
	encoded, err := json.Marshal(args)
	assert.NoError(t, err)
	id := fmt.Sprintf("tool_%d", len(outputs))
	return map[string]any{"type": "function_call", "id": id, "call_id": id, "name": name, "arguments": string(encoded)}
}
