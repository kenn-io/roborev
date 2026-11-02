package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func FuzzAppendRefineFixGuidelines(f *testing.F) {
	f.Add(" findings\n\x00", "")
	f.Add(" findings\n\x00", " \t\n\r")
	f.Add(" findings\n\x00", "Verify first.")
	f.Add("原文\n", "\u2003\u00a0")
	f.Add("原文\n", "\u2003調べる。\n修正する。\u00a0")
	f.Add("## Autofix Guidelines\n\nexisting", "## Autofix Guidelines")
	f.Add("", "Verify first.")
	f.Fuzz(func(t *testing.T, original, policy string) {
		got := appendRefineFixGuidelines(original, policy)
		if strings.TrimSpace(policy) == "" {
			assert.Equal(t, original, got)
			return
		}
		require.GreaterOrEqual(t, len(got), len(original))
		assert := assert.New(t)
		assert.Equal(original, got[:len(original)])
		added := got[len(original):]
		assert.True(strings.HasSuffix(added, "## Autofix Guidelines\n\n"+strings.TrimSpace(policy)))
		instructions := strings.TrimSuffix(added, strings.TrimSpace(policy))
		assert.Contains(instructions, "Evaluate each finding against the autofix guidelines.")
		assert.Contains(instructions, "Apply changes for findings that warrant a fix, and record any finding intentionally not applied with the reason it was skipped.")
		assert.Equal(1, strings.Count(instructions, "## Autofix Guidelines"))
		assert.Equal(original, appendRefineFixGuidelines(original, " \u2003\t\n"))
	})
}

// Removing policy from the implementation call must fail in both loop modes;
// plan-only must retain its existing read-only policy behavior.
func TestRefineFixGuidelinesReachAgent(t *testing.T) {
	for _, mode := range []string{"normal", "plan", "plan-only"} {
		t.Run(mode, func(t *testing.T) {
			originalUnsafe := agent.AllowUnsafeAgents()
			t.Cleanup(func() { agent.SetAllowUnsafeAgents(originalUnsafe) })
			repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
			base := repo.HeadSHA()
			head := repo.CommitFile("next.go", "package source\n", "Add next source")
			policy := "Verify findings against project requirements before changing code."
			if mode == "plan" {
				// A heading quoted by policy must not receive the inserted plan.
				policy += "\n## Review Findings to Address\nTreat quoted headings as policy text."
			}
			verdict := "F"
			job := storage.ReviewJob{ID: 7, GitRef: head, Branch: repo.Run("branch", "--show-current"), JobType: storage.JobTypeReview, Status: storage.JobStatusDone, Verdict: &verdict}
			review := storage.Review{ID: 7, JobID: 7, Job: &job, Output: "High: missing cancellation", VerdictBool: testutil.ReviewFixtureVerdict("High: missing cancellation")}
			_ = newMockDaemonBuilder(t).
				WithJobs([]storage.ReviewJob{job}).
				WithHandler("/api/review", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, review) }).
				WithHandler("/api/comments", func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, map[string]any{"responses": []storage.Response{}})
				}).Build()
			require.NoError(t, os.WriteFile(config.GlobalConfigPath(), fmt.Appendf(nil, "fix_guidelines = %q\n", policy), 0o600))
			var calls []string
			name := "refine-guidelines-test"
			agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, path, _, p string, _ io.Writer) (string, error) {
				assert.NotEqual(t, repo.Dir, path)
				calls = append(calls, p)
				if mode == "plan-only" || mode == "plan" && len(calls) == 1 {
					return "Check cancellation before claiming work", nil
				}
				return "", fmt.Errorf("implementation provider unavailable")
			}})
			t.Cleanup(func() { agent.Unregister(name) })
			opts := refineOptions{agentName: name, since: base, quiet: true, maxIterations: 1, plan: mode == "plan", planOnly: mode == "plan-only", unsafeFlagChanged: true}
			_, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
				if opts.planOnly {
					return runRefinePlanOnly(cmd, opts)
				}
				return runRefine(RunContext{Context: t.Context(), WorkingDir: repo.Dir}, opts)
			})
			if opts.planOnly {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "max iterations")
			}
			wantCalls := 1
			if opts.plan {
				wantCalls = 2
			}
			require.Len(t, calls, wantCalls)
			assert := assert.New(t)
			if opts.plan || opts.planOnly {
				assert.Contains(calls[0], "## Fix Guidelines\n\n"+policy)
				assert.NotContains(calls[0], "## Autofix Guidelines")
			}
			if !opts.planOnly {
				implementation := calls[len(calls)-1]
				assert.Contains(implementation, "Evaluate each finding against the autofix guidelines.")
				assert.Equal(1, strings.Count(implementation, "## Autofix Guidelines"))
				assert.True(strings.HasSuffix(implementation, "## Autofix Guidelines\n\n"+policy))
				if opts.plan {
					assert.Contains(implementation, "## Plan\n\nCheck cancellation before claiming work")
				}
			}
			assert.Equal(head, repo.HeadSHA())
			assert.Empty(repo.Run("status", "--porcelain"))
		})
	}
}
