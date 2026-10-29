//go:build integration

package daemon

import (
	"encoding/json/v2"
	"fmt"
	"net"
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
	tc := newWorkerTestContext(t, 1)
	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "credential-proxy.key"), []byte("checker-test-key"), 0o600))
	tc.Pool.cfgGetter = NewConfigWatcher(filepath.Join(configDir, "config.toml"), config.DefaultConfig(), nil, nil)
	beforeRefs, err := exec.CommandContext(t.Context(), "git", "-C", tc.TmpDir, "show-ref").CombinedOutput()
	require.NoError(t, err)
	beforeStatus, err := exec.CommandContext(t.Context(), "git", "-C", tc.TmpDir, "status", "--porcelain").CombinedOutput()
	require.NoError(t, err)
	oldRoots, err := filepath.Glob(filepath.Join(os.TempDir(), "roborev-ci-check-*"))
	require.NoError(t, err)
	var roots []string
	listener, err := net.Listen("tcp", "127.0.0.1:8317")
	require.NoError(t, err)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer checker-test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
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
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []struct {
			name  string
			value any
		}{
			{"response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_test"}}},
			{"response.output_item.done", map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "msg_test", "content": []any{map[string]any{"type": "output_text", "text": final}}}}},
			{"response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_test", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}}},
		} {
			raw, err := json.Marshal(event.value)
			assert.NoError(t, err)
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.name, raw)
			assert.NoError(t, err)
		}
	}))
	// Replace the unused listener allocated by httptest before starting.
	require.NoError(t, server.Listener.Close())
	server.Listener = listener
	server.Start()
	defer server.Close()
	run, members, _ := enqueuePanelRun(t, tc, "checker-panel", []memberSpec{{name: "one", agent: "test"}})
	snapshot, err := synthesisJobConfig{Check: &ciCheckConfig{Model: "test-model"}}.encode()
	require.NoError(t, err)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	_, err = tc.DB.Exec("UPDATE review_jobs SET panel_member_config_json = ?, git_ref = ?, ci_base_branch = 'main' WHERE panel_run_uuid = ? AND panel_role = 'synthesis'", snapshot, sha+".."+sha, run)
	require.NoError(t, err)
	doc := structuredreview.Document{SchemaVersion: 3, Summary: "Two findings", Verdict: "fail", Findings: []structuredreview.Finding{{Severity: "high", Problem: "build claim", Fix: "fix"}, {Severity: "medium", Problem: "data loss", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	markMemberRunning(t, tc, members[0].ID)
	require.NoError(t, tc.DB.CompleteJobResult(members[0].ID, "test", storage.ReviewCompletion{Output: doc.Markdown(""), Verdict: storage.VerdictFail, StructuredOutput: raw}))
	job := releaseAndClaimSynthesis(t, tc, run)
	tc.Pool.processSynthesisJob(t.Context(), testWorkerID, job)
	review, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	log, logErr := os.ReadFile(JobLogPath(job.ID))
	require.NoError(t, logErr)
	assert.NotContains(t, string(log), "CI checker failed")
	assert.NotContains(t, review.Output, "build claim", string(log))
	assert.Contains(t, review.Output, "data loss")
	require.NotEmpty(t, roots, string(log))
	for _, root := range roots {
		assert.NoDirExists(t, root)
	}
	afterRefs, err := exec.CommandContext(t.Context(), "git", "-C", tc.TmpDir, "show-ref").CombinedOutput()
	require.NoError(t, err)
	assert.Equal(t, string(beforeRefs), string(afterRefs))
	afterStatus, err := exec.CommandContext(t.Context(), "git", "-C", tc.TmpDir, "status", "--porcelain").CombinedOutput()
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(string(beforeStatus)), strings.TrimSpace(string(afterStatus)))
}
