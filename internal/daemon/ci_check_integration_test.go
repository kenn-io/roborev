//go:build integration

package daemon

import (
	"bytes"
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

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
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
	repo := testutil.NewTestRepoWithCommit(t)
	loginDir := t.TempDir()
	t.Setenv("CODEX_HOME", loginDir)
	require.NoError(t, os.WriteFile(filepath.Join(loginDir, "auth.json"), []byte(`{"tokens":{"access_token":"checker-test-token"}}`), 0o600))
	beforeRefs, err := exec.CommandContext(t.Context(), "git", "-C", repo.Root, "show-ref").CombinedOutput()
	require.NoError(t, err)
	beforeStatus, err := exec.CommandContext(t.Context(), "git", "-C", repo.Root, "status", "--porcelain").CombinedOutput()
	require.NoError(t, err)
	oldRoots, err := filepath.Glob(filepath.Join(os.TempDir(), "roborev-ci-check-*"))
	require.NoError(t, err)
	var roots []string
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
			}
		}
		const final = `{"verdicts":[{"finding":1,"keep":false,"evidence":"go build succeeded","reason":"Build claim refuted"},{"finding":2,"keep":true,"evidence":"test reproduced data loss","reason":"Data is lost"}]}`
		testutil.WriteCodexResponse(t, w, map[string]any{"type": "message", "role": "assistant", "id": "msg_test", "content": []any{map[string]any{"type": "output_text", "text": final}}})
	}))
	defer server.Close()
	cfg := config.DefaultConfig()
	cfg.CI.CheckReasoning = " HIGH "
	require.NoError(t, cfg.Validate())
	sha := testutil.GetHeadSHA(t, repo.Root)
	doc := structuredreview.Document{SchemaVersion: 3, SourceLabels: []string{"test"}, Summary: "The build claim and data loss need fixes", Verdict: "fail", Findings: []structuredreview.Finding{{Severity: "high", Problem: "build claim", Fix: "fix", Sources: []int{1}}, {Severity: "medium", Problem: "data loss", Fix: "fix", Sources: []int{1}}}}
	job := &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha, CIBaseBranch: "main"}
	var log bytes.Buffer
	checked, err := runCICheck(t.Context(), job, ciCheckConfig{Model: "test-model", Reasoning: cfg.CI.CheckReasoning}, doc, agent.CodexSandbox{Upstream: server.URL}, nil, &log)
	require.NoError(t, err, "%s", log.String())
	output := checked.Markdown("")
	assert.NotContains(t, output, "build claim", log.String())
	assert.Contains(t, output, "data loss")
	assert.Equal(t, "1 finding.", checked.Summary)
	assert.Equal(t, structuredreview.VerdictFail, checked.Verdict)
	assert.Contains(t, log.String(), "go build succeeded")
	require.NotEmpty(t, roots, log.String())
	for _, root := range roots {
		assert.NoDirExists(t, root)
	}
	afterRefs, err := exec.CommandContext(t.Context(), "git", "-C", repo.Root, "show-ref").CombinedOutput()
	require.NoError(t, err)
	assert.Equal(t, string(beforeRefs), string(afterRefs))
	afterStatus, err := exec.CommandContext(t.Context(), "git", "-C", repo.Root, "status", "--porcelain").CombinedOutput()
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(string(beforeStatus)), strings.TrimSpace(string(afterStatus)))
}
