package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/prompt"
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

func TestGoalReviewEnqueueClientDoesNotAddFixedTimeout(t *testing.T) {
	ep, err := daemon.ParseEndpoint("127.0.0.1:7373")
	require.NoError(t, err)
	assert.Zero(t, goalReviewDaemonHTTPClient(ep).Timeout)
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
