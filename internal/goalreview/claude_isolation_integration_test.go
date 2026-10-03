//go:build integration

package goalreview

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/agent"
)

func TestClaudeGoalReviewSuppressesRepositoryHooks(t *testing.T) {
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("Claude CLI is required for the local transport isolation check")
	}
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".claude"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "auth"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".claude", "settings.json"), []byte(`{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"echo hook > hook-ran"}]}]}}`), 0600))
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case received <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"Synthetic credential rejected"}}`)
	}))
	defer server.Close()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "auth"))
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-key")
	t.Setenv("ROBOREV_CLAUDE_PROXY_TOKEN", "synthetic-key")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_SAFE_MODE", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	snapshot := Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}}}
	_, err = Run(ctx, agent.NewClaudeAgent(binary).WithModel("synthetic@"+server.URL), root, snapshot, io.Discard)
	require.Error(t, err, "the synthetic provider deliberately rejects credentials")
	assert.NotEmpty(t, received, "Claude must reach the local provider so startup isolation is exercised")
	assert.NoFileExists(t, filepath.Join(root, "hook-ran"), "goal critique must not execute repository lifecycle hooks")
}
