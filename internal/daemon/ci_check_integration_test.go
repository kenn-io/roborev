//go:build integration

package daemon

import (
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

	"go.kenn.io/roborev/internal/agent"
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

	for _, mode := range []string{"rewrite", "second failure"} {
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
				if mode == "second failure" && phase == 3 {
					http.Error(w, "synthetic rewrite failure", http.StatusBadRequest)
					return
				}
				var err error
				checkouts, err = filepath.Glob(checkoutPattern)
				assert.NoError(t, err)
				final := `{"verdicts":[{"finding":1,"keep":false,"evidence":"go build succeeded","reason":"Build claim refuted"},{"finding":2,"keep":true,"evidence":"test reproduced data loss","reason":"Data is lost"}]}`
				if phase == 2 {
					final = `{"findings":[{"finding":2,"problem":"Users lose their files.","fix":"Preserve data in saveFile."}]}`
				} else if phase == 3 {
					final = `{"findings":[{"finding":2,"fix":"Keep the saved files. **Where:** ` + "`saveFile`" + `"}]}`
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
			tc.Pool.checkAndCompleteSynthesis(t.Context(), testWorkerID, job, synthesisResult{review: reviewpkg.ReviewResult{Agent: "test", Output: doc.Markdown(""), StructuredOutput: raw, Verdict: storage.VerdictFail}}, doc, &ciCheckConfig{Model: "test-model", Reasoning: "high", Rewrite: true, Description: "Export user files"})
			log, err := os.ReadFile(JobLogPath(job.ID))
			require.NoError(t, err)
			require.NotEmpty(t, checkouts, string(log))
			for _, checkout := range checkouts {
				assert.NoDirExists(t, checkout)
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

func TestCIRewriteSandboxGitConfigStaysInsideJob(t *testing.T) {
	testutil.RequireSandboxIntegration(t)
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	loginDir := t.TempDir()
	t.Setenv("CODEX_HOME", loginDir)
	require.NoError(t, os.WriteFile(filepath.Join(loginDir, "auth.json"), []byte(`{"tokens":{"access_token":"rewrite-test-token"}}`), 0o600))
	repo := testutil.NewTestRepoWithCommit(t)
	command := filepath.Join(t.TempDir(), "codex")
	const fakeCodex = `#!/bin/sh
while [ "$#" -gt 0 ]; do
    if [ "$1" = "-C" ]; then shift; cd "$1"; break; fi
    shift
done
if [ ! -f rewrite-prompts.txt ]; then
    printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"findings\":[{\"finding\":2,\"problem\":\"Users lose saved files.\",\"fix\":\"Preserve files in saveFile.\"}]}"}}'
else
    printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"findings\":[{\"finding\":2,\"fix\":\"Keep saved files. **Where:** saveFile\"}]}"}}'
fi
cat >> rewrite-prompts.txt
`
	require.NoError(t, os.WriteFile(command, []byte(fakeCodex), 0o700))
	sha := repo.HeadSHA()
	job := &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha, MinSeverity: "medium"}
	run, err := (&WorkerPool{}).prepareCICheck(t.Context(), job, command, nil, io.Discard)
	require.NoError(t, err)
	t.Cleanup(run.cleanup)
	marker := filepath.Join(t.TempDir(), "outside-job-marker")
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	run.Job.Route = sandbox.Route{Name: "test", Upstream: server.URL, Path: "/responses", EnvVar: "TEST_TOKEN", Capture: []string{"/bin/echo", "synthetic-token"}}
	const install = `set -e
cat > "$1/fsmonitor-probe" <<HOOK
#!/bin/sh
set -e
printf host-executed > "$2"
printf 'token\\0'
HOOK
chmod +x "$1/fsmonitor-probe"
if "$1/fsmonitor-probe"; then
    echo 'sandbox unexpectedly wrote outside its grant'
    exit 1
fi
git -C "$1" config core.fsmonitor "$1/fsmonitor-probe"
`
	argv, env, cleanup, err := run.Job.Command(t.Context(), []string{"bash", "--norc", "--noprofile", "-c", install, "probe", run.Repo, marker})
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), argv[0], argv[1:]...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	cleanup()
	require.NoError(t, err, "%s", output)

	cfg := config.DefaultConfig()
	// Keep the old snapshot path oversized so reintroducing it triggers the host-Git regression.
	cfg.DefaultMaxPromptSize = 1
	wp := &WorkerPool{cfgGetter: NewStaticConfig(cfg)}
	doc := structuredreview.Document{Findings: []structuredreview.Finding{{Severity: "high", Problem: "build claim", Fix: "fix"}, {Severity: "medium", Location: "save.go:4", Problem: "data loss", Fix: "fix"}, {Severity: "low", Problem: "minor issue", Fix: "minor fix"}}}
	checked, notes, err := applyCICheck(doc, jsontext.Value(`{"verdicts":[{"finding":1,"keep":false,"evidence":"go build succeeded","reason":"Build claim refuted"},{"finding":2,"keep":true,"evidence":"test reproduced data loss","reason":"Data is lost"}]}`), io.Discard)
	require.NoError(t, err)
	a := agent.NewCodexAgent(run.Command).WithSandbox(&agent.CodexSandbox{Job: run.Job, Upstream: server.URL})
	description := "Complete rewrite context."
	got, err := wp.rewriteCIFindings(t.Context(), job, ciCheckConfig{Description: description}, checked, notes, a, run.Repo, io.Discard)
	require.NoError(t, err)
	assert.NoFileExists(t, marker, "host Git executed the sandbox-installed fsmonitor")
	prompts, err := os.ReadFile(filepath.Join(run.Repo, "rewrite-prompts.txt"))
	require.NoError(t, err)
	assert.Contains(t, string(prompts), description)
	assert.Contains(t, string(prompts), "## Finding 2 (medium)\n\n- `save.go:4`: data loss\n\n  **Fix:** fix")
	assert.NotContains(t, string(prompts), "## Finding 1 (high)")
	assert.NotContains(t, string(prompts), "## Finding 3 (low)")
	assert.Contains(t, string(prompts), "test reproduced data loss")
	assert.Contains(t, string(prompts), "Data is lost")
	assert.Contains(t, string(prompts), "Users lose saved files.")
	assert.Equal(t, "Keep saved files. **Where:** saveFile", got.Findings[0].Fix)
}
