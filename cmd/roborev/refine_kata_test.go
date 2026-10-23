package main

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/katacontext"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestRefineWithoutPlanPreparesCompleteKataContext(t *testing.T) {
	originalUnsafe := agent.AllowUnsafeAgents()
	t.Cleanup(func() { agent.SetAllowUnsafeAgents(originalUnsafe) })
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n", ".roborev.toml": "max_prompt_size = 1000\n"})
	base := repo.HeadSHA()
	head := repo.CommitFile("next.go", "package source\n", "Add source")
	job := storage.ReviewJob{ID: 7, GitRef: head, Status: storage.JobStatusDone}
	reviewUUID := uuid.New()
	review := storage.Review{ID: 7, JobID: 7, Job: &job, UUID: &reviewUUID, Output: "High: missing cancellation", VerdictBool: testutil.ReviewFixtureVerdict("High: missing cancellation")}
	claim := "Validate this attributed claim independently: " + strings.Repeat("evidence ", 1000)
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/review", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, review) }).
		WithHandler("/api/comments", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"responses": []storage.Response{}})
		}).
		WithHandler("/api/comment", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) }).
		WithHandler("/api/review/close", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]any{}) }).
		WithHandler("/api/jobs", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{}})
		}).
		WithHandler("/api/kata/context", func(w http.ResponseWriter, r *http.Request) {
			var request daemon.KataConsumeContextRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &request))
			writeJSON(w, katacontext.KataReplyContext{JobID: 7, RunID: request.RunID, Snapshots: []katacontext.Snapshot{{
				ReviewUUID: reviewUUID.String(), Findings: []katacontext.Finding{{Selector: "f:example", Replies: []kata.Comment{{Body: claim, Author: "reviewer"}}}}, //nolint:forbidigo // Synthetic review UUID at the daemon API boundary.
			}}})
		}).Build()
	var promptPath string
	calls := 0
	name := "refine-kata-transport-test"
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, dir, _, p string, _ io.Writer) (string, error) {
		calls++
		const prefix = "Read the complete task prompt from "
		require.True(t, strings.HasPrefix(p, prefix))
		quoted, _, _ := strings.Cut(strings.TrimPrefix(p, prefix), " and carry out")
		var err error
		promptPath, err = strconv.Unquote(quoted)
		require.NoError(t, err)
		body, err := os.ReadFile(promptPath)
		require.NoError(t, err)
		assert.Contains(t, string(body), claim)
		assert.Contains(t, string(body), "reviewer")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "source.go"), []byte("package source\n// cancellation checked\n"), 0o600))
		return "Add cancellation check", nil
	}})
	var runErr error
	captureOutput(t, func() error {
		runErr = runRefine(RunContext{Context: t.Context(), WorkingDir: repo.Dir, PostCommitDelay: time.Nanosecond}, refineOptions{since: base, agentName: name, maxIterations: 1, quiet: true, unsafeFlagChanged: true})
		return nil
	})
	require.ErrorContains(t, runErr, "max iterations")
	assert.Equal(t, 1, calls)
	assert.NoFileExists(t, promptPath)
}
