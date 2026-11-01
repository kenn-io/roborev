//go:build integration

package daemon

import (
	"bytes"
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
	"go.kenn.io/roborev/internal/prompt"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/sandbox"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/tokens"
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

	tc := newWorkerTestContext(t, 1)
	if command := os.Getenv("ROBOREV_TEST_CODEX_COMMAND"); command != "" {
		cfg := config.DefaultConfig()
		cfg.CodexCmd = command
		tc.Pool.cfgGetter = NewStaticConfig(cfg)
	}
	repoRoot := tc.TmpDir
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "fixture.txt"), []byte("committed contents"), 0o600))
	gitCmd := exec.Command("git", "-C", repoRoot, "add", "fixture.txt")
	require.NoError(t, gitCmd.Run())
	gitCmd = exec.Command("git", "-C", repoRoot, "commit", "-m", "fixture")
	require.NoError(t, gitCmd.Run())
	loginDir := t.TempDir()
	t.Setenv("CODEX_HOME", loginDir)
	require.NoError(t, os.WriteFile(filepath.Join(loginDir, "auth.json"), []byte(`{"tokens":{"access_token":"checker-test-token"}}`), 0o600))
	checkoutPattern := filepath.Join(ciWorktreeRepoDir(repoRoot), ciWorktreePrefix+"*")
	var checkouts []string
	var checkerRepo string
	var calls atomic.Int32
	var skipRewrite atomic.Bool
	var probes atomic.Int32
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
		var inputs []struct {
			Type   string `json:"type"`
			Output string `json:"output"`
		}
		assert.NoError(t, json.Unmarshal(request.Input, &inputs))
		var toolOutput string
		for _, input := range inputs {
			if input.Type == "function_call_output" {
				toolOutput = input.Output
			}
		}
		phase := calls.Load()
		if toolOutput == "" {
			phase = calls.Add(1)
		} else {
			assert.Contains(t, toolOutput, "OK rewrite read-only")
			assert.NotContains(t, toolOutput, "UNEXPECTED")
			probes.Add(1)
		}
		var rewriteWork string
		if phase == 1 {
			assert.Contains(t, string(request.Input), "Make it fail.")
		}
		var err error
		checkouts, err = filepath.Glob(checkoutPattern)
		assert.NoError(t, err)
		assert.Len(t, checkouts, 1)
		if len(checkouts) == 1 {
			roots, err := filepath.Glob(filepath.Join(checkouts[0], "roborev-ci-check-*"))
			assert.NoError(t, err)
			assert.Len(t, roots, 1)
			if len(roots) == 1 {
				checkerRepo = filepath.Join(roots[0], "work", "repo")
				if phase == 1 {
					// Simulate files changed by commands the checker ran in its clone.
					assert.NoError(t, os.WriteFile(filepath.Join(checkerRepo, "fixture.txt"), []byte("checker dirtied this"), 0o600))
					if skipRewrite.Load() {
						// Any attempted rewrite setup must fail and appear in the log.
						for _, name := range []string{"rewrite", "rewrite-source"} {
							assert.NoError(t, os.WriteFile(filepath.Join(roots[0], name), []byte("setup blocked"), 0o600))
						}
					}
				} else {
					rewriteRepo := filepath.Join(roots[0], "rewrite-source", "repo")
					rewriteWork = filepath.Join(roots[0], "rewrite")
					assert.NotEqual(t, checkerRepo, rewriteRepo)
					content, err := os.ReadFile(filepath.Join(rewriteRepo, "fixture.txt"))
					assert.NoError(t, err)
					assert.Equal(t, "committed contents", string(content))
					assert.Contains(t, string(request.Input), rewriteRepo)
					assert.NotContains(t, string(request.Input), "distinctive checker evidence")
					assert.NotContains(t, string(request.Input), "distinctive checker reason")
					assert.NotContains(t, string(request.Input), "checker dirtied this")
					assert.Contains(t, string(request.Input), "Export files")
				}
			}
		}
		final := `{"verdicts":[{"finding":1,"keep":false,"evidence":"go build succeeded","reason":"Build claim refuted"},{"finding":2,"keep":true,"evidence":"distinctive checker evidence","reason":"distinctive checker reason"}]}`
		if phase == 2 {
			final = `{"findings":[{"finding":1,"problem":"Users lose their files.","fix":"Preserve data in saveFile."}]}`
		} else if phase == 3 {
			final = `{"findings":[{"finding":1,"fix":"Keep the saved files. **Where:** ` + "`saveFile`" + `"}]}`
		}
		// Distinct counts expose a missing checker or rewrite invocation.
		usage := map[string]any{"input_tokens": 11, "output_tokens": 2, "total_tokens": 13, "input_tokens_details": map[string]int{"cached_tokens": 3}}
		if phase == 2 {
			usage = map[string]any{"input_tokens": 23, "output_tokens": 5, "total_tokens": 28, "input_tokens_details": map[string]int{"cached_tokens": 7}}
		} else if phase == 3 {
			usage = map[string]any{"input_tokens": 41, "output_tokens": 7, "total_tokens": 48, "input_tokens_details": map[string]int{"cached_tokens": 13}}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		item := map[string]any{"type": "message", "role": "assistant", "id": "msg_test", "content": []any{map[string]any{"type": "output_text", "text": final}}}
		if phase > 1 && toolOutput == "" {
			// Run through Codex's tool so the writes originate inside its sandbox.
			probe := fmt.Sprintf(`set -e
test "$(cat fixture.txt)" = 'committed contents'
if printf 'rewrite edit' > fixture.txt; then echo 'UNEXPECTED write'; exit 1; fi
chmod u+w fixture.txt || true
if printf 'rewrite edit' > fixture.txt; then echo 'UNEXPECTED write after chmod'; exit 1; fi
if touch new-file; then echo 'UNEXPECTED create'; exit 1; fi
printf 'scratch edit' > %q/probe.txt
echo 'OK rewrite read-only'
`, rewriteWork)
			args, err := json.Marshal(map[string]any{"cmd": probe, "yield_time_ms": 10000})
			assert.NoError(t, err)
			item = map[string]any{"type": "function_call", "id": "tool_test", "call_id": "tool_test", "name": "exec_command", "arguments": string(args)}
		}
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp_test"}},
			{"type": "response.output_item.done", "item": item},
			{"type": "response.completed", "response": map[string]any{"id": "resp_test", "usage": usage}},
		} {
			encoded, err := json.Marshal(event)
			assert.NoError(t, err) //nolint:testifylint // Runs in the HTTP server goroutine.
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], encoded)
			assert.NoError(t, err)
		}
	}))
	defer server.Close()
	sha := testutil.GetHeadSHA(t, repoRoot)
	doc := structuredreview.Document{SchemaVersion: 3, SourceLabels: []string{"test"}, Summary: "The build claim and data loss need fixes", Verdict: "fail", Findings: []structuredreview.Finding{{Severity: "high", Kind: "defect", Confidence: .9, Problem: "build claim", Fix: "fix", Sources: []int{1}}, {Severity: "medium", Kind: "defect", Confidence: .9, Location: "save.go:4", Problem: "data loss", Fix: "fix", Sources: []int{1}}, {Severity: "low", Kind: "defect", Confidence: .9, Problem: "minor issue", Fix: "minor fix", Sources: []int{1}}}}
	run, members, _ := enqueuePanelRun(t, tc, "checker-integration", []memberSpec{{name: "one", agent: "test"}})
	completeMember(t, tc, members[0].ID, "test", "findings")
	job := releaseAndClaimSynthesis(t, tc, run)
	job.GitRef = sha + ".." + sha
	job.MinSeverity = "medium"
	tc.Pool.ciCheckUpstream = server.URL
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	tc.Pool.checkAndCompleteSynthesis(t.Context(), testWorkerID, job, synthesisResult{review: reviewpkg.ReviewResult{Agent: "test", Output: doc.Markdown(""), StructuredOutput: raw, Verdict: storage.VerdictFail}}, doc, &ciCheckConfig{Model: "test-model", Reasoning: "high", Rewrite: true, PullRequest: prompt.PullRequestBlock(prompt.PullRequest{Title: "Export files"})})
	log, err := os.ReadFile(JobLogPath(job.ID))
	require.NoError(t, err)
	require.NotEmpty(t, checkouts, string(log))
	for _, checkout := range checkouts {
		assert.NoDirExists(t, checkout)
	}
	stored, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	finished, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(finished.TokenUsage)
	require.NotNil(t, usage)
	// The checker has one response; each rewrite has a tool call and a final response.
	assert.EqualValues(t, 139, usage.InputTokens)
	assert.EqualValues(t, 43, usage.CachedInputTokens)
	assert.EqualValues(t, 26, usage.OutputTokens)
	assert.False(t, usage.HasCost)
	assert.Empty(t, finished.SessionID)
	var invoked bool
	require.NoError(t, tc.DB.QueryRow("SELECT agent_invoked FROM review_jobs WHERE id = ?", job.ID).Scan(&invoked))
	assert.True(t, invoked)
	assert.NotEmpty(t, finished.CommandLine)

	encoded, err := json.Marshal(stored.StructuredOutput)
	require.NoError(t, err)
	checked, err := reviewpkg.DecodeStructuredReview(encoded)
	require.NoError(t, err)
	require.Len(t, checked.Findings, 2)
	assert.EqualValues(t, 3, calls.Load())
	assert.EqualValues(t, 2, probes.Load())
	assert.Equal(t, "Users lose their files.", checked.Findings[0].Problem)
	assert.Equal(t, "Keep the saved files. **Where:** `saveFile`", checked.Findings[0].Fix)
	assert.NotContains(t, string(log), "CI checker failed;")

	calls.Store(0)
	kept, err := tc.Pool.runCICheck(t.Context(), job, ciCheckConfig{Model: "test-model", Reasoning: "high"}, doc, nil, io.Discard)
	require.NoError(t, err)
	assert.EqualValues(t, 1, calls.Load(), "rewrite off only calls the checker")
	assert.Equal(t, doc.Findings[1:], kept.Findings)

	calls.Store(0)
	skipRewrite.Store(true)
	job.MinSeverity = "high"
	var output bytes.Buffer
	_, err = tc.Pool.runCICheck(t.Context(), job, ciCheckConfig{Model: "test-model", Reasoning: "high", Rewrite: true}, doc, nil, &output)
	require.NoError(t, err)
	assert.EqualValues(t, 1, calls.Load(), "kept findings below the threshold only call the checker")
	assert.NotContains(t, output.String(), "CI rewrite failed;")

	failureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic provider failure", http.StatusBadRequest)
	}))
	defer failureServer.Close()
	tc.Pool.ciCheckUpstream = failureServer.URL
	for _, commandLine := range []string{"", "synthesis-agent --model test-model"} {
		t.Run("provider failure/"+commandLine, func(t *testing.T) {
			run, members, _ := enqueuePanelRun(t, tc, "checker-failure", []memberSpec{{name: "one", agent: "test"}})
			completeMember(t, tc, members[0].ID, "test", "findings")
			job := releaseAndClaimSynthesis(t, tc, run)
			job.GitRef = sha + ".." + sha
			if commandLine != "" {
				require.NoError(t, tc.DB.MarkJobAgentInvoked(job.ID, testWorkerID, commandLine))
			}
			_, err := tc.Pool.runCICheck(t.Context(), job, ciCheckConfig{Model: "test-model", Reasoning: "high"}, doc, nil, io.Discard)
			require.ErrorContains(t, err, "run CI checker")
			if commandLine == "" {
				require.NoError(t, tc.DB.QueryRow("SELECT agent_invoked FROM review_jobs WHERE id = ?", job.ID).Scan(&invoked))
				assert.True(t, invoked)
			}
			finished, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			if commandLine != "" {
				assert.Equal(t, commandLine, finished.CommandLine)
			}
		})
	}
}
