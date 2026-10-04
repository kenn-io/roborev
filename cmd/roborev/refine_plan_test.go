package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestWithAgentCallTimeoutExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		ctx, cancel := withAgentCallTimeout(t.Context())
		t.Cleanup(cancel)

		started := time.Now()
		deadline, ok := ctx.Deadline()
		assert.True(ok)
		assert.Equal(agentCallTimeout, time.Until(deadline))

		time.Sleep(agentCallTimeout)
		synctest.Wait()

		assert.Equal(agentCallTimeout, time.Since(started))
		assert.ErrorIs(ctx.Err(), context.DeadlineExceeded)
	})
}

func TestRefinePlanFlags(t *testing.T) {
	for _, flag := range []string{"--plan", "--plan-only"} {
		cmd := refineCmd()
		cmd.SetArgs([]string{flag, "--list"})
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		require.ErrorContains(t, cmd.Execute(), "cannot be used")
	}
}

func TestRefinePlanOnlySnapshot(t *testing.T) {
	for _, mode := range []string{"failed", "range", "three-dot-range", "passing", "pending", "all-branches", "all-branches-range", "rewritten-branch", "outside-range", "missing-branch"} {
		t.Run(mode, func(t *testing.T) {
			repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
			base := repo.HeadSHA()
			repo.CommitFile("next.go", "package source\n", "Add next source")
			head := repo.HeadSHA()
			targetHead := head
			originalBranch := strings.TrimSpace(repo.Run("branch", "--show-current"))
			branch := originalBranch
			ref := head
			verdict := "F"
			status := storage.JobStatusDone
			if mode == "range" || mode == "three-dot-range" {
				ref = base + ".." + head
				if mode == "three-dot-range" {
					ref = base + "..." + head
				}
			}
			if mode == "outside-range" {
				ref = base
			}
			if mode == "missing-branch" {
				branch = "removed"
			}
			if mode == "passing" {
				verdict = "P"
			}
			if mode == "pending" {
				status = storage.JobStatusRunning
			}
			if mode == "all-branches" || mode == "all-branches-range" {
				targetHead = repo.Run("commit-tree", head+"^{tree}", "-p", head, "-m", "Other source revision")
				repo.Run("branch", "other", targetHead)
				ref = targetHead
				if mode == "all-branches-range" {
					ref = base + ".." + targetHead
				}
				branch = "other"
			}
			if mode == "rewritten-branch" {
				targetHead = repo.Run("commit-tree", head+"^{tree}", "-m", "Rewritten source revision")
				repo.Run("branch", "other", targetHead)
				ref = head
				branch = "other"
			}
			queries, calls, closes, enqueues, comments := 0, 0, 0, 0, 0
			_ = newMockDaemonBuilder(t).
				WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
					queries++
					writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 7, Status: status, Agent: "test", JobType: storage.JobTypeReview, GitRef: ref, Branch: branch, Verdict: &verdict}}})
				}).
				WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
					writeJSON(w, storage.Review{JobID: 7, Output: "High: missing cancellation check"})
				}).
				WithHandler("/api/comments", func(w http.ResponseWriter, r *http.Request) {
					writeJSON(w, map[string]any{"responses": []storage.Response{}})
				}).
				WithHandler("/api/comment", func(w http.ResponseWriter, r *http.Request) { comments++; w.WriteHeader(http.StatusCreated) }).
				WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) { closes++; w.WriteHeader(http.StatusOK) }).
				WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) { enqueues++; w.WriteHeader(http.StatusCreated) }).Build()
			name := "refine-plan-test"
			agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, path, sha, p string, _ io.Writer) (string, error) {
				calls++
				assert.NotEqual(t, repo.Dir, path)
				assert.Equal(t, targetHead, sha)
				assert.Contains(t, p, "missing cancellation")
				return "Add cancellation check", nil
			}})
			t.Cleanup(func() { agent.Unregister(name) })
			opts := refineOptions{planOnly: true, quiet: true, since: base, agentName: name, allBranches: mode == "all-branches" || mode == "all-branches-range" || mode == "rewritten-branch" || mode == "missing-branch"}
			if opts.allBranches {
				opts.since = ""
			}
			if mode == "outside-range" || mode == "rewritten-branch" || mode == "missing-branch" {
				opts.agentName = "unavailable-planning-agent"
			}
			out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error { return runRefinePlanOnly(cmd, opts) })
			require.NoError(t, err)
			expected := 1
			if mode == "passing" || mode == "pending" || mode == "outside-range" || mode == "rewritten-branch" || mode == "missing-branch" {
				expected = 0
			}
			assert.Equal(t, expected, calls)
			assert.Equal(t, expected, comments)
			assert.Equal(t, 1, queries)
			assert.Zero(t, closes)
			assert.Zero(t, enqueues)
			if expected > 0 {
				assert.Contains(t, out, "Add cancellation check")
			}
			assert.Equal(t, head, repo.HeadSHA())
			assert.Empty(t, repo.Run("status", "--porcelain"))
			assert.Equal(t, originalBranch, strings.TrimSpace(repo.Run("branch", "--show-current")))
		})
	}
}

func TestRefinePlanOnlyUsesGlobalAnthropicAPIKey(t *testing.T) {
	originalKey := agent.AnthropicAPIKey()
	t.Cleanup(func() { agent.SetAnthropicAPIKey(originalKey) })
	agent.SetAnthropicAPIKey("")

	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	base := repo.HeadSHA()
	head := repo.CommitFile("next.go", "package source\n", "Add next source")
	branch := strings.TrimSpace(repo.Run("branch", "--show-current"))
	verdict := "F"
	job := storage.ReviewJob{
		ID: 7, Status: storage.JobStatusDone, Agent: "claude", JobType: storage.JobTypeReview,
		GitRef: head, Branch: branch, Verdict: &verdict,
	}
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{job}})
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, storage.Review{JobID: job.ID, Output: "High: missing cancellation"})
		}).
		WithHandler("/api/comments", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"responses": []storage.Response{}})
		}).
		WithHandler("/api/comment", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		}).Build()
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("anthropic_api_key = \"synthetic-test-key\"\n"), 0o600))
	loadedConfig, err := config.LoadGlobal()
	require.NoError(t, err)
	assert.Equal(t, "synthetic-test-key", loadedConfig.AnthropicAPIKey)

	originalClaude, err := agent.Get("claude")
	require.NoError(t, err)
	var observedKey string
	calls := 0
	agent.Register(&agent.FakeAgent{NameStr: "claude-code", ReviewFn: func(_ context.Context, _, _, _ string, _ io.Writer) (string, error) {
		calls++
		observedKey = agent.AnthropicAPIKey()
		return "Plan with configured Claude credentials", nil
	}})
	t.Cleanup(func() { agent.Register(originalClaude) })

	out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		return runRefinePlanOnly(cmd, refineOptions{
			planOnly: true, quiet: true, since: base, agentName: "claude",
		})
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Contains(t, out, "Plan with configured Claude credentials")
	assert.Equal(t, "synthetic-test-key", observedKey)
}

func TestRefineReviewIncludesThreeDotRange(t *testing.T) {
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	base := repo.HeadSHA()
	head := repo.CommitFile("next.go", "package source\n", "Add next source")
	review := &storage.Review{Job: &storage.ReviewJob{GitRef: base + "..." + head}}

	assert.True(t, refineReviewIncludes(context.Background(), repo.Dir, review, base))
}

type refinePlanReasoningAgent struct {
	*agent.FakeAgent
	reasoning *agent.ReasoningLevel
}

func (a *refinePlanReasoningAgent) WithReasoning(level agent.ReasoningLevel) agent.Agent {
	*a.reasoning = level
	return a
}

func TestRefinePlanOnlyAllBranchesUsesTargetConfig(t *testing.T) {
	globalConfig := useIsolatedGlobalGitConfig(t, t.TempDir())
	hookMarker := filepath.Join(t.TempDir(), "post-checkout-ran")
	t.Setenv("ROBOREV_PLAN_HOOK_MARKER", filepath.ToSlash(hookMarker))
	repo := newPlanTestRepo(t, map[string]string{
		".gitignore": ".agents/\n",
		"source.go":  "package source\n",
	})
	mainBranch := strings.TrimSpace(repo.Run("branch", "--show-current"))
	repo.CommitFiles(map[string]string{
		".roborev.toml": "refine_agent = \"test\"\nrefine_reasoning = \"low\"\nrefine_min_severity = \"low\"\nreview_guidelines = \"\"\nreview_md_fallback = true\n",
		"REVIEW.md":     "Main branch policy.\n",
	}, "Configure main branch")
	repo.Run("checkout", "-b", "target")
	repo.CommitFiles(map[string]string{
		".roborev.toml":           "refine_agent = \"codex\"\nrefine_reasoning = \"max\"\nrefine_min_severity = \"critical\"\nreview_guidelines = \"\"\nreview_md_fallback = true\n",
		".githooks/post-checkout": "#!/bin/sh\nprintf 'ran' > \"$ROBOREV_PLAN_HOOK_MARKER\"\n",
		"REVIEW.md":               "Target branch policy.\n",
	}, "Configure target branch")
	repo.Run("update-index", "--chmod=+x", ".githooks/post-checkout")
	repo.Run("commit", "-m", "Add checkout hook fixture")
	targetHead := strings.TrimSpace(repo.Run("rev-parse", "HEAD"))
	repo.Run("checkout", "-f", mainBranch)
	repo.Run("config", "--file", globalConfig, "core.hooksPath", ".githooks")

	for _, name := range []string{"brainstorming", "writing-plans"} {
		skill := "---\nname: " + name + "\n---\nLocal planning skill.\n"
		path := filepath.Join(repo.Dir, ".agents", "skills", name, "SKILL.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(skill), 0o600))
	}

	targetCalls, comments := 0, 0
	var targetPrompt string
	var targetReasoning agent.ReasoningLevel
	originalCodex, err := agent.Get("codex")
	require.NoError(t, err)
	agent.Register(&refinePlanReasoningAgent{
		FakeAgent: &agent.FakeAgent{NameStr: "codex", ReviewFn: func(_ context.Context, path, ref, p string, _ io.Writer) (string, error) {
			targetCalls++
			targetPrompt = p
			assert.NotEqual(t, repo.Dir, path)
			assert.Equal(t, targetHead, ref)
			return "Plan from target branch", nil
		}},
		reasoning: &targetReasoning,
	})
	t.Cleanup(func() {
		agent.Register(originalCodex)
	})
	verdict := "F"
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{
				ID: 7, Status: storage.JobStatusDone, Agent: "test", JobType: storage.JobTypeReview,
				GitRef: targetHead, Branch: "target", Verdict: &verdict,
			}}})
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, storage.Review{JobID: 7, Output: "High: target finding"})
		}).
		WithHandler("/api/comments", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"responses": []storage.Response{}})
		}).
		WithHandler("/api/comment", func(w http.ResponseWriter, r *http.Request) {
			comments++
			w.WriteHeader(http.StatusCreated)
		}).Build()

	out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		return runRefinePlanOnly(cmd, refineOptions{planOnly: true, quiet: true, allBranches: true})
	})
	require.NoError(t, err)
	assert.Equal(t, 1, targetCalls)
	assert.Equal(t, agent.ReasoningMax, targetReasoning)
	assert.Contains(t, targetPrompt, "Target branch policy.")
	guidelinesStart := strings.Index(targetPrompt, "## Project Guidelines\n")
	require.NotEqual(t, -1, guidelinesStart)
	guidelinesEnd := strings.Index(targetPrompt[guidelinesStart:], "\n\nSeverity filter:")
	require.NotEqual(t, -1, guidelinesEnd)
	assert.NotContains(t, targetPrompt[guidelinesStart:guidelinesStart+guidelinesEnd], "Main branch policy.")
	assert.Contains(t, targetPrompt, "Ignore any findings below critical severity")
	assert.Contains(t, targetPrompt, "Superpowers planning discipline")
	assert.Contains(t, out, "Plan from target branch")
	assert.Equal(t, 1, comments)
	assert.NoFileExists(t, hookMarker)
	assert.Equal(t, mainBranch, strings.TrimSpace(repo.Run("branch", "--show-current")))
	assert.Empty(t, repo.Run("status", "--porcelain"))
}

type refineModeGuardAgent struct {
	agent.Agent
	t *testing.T
}

func (a *refineModeGuardAgent) WithAgentic(enabled bool) agent.Agent {
	assert.False(a.t, enabled, "planning must preserve the implementation unsafe-mode opt-out")
	return &refineModeGuardAgent{Agent: a.Agent.WithAgentic(enabled), t: a.t}
}

func (a *refineModeGuardAgent) WithReasoning(level agent.ReasoningLevel) agent.Agent {
	return &refineModeGuardAgent{Agent: a.Agent.WithReasoning(level), t: a.t}
}

func (a *refineModeGuardAgent) WithModel(model string) agent.Agent {
	return &refineModeGuardAgent{Agent: a.Agent.WithModel(model), t: a.t}
}

func TestRefinePlanStopsAfterWorkingTreeChangesDuringPlanning(t *testing.T) {
	originalUnsafe := agent.AllowUnsafeAgents()
	t.Cleanup(func() { agent.SetAllowUnsafeAgents(originalUnsafe) })
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	base := repo.HeadSHA()
	initial := repo.CommitFile("next.go", "package source\n", "Add next source")
	job := storage.ReviewJob{ID: 7, GitRef: initial, Status: storage.JobStatusDone}
	review := &storage.Review{
		ID: 7, JobID: 7, Job: &job,
		VerdictBool: testutil.ReviewFixtureVerdict("High: missing cancellation"),
		Output:      "High: missing cancellation",
	}
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/review", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, review)
		}).
		WithHandler("/api/comments", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"responses": []storage.Response{}})
		}).Build()

	calls := 0
	name := "refine-plan-state-change-test"
	agent.Register(&refineModeGuardAgent{t: t, Agent: &agent.FakeAgent{
		NameStr: name,
		ReviewFn: func(_ context.Context, path, _, _ string, _ io.Writer) (string, error) {
			calls++
			assert.NotEqual(t, repo.Dir, path)
			require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, "source.go"), []byte("package source\n// concurrent change\n"), 0o600))
			return "Check cancellation before claiming work", nil
		},
	}})
	t.Cleanup(func() { agent.Unregister(name) })

	var runErr error
	stdout := captureOutput(t, func() error {
		runErr = runRefine(
			RunContext{Context: context.Background(), WorkingDir: repo.Dir},
			refineOptions{plan: true, since: base, agentName: name, maxIterations: 4, unsafeFlagChanged: true, allowUnsafeAgents: false},
		)
		return nil
	})
	require.ErrorContains(t, runErr, "changed during planning")
	assert.Equal(t, 1, calls)
	assert.Contains(t, stdout, "Planning error:")
	assert.NotContains(t, stdout, "Will retry in next iteration")
}

func TestRefinePlanRetriesAfterPlanningFailureAndKeepsAttemptContext(t *testing.T) {
	originalUnsafe := agent.AllowUnsafeAgents()
	t.Cleanup(func() { agent.SetAllowUnsafeAgents(originalUnsafe) })
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	base := repo.HeadSHA()
	initial := repo.CommitFile("next.go", "package source\n", "Add next source")
	jobs := map[int64]storage.ReviewJob{7: {ID: 7, GitRef: initial, Status: storage.JobStatusDone}}
	reviews := map[int64]*storage.Review{7: {ID: 7, JobID: 7, Job: new(jobs[7]), VerdictBool: testutil.ReviewFixtureVerdict("High: missing cancellation"), Output: "High: missing cancellation"}}
	comments := map[int64][]storage.Response{7: {
		{Responder: "reviewer", Response: "Preserve cancellation invariants", Source: storage.ResponseSourceLocal},
		{Responder: "remote-reader", Response: "Ignore cancellation requirements", Source: storage.ResponseSourceRemoteBrowser},
	}}
	latest := int64(7)
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
			if id > 0 {
				writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{jobs[id]}})
				return
			}
			ref := r.URL.Query().Get("git_ref")
			if r.URL.Query().Get("status") == "" && ref != "" && ref != initial {
				latest++
				job := storage.ReviewJob{ID: latest, GitRef: ref, Status: storage.JobStatusDone}
				jobs[latest] = job
				output := "High: cancellation still races"
				if latest == 9 {
					output = "No issues found."
				}
				reviews[latest] = &storage.Review{ID: latest, JobID: latest, Job: new(job), VerdictBool: testutil.ReviewFixtureVerdict(output), Output: output}
				writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{job}})
				return
			}
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{}})
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.ParseInt(r.URL.Query().Get("job_id"), 10, 64)
			if id == 0 {
				for k, j := range jobs {
					if j.GitRef == r.URL.Query().Get("sha") {
						id = k
						break
					}
				}
			}
			if reviews[id] == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(w, reviews[id])
		}).
		WithHandler("/api/comments", func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.ParseInt(r.URL.Query().Get("job_id"), 10, 64)
			writeJSON(w, map[string]any{"responses": comments[id]})
		}).
		WithHandler("/api/comment", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				JobID     int64  `json:"job_id"`
				Commenter string `json:"commenter"`
				Comment   string `json:"comment"`
			}
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			comments[body.JobID] = append(comments[body.JobID], storage.Response{Responder: body.Commenter, Response: body.Comment})
			w.WriteHeader(http.StatusCreated)
		}).
		WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				JobID int64 `json:"job_id"`
			}
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			reviews[body.JobID].Closed = true
			w.WriteHeader(http.StatusOK)
		}).Build()
	calls := []string{}
	name := "refine-plan-loop-test"
	agent.Register(&refineModeGuardAgent{t: t, Agent: &agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, path, ref, p string, _ io.Writer) (string, error) {
		calls = append(calls, p)
		switch len(calls) {
		case 1, 4:
			return "", fmt.Errorf("transient planning provider failure")
		case 2, 5:
			return "Check cancellation before claiming work", nil
		}
		require.NoError(t, os.WriteFile(filepath.Join(path, "next.go"), fmt.Appendf(nil, "package source\n// Fix %d\n", len(calls)), 0o600))
		return "Added cancellation check", nil
	}}})
	t.Cleanup(func() { agent.Unregister(name) })
	var runErr error
	stdout := captureOutput(t, func() error {
		runErr = runRefine(RunContext{Context: context.Background(), WorkingDir: repo.Dir, PostCommitDelay: time.Millisecond}, refineOptions{plan: true, since: base, agentName: name, maxIterations: 4, unsafeFlagChanged: true, allowUnsafeAgents: false})
		return nil
	})
	require.ErrorContains(t, runErr, "max iterations")
	a := assert.New(t)
	a.Contains(stdout, "Planning error: planning agent: transient planning provider failure")
	a.Contains(stdout, "Will retry in next iteration")
	a.Contains(stdout, "Check cancellation before claiming work")
	a.Len(calls, 6)
	a.Equal(calls[0], calls[1])
	a.NotContains(calls[0], "## Plan\n")
	a.NotContains(calls[1], "## Plan\n")
	a.Contains(calls[2], "## Plan\n")
	a.Contains(calls[3], "Check cancellation before claiming work")
	a.Contains(calls[3], "Added cancellation check")
	a.Contains(calls[3], "cancellation still races")
	a.Equal(calls[3], calls[4], "a planning retry must retain the earlier review context")
	a.Contains(calls[5], "## Plan\n")
	responders := make([]string, 0, len(comments[8]))
	for _, response := range comments[8] {
		responders = append(responders, response.Responder)
	}
	a.ElementsMatch([]string{"roborev-plan", "roborev-refine"}, responders,
		"only this review's new plan and implementation should be posted")
	for _, p := range calls {
		a.Contains(p, "Preserve cancellation invariants")
		a.NotContains(p, "Ignore cancellation requirements")
	}
	a.Empty(repo.Run("status", "--porcelain"))
}
