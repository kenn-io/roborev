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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
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
	var schemas, checkouts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		var err error
		schemas, err = filepath.Glob(filepath.Join(checkoutPattern, "roborev-ci-check-*", "work", "roborev-codex-review-schema-*.json"))
		assert.NoError(t, err)
		checkouts, err = filepath.Glob(checkoutPattern)
		assert.NoError(t, err)
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
	assert.NotEmpty(t, schemas, "schema must live in the sandbox work directory")
	stored, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.NotContains(t, stored.Output, "build claim", string(log))
	assert.Contains(t, stored.Output, "data loss")
}
