package daemon

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestCICheckApply(t *testing.T) {
	doc := structuredreview.Document{SchemaVersion: 3, Summary: "Original summary", Verdict: "fail", Findings: []structuredreview.Finding{
		{Severity: "high", Problem: "first", Fix: "fix"}, {Severity: "low", Problem: "second", Fix: "fix"},
	}}
	for _, tc := range []struct {
		name, raw, summary string
		want               []string
	}{
		{name: "drop with evidence", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":"build succeeded","reason":"claim refuted"}]}`, summary: "1 finding.", want: []string{"second"}},
		{name: "explicit keep", raw: `{"verdicts":[{"finding":1,"keep":true,"evidence":"failed","reason":"reproduced"}]}`},
		{name: "missing verdict", raw: `{"verdicts":[]}`},
		{name: "duplicate", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":"ok","reason":"refuted"},{"finding":1,"keep":false,"evidence":"ok","reason":"refuted"}]}`},
		{name: "empty evidence", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":" ","reason":"refuted"}]}`},
		{name: "empty reason", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":"ok","reason":""}]}`},
		{name: "missing keep", raw: `{"verdicts":[{"finding":1,"evidence":"ok","reason":"refuted"}]}`},
		{name: "all dropped", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":"ok","reason":"refuted"},{"finding":2,"keep":false,"evidence":"ok","reason":"refuted"}]}`, summary: "No issues found.", want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == nil {
				tc.want, tc.summary = []string{"first", "second"}, doc.Summary
			}
			var log bytes.Buffer
			got, err := applyCICheck(doc, jsontext.Value(tc.raw), &log)
			require.NoError(t, err)
			problems := make([]string, 0, len(got.Findings))
			for _, f := range got.Findings {
				problems = append(problems, f.Problem)
			}
			assert.Equal(t, tc.want, problems)
			assert.Equal(t, tc.summary, got.Summary)
			if tc.name != "missing verdict" {
				assert.Contains(t, log.String(), `"evidence":"`)
			}
		})
	}
}

func TestCICheckSnapshot(t *testing.T) {
	p := &CIPoller{}
	p.buildReviewPromptFn = func(context.Context, string, string, int64, int, string, string, string, string, prompt.PullRequest, *config.RepoConfig, string, *config.Config) (string, error) {
		return "prompt", nil
	}
	cfg := config.DefaultConfig()
	in := buildPanelOptsInput{repo: &storage.Repo{ID: 1, RootPath: t.TempDir()}, cfg: cfg, gitRef: "base..head", baseBranch: "main", members: []config.ResolvedMember{{Name: "one", Agent: "codex"}}, synth: config.SynthesisSpec{Agent: "codex", Instructions: "merge"}}
	_, off, err := p.buildPanelOpts(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, `{"instructions":"merge"}`, off.PanelMemberConfigJSON) //nolint:testifylint // Checker off preserves the existing snapshot byte for byte.
	cfg.CI.Check = true
	cfg.CI.CheckModel = "checker-model"
	cfg.CI.CheckReasoning = " HIGH "
	_, on, err := p.buildPanelOpts(t.Context(), in)
	require.NoError(t, err)
	var snapshot synthesisJobConfig
	require.NoError(t, json.Unmarshal([]byte(on.PanelMemberConfigJSON), &snapshot))
	require.NotNil(t, snapshot.Check)
	assert.Equal(t, "checker-model", snapshot.Check.Model)
	assert.Equal(t, "high", snapshot.Check.Reasoning)
	override := false
	in.repoCfg = &config.RepoConfig{CI: config.RepoCIConfig{Check: &override}}
	_, overridden, err := p.buildPanelOpts(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, off.PanelMemberConfigJSON, overridden.PanelMemberConfigJSON)
}

func setupCICheckCompletion(t *testing.T, threshold, instructions string, usage *tokens.Usage) (*workerTestContext, *storage.ReviewJob, structuredreview.Document, synthesisResult) {
	t.Helper()
	const raw = `{"schema_version":3,"source_labels":["test"],"summary":"Two claims","verdict":"fail","findings":[{"severity":"high","kind":"defect","confidence":0.9,"problem":"Build claim","fix":"Fix build","sources":[1]},{"severity":"medium","kind":"defect","confidence":0.9,"problem":"Data loss","fix":"Preserve data","sources":[1]}]}`
	tc := newWorkerTestContext(t, 1)
	tc.Pool.cfgGetter.Config().CodexCmd = "custom-codex"
	synthAgent := &synthesisEntrypointTestAgent{name: "checker-agent", result: raw}
	agent.RegisterForTest(t, synthAgent)
	run, members, _ := enqueuePanelRun(t, tc, "checker", []memberSpec{{name: "one", agent: "test"}})
	setSynthesisAgent(t, tc, run, synthAgent.name)
	if usage != nil {
		synthAgent.streamLine = "{\"type\":\"thread.started\",\"thread_id\":\"synthesis-session\"}\n" + `{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":20}}`
		tc.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
			assert.Equal(t, "synthesis-session", sessionID)
			return usage, nil
		}
	}
	encoded, err := (synthesisJobConfig{Instructions: instructions, Check: &ciCheckConfig{Reasoning: "high"}}).encode()
	require.NoError(t, err)
	_, err = tc.DB.Exec("UPDATE review_jobs SET panel_member_config_json = ?, min_severity = ? WHERE panel_run_uuid = ? AND panel_role = 'synthesis'", encoded, threshold, run)
	require.NoError(t, err)
	markMemberRunning(t, tc, members[0].ID)
	require.NoError(t, tc.DB.CompleteJobResult(members[0].ID, "test", storage.ReviewCompletion{Output: "two claims", Verdict: storage.VerdictFail, StructuredOutput: jsontext.Value(raw)}))
	job := releaseAndClaimSynthesis(t, tc, run)
	doc, err := reviewpkg.DecodeStructuredReview(jsontext.Value(raw))
	require.NoError(t, err)
	original := synthesisResult{review: reviewpkg.ReviewResult{Agent: "test", Output: doc.Markdown(threshold), Verdict: storage.VerdictFail, StructuredOutput: jsontext.Value(raw)}}
	return tc, job, doc, original
}

func readCICheckCompletionReview(t *testing.T, tc *workerTestContext, job *storage.ReviewJob) (*storage.Review, structuredreview.Document) {
	t.Helper()
	stored, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	structured, err := json.Marshal(stored.StructuredOutput)
	require.NoError(t, err)
	checked, err := reviewpkg.DecodeStructuredReview(structured)
	require.NoError(t, err)
	return stored, checked
}

func TestCICheckCompletionFailure(t *testing.T) {
	for _, failure := range []struct {
		name       string
		afterUsage bool
	}{
		{name: "failure"},
		{name: "failure after usage", afterUsage: true},
	} {
		t.Run(failure.name, func(t *testing.T) {
			tc, job, doc, original := setupCICheckCompletion(t, "high", "", nil)
			tc.Pool.ciCheck = func(ctx context.Context, job *storage.ReviewJob, cfg ciCheckConfig, doc structuredreview.Document, env []string, out io.Writer) (structuredreview.Document, error) {
				if failure.afterUsage {
					fmt.Fprintln(out, `{"type":"thread.started","thread_id":"checker-session"}`)
					// Capture usage even when the final event has no newline.
					fmt.Fprint(out, `{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":5}}`)
					return structuredreview.Document{}, fmt.Errorf("checker failed after reporting usage")
				}
				return tc.Pool.runCICheck(ctx, job, cfg, doc, env, out)
			}
			tc.Pool.processSynthesisJob(t.Context(), testWorkerID, job)
			finished, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusDone, finished.Status)
			stored, checked := readCICheckCompletionReview(t, tc, job)
			assert.Equal(t, original.review.Output, stored.Output)
			assert.Equal(t, original.review.Verdict, stored.Verdict())
			assert.Equal(t, doc, checked)
			if failure.afterUsage {
				usage := tokens.ParseJSON(finished.TokenUsage)
				require.NotNil(t, usage)
				assert := assert.New(t)
				assert.False(usage.HasCost)
				assert.EqualValues(3, usage.InputTokens)
				assert.EqualValues(5, usage.OutputTokens)
				assert.Empty(finished.SessionID)
				assert.Empty(usage.ThreadID)
				assert.Empty(usage.ProviderSessionIDs)
				assert.Equal(1, usage.ExpectedProviderSessions)
			} else {
				var invoked bool
				require.NoError(t, tc.DB.QueryRow("SELECT agent_invoked FROM review_jobs WHERE id = ?", job.ID).Scan(&invoked))
				assert.False(t, invoked)
				log, err := os.ReadFile(JobLogPath(job.ID))
				require.NoError(t, err)
				assert.Contains(t, string(log), "CI checker failed; keeping original review: get checker codex")
				assert.Contains(t, string(log), "custom-codex")
			}
		})
	}
}

func TestCICheckCompletionDrop(t *testing.T) {
	for _, drop := range []struct {
		name, threshold, instructions string
		usage                         *tokens.Usage
		verdict                       storage.Verdict
		structuredVerdict             string
	}{
		{name: "drop", threshold: "medium", verdict: storage.VerdictFail, structuredVerdict: structuredreview.VerdictFail},
		{name: "agent drop", threshold: "high", instructions: "verify", usage: &tokens.Usage{InputTokens: 12, OutputTokens: 24, CostUSD: 0.03, HasCost: true}, verdict: storage.VerdictPass, structuredVerdict: structuredreview.VerdictPass},
		{name: "agent late pricing drop", threshold: "high", instructions: "verify", usage: &tokens.Usage{InputTokens: 10, OutputTokens: 20}, verdict: storage.VerdictPass, structuredVerdict: structuredreview.VerdictPass},
	} {
		t.Run(drop.name, func(t *testing.T) {
			tc, job, _, _ := setupCICheckCompletion(t, drop.threshold, drop.instructions, drop.usage)
			_, updates, unsubscribe := tc.Pool.outputBuffers.Subscribe(job.ID)
			defer unsubscribe()
			calls := 0
			tc.Pool.ciCheck = func(_ context.Context, _ *storage.ReviewJob, cfg ciCheckConfig, doc structuredreview.Document, _ []string, out io.Writer) (structuredreview.Document, error) {
				calls++
				assert.Equal(t, "high", cfg.Reasoning)
				require.Len(t, doc.Findings, 2)
				fmt.Fprintln(out, `{"type":"thread.started","thread_id":"checker-session"}`)
				fmt.Fprintln(out, `{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":5}}`)
				if drop.name == "drop" {
					fmt.Fprintln(out, `{"type":"item.started","item":{"type":"command_execution","command":"go test ./example"}}`)
					// Keep an unterminated tail, as subprocess writes can do.
					fmt.Fprint(out, `{"type":"item.completed","item":{"type":"agent_message","text":"Checker progress"}}`)
				}
				doc.Findings = doc.Findings[1:]
				return doc, nil
			}
			tc.Pool.processSynthesisJob(t.Context(), testWorkerID, job)
			assert.Equal(t, 1, calls)
			if drop.name == "drop" {
				unsubscribe()
				var lines []string
				for line := range updates {
					lines = append(lines, line.Text)
				}
				log, err := os.ReadFile(JobLogPath(job.ID))
				require.NoError(t, err)
				for _, want := range []string{"[Command: go test ./example]", "Checker progress"} {
					assert.Contains(t, lines, want)
					assert.Contains(t, string(log), want)
				}
			}
			finished, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusDone, finished.Status)
			stored, checked := readCICheckCompletionReview(t, tc, job)
			assert.NotContains(t, stored.Output, "Build claim")
			assert.Contains(t, stored.Output, "Data loss")
			assert.Equal(t, drop.verdict, stored.Verdict())
			assert.Equal(t, drop.structuredVerdict, checked.Verdict)
			require.Len(t, checked.Findings, 1)
			assert.Equal(t, "Data loss", checked.Findings[0].Problem)
			if drop.usage != nil {
				usage := tokens.ParseJSON(finished.TokenUsage)
				require.NotNil(t, usage)
				assert := assert.New(t)
				assert.Equal("synthesis-session", finished.SessionID)
				assert.Equal("synthesis-session", usage.ThreadID)
				assert.Equal([]string{"synthesis-session"}, usage.ProviderSessionIDs)
				assert.Equal(2, usage.ExpectedProviderSessions)
				if drop.usage.HasCost {
					assert.True(usage.HasCost)
					assert.InDelta(0.03, usage.CostUSD, 1e-9)
					assert.EqualValues(15, usage.InputTokens)
					assert.EqualValues(29, usage.OutputTokens)
				} else {
					candidate, err := tc.DB.GetTokenCostCandidate(job.ID)
					require.NoError(t, err)
					require.NotNil(t, candidate)
					assert.False(usage.HasCost)
					assert.EqualValues(13, usage.InputTokens)
					assert.EqualValues(25, usage.OutputTokens)
				}
			}
		})
	}
}

func TestCICheckCompletionInterruption(t *testing.T) {
	for _, interruption := range []struct {
		name   string
		ctx    func(context.Context) (context.Context, context.CancelFunc)
		status storage.JobStatus
	}{
		{name: "checker canceled", ctx: func(parent context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(parent)
			cancel()
			return ctx, cancel
		}, status: storage.JobStatusQueued},
		{name: "checker deadline", ctx: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithDeadline(parent, time.Unix(0, 0))
		}, status: storage.JobStatusDone},
	} {
		t.Run(interruption.name, func(t *testing.T) {
			tc, job, doc, original := setupCICheckCompletion(t, "high", "", nil)
			tc.Pool.ciCheck = func(ctx context.Context, _ *storage.ReviewJob, _ ciCheckConfig, _ structuredreview.Document, _ []string, _ io.Writer) (structuredreview.Document, error) {
				return structuredreview.Document{}, ctx.Err()
			}
			if interruption.status == storage.JobStatusQueued {
				tc.Pool.InterruptJobsForUpdate([]int64{job.ID})
			}
			ctx, cancel := interruption.ctx(t.Context())
			defer cancel()
			tc.Pool.checkAndCompleteSynthesis(ctx, testWorkerID, job, original, doc, &ciCheckConfig{Reasoning: "high"})
			finished, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, interruption.status, finished.Status)
		})
	}
}

func TestCICheckCompletionBelowSeverityThreshold(t *testing.T) {
	calls := 0
	tc := newWorkerTestContext(t, 1)
	tc.Pool.ciCheck = func(_ context.Context, _ *storage.ReviewJob, _ ciCheckConfig, doc structuredreview.Document, _ []string, _ io.Writer) (structuredreview.Document, error) {
		calls++
		return doc, nil
	}
	doc := structuredreview.Document{Verdict: structuredreview.VerdictPass, Findings: []structuredreview.Finding{{Severity: "low"}}}
	_, ran := tc.Pool.checkSynthesis(t.Context(), &storage.ReviewJob{MinSeverity: "high"}, synthesisResult{}, doc, &ciCheckConfig{})
	assert.False(t, ran)
	assert.Equal(t, 0, calls)
}
