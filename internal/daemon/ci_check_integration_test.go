//go:build integration

package daemon

import (
	"encoding/json/v2"
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
	oldRoots, err := filepath.Glob(filepath.Join(os.TempDir(), "roborev-ci-check-*"))
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
		found, err := filepath.Glob(filepath.Join(os.TempDir(), "roborev-ci-check-*"))
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
		testutil.WriteCodexResponse(t, w, map[string]any{"type": "message", "role": "assistant", "id": "msg_test", "content": []any{map[string]any{"type": "output_text", "text": final}}})
	}))
	defer server.Close()
	sha := testutil.GetHeadSHA(t, repoRoot)
	doc := structuredreview.Document{SchemaVersion: 3, SourceLabels: []string{"test"}, Summary: "The build claim and data loss need fixes", Verdict: "fail", Findings: []structuredreview.Finding{{Severity: "high", Problem: "build claim", Fix: "fix", Sources: []int{1}}, {Severity: "medium", Problem: "data loss", Fix: "fix", Sources: []int{1}}}}
	run, members, _ := enqueuePanelRun(t, tc, "checker-integration", []memberSpec{{name: "one", agent: "test"}})
	completeMember(t, tc, members[0].ID, "test", "findings")
	job := releaseAndClaimSynthesis(t, tc, run)
	job.GitRef = sha + ".." + sha
	job.PanelMemberConfigJSON = `{"check":{"model":"test-model","reasoning":"high"}}`
	tc.Pool.ciCheckUpstream = server.URL
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	tc.Pool.checkAndCompleteSynthesis(t.Context(), testWorkerID, job, synthesisResult{review: reviewpkg.ReviewResult{Agent: "test", Output: doc.Markdown(""), StructuredOutput: raw, Verdict: storage.VerdictFail}}, &ciCheckConfig{Model: "test-model", Reasoning: "high"})
	stored, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	log, err := os.ReadFile(JobLogPath(job.ID))
	require.NoError(t, err)
	assert.NotContains(t, stored.Output, "build claim", string(log))
	assert.Contains(t, stored.Output, "data loss")
	encoded, err := json.Marshal(stored.StructuredOutput)
	require.NoError(t, err)
	checked, err := reviewpkg.DecodeStructuredReview(encoded)
	require.NoError(t, err)
	assert.Equal(t, "1 finding.", checked.Summary)
	require.Len(t, checked.Findings, 1)
	assert.Equal(t, "data loss", checked.Findings[0].Problem)
	assert.Equal(t, storage.VerdictFail, stored.Verdict())
	assert.Contains(t, string(log), "go build succeeded")
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
