//go:build integration

package daemon

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestCICheckSandboxIntegration(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
	}
	for _, name := range []string{"nono", "codex"} {
		_, err := exec.LookPath(name)
		require.NoError(t, err)
	}
	t.Run("native", testCICheckSandboxIntegration)
	t.Run("npm", func(t *testing.T) {
		codex, err := resolveCICheckCommand(t.Context(), t.TempDir(), "codex")
		require.NoError(t, err)
		installCICheckNPM(t, codex)
		testCICheckSandboxIntegration(t)
	})
}

func testCICheckSandboxIntegration(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	repoRoot := tc.TmpDir
	loginDir := t.TempDir()
	t.Setenv("CODEX_HOME", loginDir)
	require.NoError(t, os.WriteFile(filepath.Join(loginDir, "auth.json"), []byte(`{"tokens":{"access_token":"checker-test-token"}}`), 0o600))
	beforeRefs, err := exec.CommandContext(t.Context(), "git", "-C", repoRoot, "show-ref").CombinedOutput()
	require.NoError(t, err)
	beforeStatus, err := exec.CommandContext(t.Context(), "git", "-C", repoRoot, "status", "--porcelain").CombinedOutput()
	require.NoError(t, err)
	rootPattern := filepath.Join(ciWorktreeRepoDir(repoRoot), ciWorktreePrefix+"*", "roborev-ci-check-*")
	oldRoots, err := filepath.Glob(rootPattern)
	require.NoError(t, err)
	var roots, checkouts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer checker-test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		assert.Equal(t, "high", request.Reasoning.Effort)
		found, err := filepath.Glob(rootPattern)
		assert.NoError(t, err)
		for _, root := range found {
			old := false
			for _, v := range oldRoots {
				if root == v {
					old = true
				}
			}
			if !old {
				roots = append(roots, root)
				schemas, err := filepath.Glob(filepath.Join(root, "work", "roborev-codex-review-schema-*.json"))
				assert.NoError(t, err)
				assert.NotEmpty(t, schemas, "schema must live in the sandbox work directory")
			}
		}
		foundCheckouts, err := filepath.Glob(filepath.Join(ciWorktreeRepoDir(repoRoot), ciWorktreePrefix+"*"))
		assert.NoError(t, err)
		checkouts = append(checkouts, foundCheckouts...)
		const final = `{"verdicts":[{"finding":1,"keep":false,"evidence":"go build succeeded","reason":"Build claim refuted"},{"finding":2,"keep":true,"evidence":"test reproduced data loss","reason":"Data is lost"}]}`
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
	doc := structuredreview.Document{SchemaVersion: 3, SourceLabels: []string{"test"}, Summary: "The build claim and data loss need fixes", Verdict: "fail", Findings: []structuredreview.Finding{{Severity: "high", Problem: "build claim", Fix: "fix", Sources: []int{1}}, {Severity: "medium", Problem: "data loss", Fix: "fix", Sources: []int{1}}}}
	run, members, _ := enqueuePanelRun(t, tc, "checker-integration", []memberSpec{{name: "one", agent: "test"}})
	completeMember(t, tc, members[0].ID, "test", "findings")
	job := releaseAndClaimSynthesis(t, tc, run)
	job.GitRef = sha + ".." + sha
	tc.Pool.ciCheckUpstream = server.URL
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	tc.Pool.checkAndCompleteSynthesis(t.Context(), testWorkerID, job, synthesisResult{review: reviewpkg.ReviewResult{Agent: "test", Output: doc.Markdown(""), StructuredOutput: raw, Verdict: storage.VerdictFail}}, doc, &ciCheckConfig{Model: "test-model", Reasoning: "high"})
	log, err := os.ReadFile(JobLogPath(job.ID))
	require.NoError(t, err)
	require.NotEmpty(t, checkouts, string(log))
	for _, checkout := range checkouts {
		assert.NoDirExists(t, checkout)
	}
	require.NotEmpty(t, roots, string(log))
	for _, root := range roots {
		assert.NoDirExists(t, root)
	}
	afterRefs, err := exec.CommandContext(t.Context(), "git", "-C", repoRoot, "show-ref").CombinedOutput()
	require.NoError(t, err)
	assert.Equal(t, string(beforeRefs), string(afterRefs))
	afterStatus, err := exec.CommandContext(t.Context(), "git", "-C", repoRoot, "status", "--porcelain").CombinedOutput()
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(string(beforeStatus)), strings.TrimSpace(string(afterStatus)))
}
