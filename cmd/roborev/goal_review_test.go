package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
)

func registerGoalReviewPi(t *testing.T) {
	t.Helper()
	original, err := agent.Get("pi")
	require.NoError(t, err)
	executable, err := os.Executable()
	require.NoError(t, err)
	agent.Register(agent.NewPiAgent(executable))
	t.Cleanup(func() { agent.Register(original) })
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte("pi_cmd = "+strconv.Quote(executable)+"\n"), 0o600))
}

func TestGoalReviewDaemonEnqueue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
		status int
	}{
		{"slow capture", false, http.StatusCreated},
		{"caller cancellation", true, http.StatusCreated},
		{"rejected input", false, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
				require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`), 0o600))
				requests := make(chan daemon.EnqueueRequest, 1)
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/ping" {
						newMockRefineState().handlePing(w, r)
						return
					}
					assert.Equal("/api/enqueue", r.URL.Path)
					assert.Equal(http.MethodPost, r.Method)
					assert.Equal("application/json", r.Header.Get("Content-Type"))
					assert.Equal("Bearer 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", r.Header.Get("Authorization"))
					var captured daemon.EnqueueRequest
					assert.NoError(json.NewDecoder(r.Body).Decode(&captured))
					requests <- captured
					if tc.status != http.StatusCreated {
						http.Error(w, "invalid goal selection", tc.status)
						return
					}
					if tc.cancel {
						<-r.Context().Done()
						return
					}
					time.Sleep(31 * time.Second)
					respondJSON(w, http.StatusCreated, storage.ReviewJob{ID: 7, Agent: "claude", JobType: storage.JobTypeGoalReview})
				}))
				originalTransport := http.DefaultTransport
				http.DefaultTransport = server.Client().Transport
				t.Cleanup(func() { http.DefaultTransport = originalTransport })
				patchServerAddr(t, "http://127.0.0.1:7373")
				cmd, output := newTestCmd(t)
				ctx := t.Context()
				if tc.cancel {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, time.Second)
					defer cancel()
				}
				cmd.SetContext(ctx)
				root := t.TempDir()
				err := runGoalReview(cmd, root,
					goalreview.AgentOptions{Agent: "claude", Model: "review-model", Provider: "anthropic", Reasoning: "thorough"},
					false, false, false, new("chosen spec.md"), new("chosen plan.md"))
				require.Len(t, requests, 1)
				assert.Equal(daemon.EnqueueRequest{
					RepoPath: root, ReviewType: config.ReviewTypeGoal,
					Agent: "claude", Model: "review-model", Provider: "anthropic", Reasoning: "thorough",
					SpecFile: new("chosen spec.md"), PlanFile: new("chosen plan.md"),
				}, <-requests)
				switch {
				case tc.cancel:
					require.ErrorIs(t, err, context.DeadlineExceeded)
				case tc.status != http.StatusCreated:
					require.ErrorContains(t, err, "goal review failed: invalid goal selection")
				default:
					require.NoError(t, err)
					assert.Contains(output.String(), "job 7")
				}
			})
		})
	}
}

func goalReviewTestRunner(run func(goalreview.Snapshot, prompt.SnapshotResult) ([]goalreview.Finding, error)) goalReviewRunner {
	return func(_ context.Context, _ agent.Agent, _ string, snapshot goalreview.Snapshot, prepared prompt.SnapshotResult, _ io.Writer) ([]goalreview.Finding, error) {
		return run(snapshot, prepared)
	}
}

func TestGoalReviewCLI(t *testing.T) {
	for _, flags := range [][]string{{"--dirty"}, {"--branch"}, {"--since", "HEAD"}, {"--base", "main"}, {"--sha", "HEAD"}, {"--panel", "none"}, {"--min-severity", "high"}, {"HEAD"}} {
		t.Run(flags[0], func(t *testing.T) {
			repo := NewGitTestRepo(t)
			cmd := reviewCmd()
			cmd.SetArgs(append([]string{"--repo", repo.Dir, "--type", "goal", "--local", "--agent", "test"}, flags...))
			err := cmd.Execute()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "goal")
		})
	}
	t.Run("spec flag only for goal", func(t *testing.T) {
		repo := NewGitTestRepo(t)
		cmd := reviewCmd()
		cmd.SetArgs([]string{"--repo", repo.Dir, "--local", "--spec", "design.md"})
		err := cmd.Execute()
		require.ErrorContains(t, err, "--type goal")
	})
	t.Run("local spec and linked plan", func(t *testing.T) {
		registerGoalReviewPi(t)
		repo := NewGitTestRepo(t)
		spec := "docs/superpowers/specs/feature-design.md"
		plan := "docs/superpowers/plans/feature.md"
		for name, text := range map[string]string{spec: "# Feature\n", plan: "**Spec:** `" + spec + "`\n**Goal:** Feature\n### Task 1: Change\n"} {
			require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(repo.Dir, name)), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, name), []byte(text), 0o600))
		}
		cmd := reviewCmd()
		cmd.SetContext(context.Background())
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		err := runGoalReviewWithRunner(cmd, repo.Dir, goalreview.AgentOptions{Agent: "pi"}, true, false, false, &spec, &plan,
			goalReviewTestRunner(func(snapshot goalreview.Snapshot, _ prompt.SnapshotResult) ([]goalreview.Finding, error) {
				return goalreview.Check(snapshot), nil
			}))
		require.NoError(t, err)
		assert.Contains(t, output.String(), "No issues found.")
	})
}

func TestGoalReviewLocalClaudeUsesConfiguredAPIKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a shell script")
	}
	dataDir := testenv.SetDataDir(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	originalKey := agent.AnthropicAPIKey()
	agent.SetAnthropicAPIKey("")
	t.Cleanup(func() { agent.SetAnthropicAPIKey(originalKey) })

	keyFile := filepath.Join(dataDir, "received-key")
	t.Setenv("GOAL_REVIEW_KEY_FILE", keyFile)
	claude := filepath.Join(dataDir, "claude")
	require.NoError(t, os.WriteFile(claude, []byte(`#!/bin/sh
if [ "$1" = "--help" ]; then
  echo 'usage: claude --tools --bare'
  exit 0
fi
cat >/dev/null
printf '%s' "$ANTHROPIC_API_KEY" > "$GOAL_REVIEW_KEY_FILE"
printf '%s\n' '{"type":"result","result":"{\"findings\":[]}"}'
`), 0o700))
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(
		"claude_code_cmd = "+strconv.Quote(claude)+"\nanthropic_api_key = \"configured-test-key\"\n"), 0o600))
	repo := NewGitTestRepo(t)
	spec := "design.md"
	require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, spec), []byte("# Feature\n"), 0o600))
	cmd, output := newTestCmd(t)
	cmd.SetContext(t.Context())

	err := runGoalReview(cmd, repo.Dir, goalreview.AgentOptions{Agent: "claude"}, true, false, false, &spec, nil)
	require.NoError(t, err)
	key, err := os.ReadFile(keyFile)
	require.NoError(t, err)
	assert.Equal(t, "configured-test-key", string(key))
	assert.Contains(t, output.String(), "No issues found.")
}

func TestGoalReviewSelectionReplacesConfiguredPair(t *testing.T) {
	repo := &config.RepoConfig{GoalReview: config.GoalReviewConfig{SpecFile: new("configured-spec.md"), PlanFile: new("configured-plan.md")}}
	for _, tc := range []struct {
		name       string
		spec, plan *string
		want       goalreview.Selection
	}{
		{"configured", nil, nil, goalreview.Selection{SpecFile: "configured-spec.md", PlanFile: "configured-plan.md"}},
		{"spec override", new("chosen-spec.md"), nil, goalreview.Selection{SpecFile: "chosen-spec.md"}},
		{"plan override", nil, new("chosen-plan.md"), goalreview.Selection{PlanFile: "chosen-plan.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selection, err := goalreview.Select(repo, tc.spec, tc.plan)
			require.NoError(t, err)
			assert.Equal(t, tc.want, selection)
		})
	}
}

func TestGoalReviewCLIAcceptsPromptAboveInlineBudget(t *testing.T) {
	registerGoalReviewPi(t)
	repo := NewGitTestRepo(t)
	spec := "design.md"
	marker := "last complete requirement evidence"
	content := strings.Repeat("complete requirement evidence\n", 500) + marker + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, spec), []byte(content), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, ".roborev.toml"), []byte("max_prompt_size = 1024\n"), 0o600))

	cmd := reviewCmd()
	cmd.SetContext(context.Background())
	var captured string
	err := runGoalReviewWithRunner(cmd, repo.Dir, goalreview.AgentOptions{Agent: "pi"}, true, false, false, &spec, nil,
		goalReviewTestRunner(func(_ goalreview.Snapshot, prepared prompt.SnapshotResult) ([]goalreview.Finding, error) {
			captured = prepared.Prompt
			if prepared.FilePath != "" {
				data, readErr := os.ReadFile(prepared.FilePath)
				require.NoError(t, readErr)
				captured = string(data)
			}
			return nil, nil
		}))
	require.NoError(t, err)
	assert.Contains(t, captured, marker)
	assert.Greater(t, len(captured), 1024)
}

func TestGoalReviewCLIQuietOutsideRepo(t *testing.T) {
	cmd := reviewCmd()
	cmd.SetArgs([]string{"--repo", t.TempDir(), "--type", "goal", "--quiet", "--local", "--agent", "test"})
	require.Error(t, cmd.Execute(), "invalid intent inputs cannot silently pass")
}

func TestGoalReviewCLIFindingsAndMalformedOutput(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprint(malformed), func(t *testing.T) {
			registerGoalReviewPi(t)
			repo := NewGitTestRepo(t)
			spec := "design.md"
			plan := "plan.md"
			require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, spec), []byte("# Feature\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, plan), []byte("### Task 1: Change\n"), 0o600))
			cmd := reviewCmd()
			cmd.SetContext(context.Background())
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			err := runGoalReviewWithRunner(cmd, repo.Dir, goalreview.AgentOptions{Agent: "pi"}, true, false, false, &spec, &plan,
				goalReviewTestRunner(func(snapshot goalreview.Snapshot, _ prompt.SnapshotResult) ([]goalreview.Finding, error) {
					if malformed {
						return nil, fmt.Errorf("findings array is malformed")
					}
					return goalreview.Check(snapshot), nil
				}))
			if malformed {
				require.ErrorContains(t, err, "findings array")
			} else {
				var exit *exitError
				require.ErrorAs(t, err, &exit)
				assert.Equal(t, 1, exit.code)
				assert.Contains(t, output.String(), "medium:")
			}
		})
	}
}
