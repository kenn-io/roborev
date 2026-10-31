//go:build integration

package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/sandbox"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestCICheckGitCredentialIsolationSandboxIntegration(t *testing.T) {
	testutil.RequireSandboxIntegration(t)
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("fixture.txt", "original\n", "checker history fixture")
	repo.Run("config", "http.https://example.com/.extraheader", "Authorization: Bearer synthetic-git-credential")
	command := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(command, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	sha := repo.HeadSHA()
	wp := &WorkerPool{}
	run, err := wp.prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}, command, nil, io.Discard)
	require.NoError(t, err)
	t.Cleanup(run.cleanup)
	require.NoError(t, os.WriteFile(filepath.Join(run.Repo, "fixture.txt"), []byte("changed\n"), 0o600))
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	run.Job.Route = sandbox.Route{Name: "test", Upstream: server.URL, Path: "/responses", EnvVar: "TEST_TOKEN", Capture: []string{"/bin/echo", "synthetic-proxy-token"}}
	const probe = `set -e
git -C "$2" status --porcelain --untracked-files=no
git -C "$2" diff -- fixture.txt
git -C "$2" log -1 --format=%s
if cat "$1"; then exit 1; fi
`
	argv, env, cleanup, err := run.Job.Command(t.Context(), []string{"bash", "--norc", "--noprofile", "-c", probe, "credential-probe", filepath.Join(repo.Root, ".git", "config"), run.Repo})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	cmd := exec.CommandContext(t.Context(), argv[0], argv[1:]...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	assert := assert.New(t)
	assert.NoError(err, "%s", output)
	assert.Contains(string(output), " M fixture.txt")
	assert.Contains(string(output), "+changed")
	assert.Contains(string(output), "checker history fixture")
}

func TestCICheckSandboxIntegration(t *testing.T) {
	testutil.RequireSandboxIntegration(t)
	for _, name := range []string{"nono", "codex"} {
		_, err := exec.LookPath(name)
		require.NoError(t, err)
	}

	for _, mode := range []string{"rewrite", "first failure", "second failure", "missing ID", "duplicate ID", "unknown ID", "empty problem", "empty fix", "cancel second"} {
		t.Run(mode, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			if command := os.Getenv("ROBOREV_TEST_CODEX_COMMAND"); command != "" {
				cfg := config.DefaultConfig()
				cfg.CodexCmd = command
				tc.Pool.cfgGetter = NewStaticConfig(cfg)
			}
			repoRoot := tc.TmpDir
			loginDir := t.TempDir()
			t.Setenv("CODEX_HOME", loginDir)
			require.NoError(t, os.WriteFile(filepath.Join(loginDir, "auth.json"), []byte(`{"tokens":{"access_token":"checker-test-token"}}`), 0o600))
			checkoutPattern := filepath.Join(ciWorktreeRepoDir(repoRoot), ciWorktreePrefix+"*")
			var checkouts []string
			var calls atomic.Int32
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input     jsontext.Value `json:"input"`
					Reasoning struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
				}
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				assert.Equal(t, "high", request.Reasoning.Effort)
				phase := calls.Add(1)
				if phase == 1 {
					assert.Contains(t, string(request.Input), "Make it fail.")
				}
				if phase == 2 {
					assert.Contains(t, string(request.Input), "Export user files")
					assert.Contains(t, string(request.Input), "## Finding 2 (medium)\\n\\n- `save.go:4`: data loss\\n\\n  **Fix:** fix")
					assert.NotContains(t, string(request.Input), "## Finding 1 (high)")
					assert.NotContains(t, string(request.Input), "## Finding 3 (low)")
					assert.Contains(t, string(request.Input), "test reproduced data loss")
					assert.Contains(t, string(request.Input), "Data is lost")
				}
				if phase == 3 {
					assert.Contains(t, string(request.Input), "Users lose their files.")
				}
				if mode == "first failure" && phase == 2 || mode == "second failure" && phase == 3 {
					http.Error(w, "synthetic rewrite failure", http.StatusBadRequest)
					return
				}
				if mode == "cancel second" && phase == 3 {
					cancel()
					return
				}
				var err error
				checkouts, err = filepath.Glob(checkoutPattern)
				assert.NoError(t, err)
				final := `{"verdicts":[{"finding":1,"keep":false,"evidence":"go build succeeded","reason":"Build claim refuted"},{"finding":2,"keep":true,"evidence":"test reproduced data loss","reason":"Data is lost"}]}`
				if phase == 2 {
					final = `{"findings":[{"finding":2,"problem":"Users lose their files.","fix":"Preserve data in saveFile."}]}`
					switch mode {
					case "missing ID":
						final = `{"findings":[]}`
					case "duplicate ID":
						final = `{"findings":[{"finding":2,"problem":"one","fix":"fix"},{"finding":2,"problem":"two","fix":"fix"}]}`
					case "unknown ID":
						final = `{"findings":[{"finding":1,"problem":"wrong","fix":"fix"}]}`
					case "empty problem":
						final = `{"findings":[{"finding":2,"problem":" ","fix":"fix"}]}`
					}
				} else if phase == 3 {
					final = `{"findings":[{"finding":2,"fix":"Keep the saved files. **Where:** ` + "`saveFile`" + `"}]}`
					if mode == "empty fix" {
						final = `{"findings":[{"finding":2,"fix":" "}]}`
					}
				}

				w.Header().Set("Content-Type", "text/event-stream")
				item := map[string]any{"type": "message", "role": "assistant", "id": "msg_test", "content": []any{map[string]any{"type": "output_text", "text": final}}}
				for _, event := range []map[string]any{
					{"type": "response.created", "response": map[string]any{"id": "resp_test"}},
					{"type": "response.output_item.done", "item": item},
					{"type": "response.completed", "response": map[string]any{"id": "resp_test", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
				} {
					encoded, err := json.Marshal(event)
					assert.NoError(t, err) //nolint:testifylint // Runs in the HTTP server goroutine.
					_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], encoded)
					assert.NoError(t, err)
				}
			}))
			defer server.Close()
			sha := testutil.GetHeadSHA(t, repoRoot)
			doc := structuredreview.Document{SchemaVersion: 3, SourceLabels: []string{"test"}, Summary: "The build claim and data loss need fixes", Verdict: "fail", Findings: []structuredreview.Finding{{Severity: "high", Problem: "build claim", Fix: "fix", Sources: []int{1}}, {Severity: "medium", Kind: "defect", Confidence: .9, Location: "save.go:4", Problem: "data loss", Fix: "fix", Sources: []int{1}}, {Severity: "low", Problem: "minor issue", Fix: "minor fix", Sources: []int{1}}}}
			run, members, _ := enqueuePanelRun(t, tc, "checker-integration", []memberSpec{{name: "one", agent: "test"}})
			completeMember(t, tc, members[0].ID, "test", "findings")
			job := releaseAndClaimSynthesis(t, tc, run)
			job.GitRef = sha + ".." + sha
			job.MinSeverity = "medium"
			tc.Pool.ciCheckUpstream = server.URL
			raw, err := json.Marshal(doc)
			require.NoError(t, err)
			tc.Pool.checkAndCompleteSynthesis(ctx, testWorkerID, job, synthesisResult{review: reviewpkg.ReviewResult{Agent: "test", Output: doc.Markdown(""), StructuredOutput: raw, Verdict: storage.VerdictFail}}, doc, &ciCheckConfig{Model: "test-model", Reasoning: "high", Rewrite: true, Description: "Export user files"})
			log, err := os.ReadFile(JobLogPath(job.ID))
			require.NoError(t, err)
			require.NotEmpty(t, checkouts, string(log))
			for _, checkout := range checkouts {
				assert.NoDirExists(t, checkout)
			}
			if mode == "cancel second" {
				finished, err := tc.DB.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Equal(t, storage.JobStatusRunning, finished.Status)
				return
			}
			stored, err := tc.DB.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			assert.NotContains(t, stored.Output, "build claim", string(log))
			encoded, err := json.Marshal(stored.StructuredOutput)
			require.NoError(t, err)
			checked, err := reviewpkg.DecodeStructuredReview(encoded)
			require.NoError(t, err)
			require.Len(t, checked.Findings, 2)
			expected := doc.Findings[1]
			if mode == "rewrite" {
				expected.Problem = "Users lose their files."
				expected.Fix = "Keep the saved files. **Where:** `saveFile`"
				assert.EqualValues(t, 3, calls.Load())
			} else {
				assert.Contains(t, string(log), "CI rewrite failed; keeping checked review:")
				assert.NotContains(t, stored.Output, "Users lose their files.")
			}
			assert.Equal(t, expected, checked.Findings[0])
			assert.Equal(t, doc.Findings[2], checked.Findings[1])
			assert.Equal(t, doc.SourceLabels, checked.SourceLabels)
			assert.Equal(t, doc.SchemaVersion, checked.SchemaVersion)
			assert.NotContains(t, string(log), "CI checker failed;")
		})
	}
}
