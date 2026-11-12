package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/tokens"
	roborevclient "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

func TestSuppliedPanelKeepsInputsThroughExecutionAndRerun(t *testing.T) {
	testenv.SetDataDir(t)
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	base := repo.CommitFile("file.txt", "before\n", "base")
	hookDir := t.TempDir()
	cfg := config.DefaultConfig()
	repoHooks := ""
	for _, event := range []string{"started", "completed"} {
		cfg.Hooks = append(cfg.Hooks, config.HookConfig{Event: "review." + event, Command: touchCmd(filepath.Join(hookDir, "global-{job_id}-"+event))})
		repoHooks += fmt.Sprintf("\n[[hooks]]\nevent = %q\ncommand = %q\n", "review."+event, touchCmd(filepath.Join(hookDir, "repo-{job_id}-"+event)))
	}
	runner := NewHookRunner(NewStaticConfig(cfg), server.workerPool.broadcaster, nil)
	t.Cleanup(runner.Stop)
	var executed []int64
	repoPolicy := "exclude_patterns = ['file.txt']\nreview_min_severity = 'high'\nmodel = 'checkout-model'\nmax_prompt_size = 1\nsnapshot_dir = 'checkout-snapshots'\n" + repoHooks
	repo.CommitFile(".roborev.toml", repoPolicy, "policy")
	head := repo.CommitFile("file.txt", "VISIBLE_CHANGE\n", "target")
	repo.Run("branch", "-m", "review-target")
	repo.Run("checkout", "-b", "unrelated-checkout", base)
	repo.CommitFile("other.txt", "unrelated", "unrelated")
	// Hook dispatch reads the current checkout, not the frozen review checkout.
	repo.CommitFile(".roborev.toml", repoPolicy, "checkout policy")
	ref := base + ".." + head
	memberCalls, synthesisCalls := 0, 0
	memberPrompts := make(map[string]int)
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "supplied-panel-member", ReviewFn: func(_ context.Context, root, gotRef, text string, _ io.Writer) (string, error) {
		memberCalls++
		assert.Equal(t, ref, gotRef)
		assert.Contains(t, text, "CALLER_REVIEW_POLICY")
		for _, name := range []string{"general", "correctness"} {
			if strings.Contains(text, "CALLER_REVIEW_POLICY_"+name) {
				memberPrompts[name]++
			}
		}
		matches, err := filepath.Glob(filepath.Join(root, ".roborev", "*", "roborev-snapshot-content.diff"))
		require.NoError(t, err)
		require.Len(t, matches, 1)
		data, err := os.ReadFile(matches[0])
		require.NoError(t, err)
		assert.Contains(t, string(data), "+VISIBLE_CHANGE")
		return `{"schema_version":3,"summary":"Check missing","verdict":"fail","findings":[{"severity":"low","kind":"defect","confidence":0.8,"problem":"Missing check.","fix":"Add check.","file":null,"start_line":null,"end_line":null}]}`, nil
	}})
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "supplied-panel-synthesis", ReviewFn: func(_ context.Context, root, gotRef, text string, _ io.Writer) (string, error) {
		synthesisCalls++
		assert.Equal(t, ref, gotRef)
		assert.Contains(t, text, "CALLER_SYNTHESIS_POLICY")
		data, err := os.ReadFile(filepath.Join(root, "file.txt"))
		require.NoError(t, err)
		assert.Equal(t, "VISIBLE_CHANGE\n", string(data))
		return string(testutil.ReviewFixtureJSON("No issues found.")), nil
	}})
	httpServer := httptest.NewServer(server.httpServer.Handler)
	t.Cleanup(httpServer.Close)
	client, err := roborevclient.New(httpServer.URL)
	require.NoError(t, err)
	panel, err := client.EnqueueSuppliedPanel(t.Context(), &generated.EnqueueSuppliedPanelRequestOptions{
		Body: &generated.EnqueueSuppliedPanelInputBody{
			RepoPath: repo.Path(), GitRef: ref, HoldSynthesis: new(true),
			Members: []generated.SuppliedPanelMember{
				{Name: "general", Agent: "supplied-panel-member", Model: new("caller-model"), ReviewPrompt: "CALLER_REVIEW_POLICY_general: " + prompt.DiffFilePathPlaceholder},
				{Name: "correctness", Agent: "supplied-panel-member", Model: new("caller-model"), ReviewPrompt: "CALLER_REVIEW_POLICY_correctness: " + prompt.DiffFilePathPlaceholder},
			},
			Synthesis: generated.SuppliedPanelSynthesis{Agent: "supplied-panel-synthesis", Model: new("synthesis-model"), Instructions: new("CALLER_SYNTHESIS_POLICY")},
		},
	})
	require.NoError(t, err)
	require.Len(t, panel.MemberJobIds, 2)
	for run := range 2 {
		for range 2 {
			job, err := db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, job)
			require.Equal(t, storage.PanelRoleMember, job.PanelRole)
			assert.Equal(t, "caller-model", job.Model)
			assert.Equal(t, "review-target", job.Branch)
			executed = append(executed, job.ID)
			server.workerPool.processJob(testWorkerID, job)
			finished, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			require.Equal(t, storage.JobStatusDone, finished.Status, finished.Error)
			review, err := db.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			require.NotNil(t, review.VerdictBool)
			assert.Equal(t, 0, *review.VerdictBool, "checkout severity must not hide the finding")
			reports, err := db.ListReviewInvocations(t.Context(), job.ID)
			require.NoError(t, err)
			require.Len(t, reports, 1)
			assert.Equal(t, "succeeded", reports[0].Status)
		}
		job, err := db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		assert.Nil(t, job)
		w := requestSynthesis(t, server, panel.PanelRunUUID)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		job, err = db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		require.NotNil(t, job)
		assert.Equal(t, "synthesis-model", job.Model)
		assert.Equal(t, "review-target", job.Branch)
		executed = append(executed, job.ID)
		server.workerPool.processJob(testWorkerID, job)
		finished, err := db.GetJobByID(job.ID)
		require.NoError(t, err)
		require.Equal(t, storage.JobStatusDone, finished.Status, finished.Error)
		reports, err := client.GetJobInvocations(t.Context(), &generated.GetJobInvocationsRequestOptions{
			Query: &generated.GetJobInvocationsQuery{JobID: job.ID},
		})
		require.NoError(t, err)
		require.Len(t, reports.Invocations, 1)
		assert.Equal(t, "succeeded", reports.Invocations[0].Status)
		assert.Equal(t, "synthesis-model", reports.Invocations[0].Model)
		assert.Equal(t, ref, reports.Invocations[0].GitRef)
		assert.Nil(t, reports.Invocations[0].Usage)
		structured, err := json.Marshal(reports.Invocations[0].StructuredOutput)
		require.NoError(t, err)
		assert.JSONEq(t, `{"schema_version":3,"summary":"No issues found.","verdict":"pass","findings":[],"source_labels":["supplied-panel-member","supplied-panel-member"]}`, string(structured))
		runner.WaitUntilIdle()
		server.hookRunner.WaitUntilIdle()
		if run == 0 {
			for _, id := range executed {
				for _, event := range []string{"started", "completed"} {
					assert.FileExists(t, filepath.Join(hookDir, fmt.Sprintf("global-%d-%s", id, event)))
					assert.NoFileExists(t, filepath.Join(hookDir, fmt.Sprintf("repo-%d-%s", id, event)), "supplied policy must not execute repository hooks")
				}
			}
			// Even malformed current policy must not change the frozen rerun.
			require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("invalid TOML ["), 0o600))
			rerun, err := server.humaRerunJob(t.Context(), &RerunJobInput{Body: RerunJobRequest{JobID: panel.ID}})
			require.NoError(t, err)
			panel.PanelRunUUID = *rerun.Body.RunUUID
			panel.ID = rerun.Body.JobID
		}
	}
	assert.Equal(t, 4, memberCalls)
	assert.Equal(t, 2, synthesisCalls)
	assert.Equal(t, map[string]int{"general": 2, "correctness": 2}, memberPrompts)
}

func TestSuppliedPanelPreparesSynthesisTools(t *testing.T) {
	testenv.SetDataDir(t)
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "target", "target")
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "strict-panel-member", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		return string(testutil.ReviewFixtureJSON("## Findings\n- [P1] Missing validation")), nil
	}})
	calls := 0
	agent.RegisterForTest(t, &strictWorkerTestAgent{
		structuredWorkerTestAgent: &structuredWorkerTestAgent{name: "strict-panel-synthesis", result: jsontext.Value(testutil.ReviewFixtureJSON("No issues found."))},
		onReview: func(path string) {
			calls++
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.JSONEq(t, `{"mcpServers":{}}`, string(data))
		},
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/panel/enqueue", map[string]any{
		"repo_path": repo.Path(), "git_ref": head,
		"members":   []any{map[string]any{"name": "reviewer", "agent": "strict-panel-member", "review_prompt": "Review.", "review_settings": map[string]any{}}},
		"synthesis": map[string]any{"agent": "strict-panel-synthesis", "instructions": "Verify the finding.", "tools": map[string]any{"mode": "read-only", "mcp": map[string]any{}}},
	}))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	for range 2 {
		job, err := db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		require.NotNil(t, job)
		server.workerPool.processJob(testWorkerID, job)
		finished, err := db.GetJobByID(job.ID)
		require.NoError(t, err)
		require.Equal(t, storage.JobStatusDone, finished.Status, finished.Error)
	}
	assert.Equal(t, 1, calls)
}

func TestSuppliedPanelRestoresPrimaryAgentsAfterFailover(t *testing.T) {
	testenv.SetDataDir(t)
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "target\n", "target")
	calls := make(map[string]int)
	for _, name := range []string{"panel-primary", "panel-backup", "synthesis-primary", "synthesis-backup"} {
		agent.RegisterForTest(t, &agent.FakeAgent{NameStr: name, ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			calls[name]++
			if name == "panel-primary" || name == "synthesis-primary" {
				return "partial", errors.New("context window exceeded")
			}
			if name == "panel-backup" {
				return string(testutil.ReviewFixtureJSON("## Findings\n- [P1] Missing validation")), nil
			}
			return string(testutil.ReviewFixtureJSON("No issues found.")), nil
		}})
	}
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/panel/enqueue", map[string]any{
		"repo_path": repo.Path(), "git_ref": head,
		"members": []any{map[string]any{
			"name": "reviewer", "agent": "panel-primary", "model": "member-primary-model", "review_prompt": "Review the change.",
			"review_settings": map[string]any{"backup_agent": "panel-backup", "backup_model": "member-backup-model"},
		}},
		"synthesis": map[string]any{"agent": "synthesis-primary", "model": "synthesis-primary-model", "backup_agent": "synthesis-backup", "backup_model": "synthesis-backup-model", "instructions": "Verify the review."},
	}))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var panel PanelEnqueueResponse
	testutil.DecodeJSON(t, w, &panel)
	for run := range 2 {
		for _, step := range []struct{ agent, model string }{
			{"panel-primary", "member-primary-model"},
			{"panel-backup", "member-backup-model"},
			{"synthesis-primary", "synthesis-primary-model"},
			{"synthesis-backup", "synthesis-backup-model"},
		} {
			job, err := db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, job)
			require.Equal(t, step.agent, job.Agent)
			assert.Equal(t, step.model, job.Model)
			server.workerPool.processJob(testWorkerID, job)
		}
		finished, err := db.GetJobByID(panel.ID)
		require.NoError(t, err)
		require.Equal(t, storage.JobStatusDone, finished.Status, finished.Error)
		reports, err := db.ListReviewInvocations(t.Context(), panel.ID)
		require.NoError(t, err)
		require.Len(t, reports, 2)
		assert.Equal(t, "failed", reports[0].Status)
		assert.Equal(t, "partial", reports[0].RawOutput)
		assert.Equal(t, "context window exceeded", reports[0].Error)
		assert.Equal(t, "synthesis-primary-model", reports[0].Model)
		assert.Equal(t, "succeeded", reports[1].Status)
		assert.Equal(t, "synthesis-backup", reports[1].Agent)
		assert.Equal(t, "synthesis-backup-model", reports[1].Model)
		if run == 0 {
			rerun, err := server.humaRerunJob(t.Context(), &RerunJobInput{Body: RerunJobRequest{JobID: panel.ID}})
			require.NoError(t, err)
			panel.ID = rerun.Body.JobID
		}
	}
	assert.Equal(t, map[string]int{"panel-primary": 2, "panel-backup": 2, "synthesis-primary": 2, "synthesis-backup": 2}, calls)
}

func TestSuppliedPanelRecordsUnavailablePrimarySynthesisModel(t *testing.T) {
	for _, backupModel := range []string{"backup-model", ""} {
		t.Run("backup_model="+backupModel, func(t *testing.T) {
			testenv.SetDataDir(t)
			assert := assert.New(t)
			server, db, _ := newTestServer(t)
			repo := testutil.NewGitRepo(t)
			head := repo.CommitFile("file.txt", "target", "target")
			primary := &synthesisEntrypointTestAgent{name: "available-primary", result: string(testutil.ReviewFixtureJSON("No issues found."))}
			backup := &synthesisEntrypointTestAgent{name: "available-backup", result: primary.result}
			agent.RegisterForTest(t, primary)
			agent.RegisterForTest(t, backup)
			agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "availability-member", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
				return string(testutil.ReviewFixtureJSON("## Findings\n- [P1] Missing validation")), nil
			}})
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/panel/enqueue", map[string]any{
				"repo_path": repo.Path(), "git_ref": head,
				"members":   []any{map[string]any{"name": "reviewer", "agent": "availability-member", "review_prompt": "Review.", "review_settings": map[string]any{}}},
				"synthesis": map[string]any{"agent": primary.name, "model": "primary-model", "backup_agent": backup.name, "backup_model": backupModel, "instructions": "Verify."},
			}))
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			var panel PanelEnqueueResponse
			testutil.DecodeJSON(t, w, &panel)
			// Enqueue resolved an available primary; execution now finds its command missing.
			agent.Register(&unavailableSynthesisCommandAgent{name: primary.name, command: filepath.Join(t.TempDir(), "missing-agent")})
			for run := range 2 {
				for range 2 {
					job, err := db.ClaimJob(testWorkerID)
					require.NoError(t, err)
					require.NotNil(t, job)
					server.workerPool.processJob(testWorkerID, job)
				}
				stored, err := db.GetJobByID(panel.ID)
				require.NoError(t, err)
				require.Equal(t, storage.JobStatusDone, stored.Status, stored.Error)
				assert.Equal(primary.name, stored.Agent, "fallback must not rewrite the configured agent")
				assert.Equal("primary-model", stored.Model, "reruns retain their primary model")
				reports, err := db.ListReviewInvocations(t.Context(), panel.ID)
				require.NoError(t, err)
				require.Len(t, reports, 1)
				assert.Equal("succeeded", reports[0].Status)
				if run == 0 {
					assert.True(backup.reviewCalled)
					assert.False(primary.reviewCalled)
					assert.Equal(backupModel, backup.model, "execution uses the backup's model, including an empty default")
					assert.Equal(backup.name, reports[0].Agent)
					assert.Equal(backupModel, reports[0].Model, "history must describe the model used for this call")
					agent.Register(primary)
					rerun, err := server.humaRerunJob(t.Context(), &RerunJobInput{Body: RerunJobRequest{JobID: panel.ID}})
					require.NoError(t, err)
					panel.ID = rerun.Body.JobID
				} else {
					assert.True(primary.reviewCalled)
					assert.Equal("primary-model", primary.model)
					assert.Equal(primary.name, reports[0].Agent)
					assert.Equal("primary-model", reports[0].Model)
				}
			}
		})
	}
}

func TestSuppliedPanelRejectsInvalidPlanWithoutQueueingMembers(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "target", "target")
	for _, tc := range []struct {
		name   string
		mutate func(*EnqueueSuppliedPanelInput)
	}{
		{"last member agent", func(in *EnqueueSuppliedPanelInput) { in.Body.Members[1].Agent = "no-such-agent" }},
		{"duplicate name", func(in *EnqueueSuppliedPanelInput) { in.Body.Members[1].Name = "first" }},
		{"empty prompt", func(in *EnqueueSuppliedPanelInput) { in.Body.Members[1].ReviewPrompt = " " }},
		{"mutable context", func(in *EnqueueSuppliedPanelInput) {
			in.Body.Members[1].ReviewPrompt = prompt.PriorRangeReviewsFilePathPlaceholder
		}},
		{"synthesis agent", func(in *EnqueueSuppliedPanelInput) { in.Body.Synthesis.Agent = "no-such-agent" }},
		{"synthesis unsupported tools", func(in *EnqueueSuppliedPanelInput) {
			in.Body.Synthesis.Tools = &config.ReviewToolPolicy{Mode: "read-only"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in EnqueueSuppliedPanelInput
			in.Body.RepoPath, in.Body.GitRef = repo.Path(), head
			for _, name := range []string{"first", "second"} {
				in.Body.Members = append(in.Body.Members, SuppliedPanelMember{Agent: "test", Name: name, ReviewPrompt: "Review."})
			}
			in.Body.Synthesis.Agent = "test"
			tc.mutate(&in)
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/panel/enqueue", in.Body))
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			jobs, err := db.ListJobs("", "", 10, 0)
			require.NoError(t, err)
			assert.Empty(t, jobs)
		})
	}
}

func TestSuppliedPanelRecordsStrictClaudeSynthesisUsage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable")
	}
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", failed), func(t *testing.T) {
			testenv.SetDataDir(t)
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			result, exit := `{"schema_version":3,"summary":"No findings.","verdict":"pass","findings":[]}`, "0"
			if failed {
				result, exit = "review failed", "1"
			}
			stream := `{"type":"system","subtype":"init","session_id":"unsaved","permissionMode":"dontAsk","tools":["Read","Glob","Grep","StructuredOutput"],"mcp_servers":[]}` + "\n" +
				fmt.Sprintf(`{"type":"result","session_id":"unsaved","is_error":%t,"result":%q,"usage":{"input_tokens":100,"output_tokens":20},"total_cost_usd":0.25}`, failed, result)
			script := "#!/bin/sh\ncase \"$1\" in\n*etxtbsy*) exit 0 ;;\n--help) echo '--restricted --tools --allowedTools --strict-mcp-config --mcp-config --permission-mode --permission-prompts --no-session-persistence --disable-slash-commands --json-schema'; exit 0 ;;\nesac\ncat <<'OUTPUT'\n" + stream + "\nOUTPUT\nexit " + exit + "\n"
			t.Cleanup(testutil.MockBinaryInPath(t, "claude", script))
			server, db, _ := newTestServer(t)
			sessionLookups := 0
			server.workerPool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
				sessionLookups++
				return nil, tokens.ErrUsageProviderUnavailable
			}
			repo := testutil.NewGitRepo(t)
			head := repo.CommitFile("file.txt", "target", "target")
			agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "usage-panel-member", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
				return string(testutil.ReviewFixtureJSON("## Findings\n- [P1] Missing validation")), nil
			}})
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/panel/enqueue", map[string]any{
				"repo_path": repo.Path(), "git_ref": head,
				"members":   []any{map[string]any{"name": "reviewer", "agent": "usage-panel-member", "review_prompt": "Review.", "review_settings": map[string]any{}}},
				"synthesis": map[string]any{"agent": "claude-code", "instructions": "Verify the finding.", "tools": map[string]any{"mode": "read-only", "mcp": map[string]any{}}},
			}))
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			var panel PanelEnqueueResponse
			testutil.DecodeJSON(t, w, &panel)
			for range 2 {
				job, err := db.ClaimJob(testWorkerID)
				require.NoError(t, err)
				require.NotNil(t, job)
				if failed && job.IsSynthesisJob() {
					for range maxRetries {
						retried, err := db.RetryJob(job.ID, testWorkerID, maxRetries, 0)
						require.NoError(t, err)
						require.True(t, retried)
						job, err = db.ClaimJob(testWorkerID)
						require.NoError(t, err)
						require.NotNil(t, job)
					}
				}
				server.workerPool.processJob(testWorkerID, job)
			}
			stored, err := db.GetJobByID(panel.ID)
			require.NoError(t, err)
			wantStatus := storage.JobStatusDone
			if failed {
				wantStatus = storage.JobStatusFailed
			}
			require.Equal(t, wantStatus, stored.Status, stored.Error)
			reports, err := db.ListReviewInvocations(t.Context(), panel.ID)
			require.NoError(t, err)
			require.Len(t, reports, 1)
			wantInvocationStatus := "succeeded"
			if failed {
				wantInvocationStatus = "failed"
			}
			assert.Equal(t, wantInvocationStatus, reports[0].Status)
			require.NotNil(t, reports[0].Usage)
			assert.Equal(t, int64(100), reports[0].Usage.InputTokens)
			assert.Equal(t, int64(20), reports[0].Usage.OutputTokens)
			assert.InDelta(t, 0.25, reports[0].Usage.CostUSD, 1e-9)
			assertStrictClaudeUsage(t, stored)
			assert.Zero(t, sessionLookups, "restricted Claude must not need a saved session for usage")
			assert.Empty(t, stored.SessionID)
			assert.Empty(t, tokens.ParseJSON(stored.TokenUsage).ThreadID)
			spend, err := db.GetBudgetSpend(time.Now())
			require.NoError(t, err)
			assert.InDelta(t, 0.25, spend.TotalUSD, 1e-9)
		})
	}
}
