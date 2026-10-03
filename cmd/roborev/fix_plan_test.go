package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func TestFixPlanFlags(t *testing.T) {
	for _, args := range [][]string{{"--plan", "--list"}, {"--plan-only", "--list"}, {"--plan-only", "--resume"}} {
		cmd := fixCmd()
		cmd.SetArgs(args)
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		err := cmd.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot be used")
		assert.NotContains(t, err.Error(), "unknown flag")
	}
}

func newPlanTestRepo(t *testing.T, files map[string]string) *TestGitRepo {
	t.Helper()
	repo := createTestRepo(t, files)
	repo.Run("config", "core.autocrlf", "false")
	return repo
}

func TestFixPlanSingleJob(t *testing.T) {
	for _, only := range []bool{false, true} {
		t.Run(map[bool]string{false: "implement", true: "plan-only"}[only], func(t *testing.T) {
			repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
			before := repo.Run("rev-parse", "HEAD")
			ts, state := newMockServer(t, MockServerOpts{
				ReviewOutput: "High: cancellation not checked",
				OnJobs: func(w http.ResponseWriter, r *http.Request) {
					writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 7, Status: storage.JobStatusDone, Agent: "test"}}})
				},
			})
			patchServerAddr(t, ts.URL)
			var prompts []string
			a := &agent.FakeAgent{NameStr: "test", ReviewFn: func(_ context.Context, _, _, p string, _ io.Writer) (string, error) {
				prompts = append(prompts, p)
				return "Check cancellation before claiming work", nil
			}}
			cmd, out := newTestCmd(t)
			err := fixSingleJob(cmd, repo.Dir, 7, fixOptions{plan: true, planOnly: only, quiet: only}, &fixSessionTracker{base: a, out: io.Discard})
			require.NoError(t, err)
			require.NotEmpty(t, prompts)
			assert.Contains(t, prompts[0], "Fix Plan Request")
			assert.Contains(t, prompts[0], "cancellation not checked")
			if only {
				assert.Len(t, prompts, 1)
				assert.Zero(t, state.Closes())
				assert.EqualValues(t, 1, state.Comments())
				assert.Contains(t, out.String(), "Check cancellation before claiming work")
			} else {
				require.Len(t, prompts, 2)
				assert.Contains(t, prompts[1], "## Plan\n\nCheck cancellation before claiming work")
				assert.EqualValues(t, 1, state.Closes())
				assert.EqualValues(t, 2, state.Comments())
			}
			assert.Zero(t, state.Enqueues())
			assert.Equal(t, before, repo.Run("rev-parse", "HEAD"))
			assert.Empty(t, repo.Run("status", "--porcelain"))
		})
	}
}

func TestFixPlanOnlyDoesNotClosePassingReview(t *testing.T) {
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	verdict := "P"
	ts, state := newMockServer(t, MockServerOpts{ReviewOutput: "No issues found.", OnJobs: func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 7, Status: storage.JobStatusDone, Verdict: &verdict}}})
	}})
	patchServerAddr(t, ts.URL)
	cmd, _ := newTestCmd(t)
	err := fixSingleJob(cmd, repo.Dir, 7, fixOptions{planOnly: true}, &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard})
	require.NoError(t, err)
	assert.Zero(t, state.Closes())
	assert.Zero(t, state.Comments())
}

func TestFixPlanOnlyDiscoveryIsFinite(t *testing.T) {
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	verdict := "F"
	queries := 0
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("id") == "" {
				queries++
			}
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 7, Status: storage.JobStatusDone, Agent: "test", Branch: strings.TrimSpace(repo.Run("branch", "--show-current")), Verdict: &verdict}}})
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, storage.Review{JobID: 7, Output: "High: cancellation not checked"})
		}).Build()
	_, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		return runFixOpen(cmd, "", false, false, false, fixOptions{planOnly: true, quiet: true}, &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard})
	})
	require.NoError(t, err)
	assert.Equal(t, 1, queries)
}

func TestFixPlanRejectsCallerChanges(t *testing.T) {
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	ts, state := newMockServer(t, MockServerOpts{ReviewOutput: "High: missing check", OnJobs: func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 7, Status: storage.JobStatusDone, Agent: "test"}}})
	}})
	patchServerAddr(t, ts.URL)
	calls := 0
	a := &agent.FakeAgent{NameStr: "test", ReviewFn: func(_ context.Context, _, _, _ string, _ io.Writer) (string, error) {
		calls++
		require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, "source.go"), []byte("concurrent change\n"), 0o600))
		return "Add cancellation check", nil
	}}
	cmd, _ := newTestCmd(t)
	err := fixSingleJob(cmd, repo.Dir, 7, fixOptions{plan: true}, &fixSessionTracker{base: a, out: io.Discard})
	require.ErrorContains(t, err, "changed during planning")
	assert.Equal(t, 1, calls)
	assert.Zero(t, state.Closes())
	assert.Zero(t, state.Enqueues())
}

func TestFixPlanOnlyBatch(t *testing.T) {
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	refs := map[int64]string{
		7: repo.CommitFile("old.go", "package source\n// historical cancellation path seven\n", "Add first path"),
		8: repo.CommitFile("old.go", "package source\n// historical cancellation path eight\n", "Change path"),
	}
	repo.CommitFile("old.go", "package source\n", "Remove old path")
	var calls []string
	a := &agent.FakeAgent{NameStr: "test", ReviewFn: func(_ context.Context, _, _, p string, _ io.Writer) (string, error) {
		calls = append(calls, p)
		return "Fix both cancellation paths", nil
	}}
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			id := int64(7)
			if r.URL.Query().Get("id") == "8" {
				id = 8
			}
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{ID: id, GitRef: refs[id], Status: storage.JobStatusDone, Agent: "test"}}})
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, storage.Review{Output: "High: cancellation missing " + r.URL.Query().Get("job_id")})
		}).
		WithHandler("/api/comments", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"responses": []storage.Response{{Responder: "developer", Response: "Scope comment " + r.URL.Query().Get("job_id")}}})
		}).Build()
	out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		return runFixBatch(cmd, []int64{7, 8}, "", false, false, false, 2, fixOptions{planOnly: true, quiet: true}, &fixSessionTracker{base: a, out: io.Discard})
	})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0], "Job 7")
	assert.Contains(t, calls[0], "Job 8")
	assert.Contains(t, calls[0], refs[7])
	assert.Contains(t, calls[0], refs[8])
	assert.Contains(t, calls[0], "historical cancellation path seven")
	assert.Contains(t, calls[0], "historical cancellation path eight")
	assert.Less(t, strings.Index(calls[0], "Job 7"), strings.Index(calls[0], "Scope comment 7"))
	assert.Less(t, strings.Index(calls[0], "Scope comment 7"), strings.Index(calls[0], "Job 8"))
	assert.Contains(t, out, "Fix both cancellation paths")
	assert.Empty(t, repo.Run("status", "--porcelain"))
}

func TestFixPlanFailureStopsImplementation(t *testing.T) {
	for _, mode := range []string{"empty", "error"} {
		t.Run(mode, func(t *testing.T) {
			repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
			ts, state := newMockServer(t, MockServerOpts{})
			patchServerAddr(t, ts.URL)
			calls := 0
			a := &agent.FakeAgent{NameStr: "test", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
				calls++
				if mode == "empty" {
					return "", nil
				}
				if mode == "error" {
					return "", fmt.Errorf("provider temporarily unavailable")
				}
				return strings.Repeat("step ", 300), nil
			}}
			cfg := config.DefaultConfig()
			cfg.DefaultMaxPromptSize = 1000
			planning, implementation := "Analyze cancellation", "## Review Findings\n\nMissing cancellation check"
			cmd, _ := newTestCmd(t)
			_, err := planFix(context.Background(), cmd, repo.Dir, a, planning, implementation, []int64{7}, fixOptions{plan: true}, cfg)
			require.Error(t, err)
			assert.Equal(t, 1, calls)
			assert.Zero(t, state.Closes())
			assert.Zero(t, state.Enqueues())
			assert.Empty(t, repo.Run("status", "--porcelain"))
		})
	}
}

func TestFixPlanCompletePromptTransport(t *testing.T) {
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, ".roborev.toml"), []byte("max_prompt_size = 1000\n"), 0o600))
	repo.Run("add", ".roborev.toml")
	repo.Run("commit", "-m", "Set prompt transport threshold")
	findings := "High: cancellation not checked " + strings.Repeat("context ", 500)
	plan := strings.TrimSpace("Check cancellation before claiming work " + strings.Repeat("step ", 500))
	ts, _ := newMockServer(t, MockServerOpts{ReviewOutput: findings, OnJobs: func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 7, Status: storage.JobStatusDone, Agent: "test"}}})
	}})
	patchServerAddr(t, ts.URL)
	var paths []string
	calls := 0
	a := &agent.FakeAgent{NameStr: "test", ReviewFn: func(_ context.Context, dir, ref, p string, _ io.Writer) (string, error) {
		calls++
		const prefix = "Read the complete task prompt from "
		require.True(t, strings.HasPrefix(p, prefix))
		quoted, _, _ := strings.Cut(strings.TrimPrefix(p, prefix), " and carry out")
		path, err := strconv.Unquote(quoted)
		require.NoError(t, err)
		rel, err := filepath.Rel(dir, path)
		require.NoError(t, err)
		assert.True(t, filepath.IsLocal(rel))
		paths = append(paths, path)
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Contains(t, string(body), findings)
		if calls == 1 {
			return plan, nil
		}
		assert.Contains(t, string(body), "## Plan\n\n"+plan)
		return "No changes required", nil
	}}
	cmd, _ := newTestCmd(t)
	require.NoError(t, fixSingleJob(cmd, repo.Dir, 7, fixOptions{plan: true}, &fixSessionTracker{base: a, out: io.Discard}))
	assert.Equal(t, 2, calls)
	for _, path := range paths {
		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	assert.Empty(t, repo.Run("status", "--porcelain"))
}
