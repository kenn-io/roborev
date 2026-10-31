package daemon

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"strings"
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
			if len(got.Findings) != 2 {
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
	// A reloaded invalid setting fails like the panel's other reasoning settings.
	in.repoCfg = nil
	cfg.CI.CheckReasoning = "invalid"
	_, _, err = p.buildPanelOpts(t.Context(), in)
	require.ErrorContains(t, err, "CI checker reasoning")
}

func TestCICheckLog(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	run, members, _ := enqueuePanelRun(t, tc, "checker-log", []memberSpec{{name: "one", agent: "test"}})
	completeMember(t, tc, members[0].ID, "test", "finding")
	job := releaseAndClaimSynthesis(t, tc, run)
	_, updates, unsubscribe := tc.Pool.outputBuffers.Subscribe(job.ID)
	defer unsubscribe()
	tc.Pool.ciCheck = func(_ context.Context, _ *storage.ReviewJob, _ ciCheckConfig, doc structuredreview.Document, _ []string, out io.Writer) (structuredreview.Document, error) {
		// Keep an unterminated tail, as subprocess writes can do.
		fmt.Fprintln(out, `{"type":"item.started","item":{"type":"command_execution","command":"go test ./example"}}`)
		fmt.Fprint(out, `{"type":"item.completed","item":{"type":"agent_message","text":"Checker progress"}}`)
		return doc, nil
	}
	doc := structuredreview.Document{Findings: []structuredreview.Finding{{Severity: "high", Problem: "claim"}}}
	_, ran := tc.Pool.checkSynthesis(t.Context(), job, synthesisResult{}, doc, &ciCheckConfig{})
	require.True(t, ran)
	for _, want := range []string{"[Command: go test ./example]", "Checker progress"} {
		var line OutputLine
		select {
		case line = <-updates:
		default:
		}
		assert.Equal(t, want, line.Text)
	}
	log, err := os.ReadFile(JobLogPath(job.ID))
	require.NoError(t, err)
	assert.Contains(t, string(log), "[Command: go test ./example]")
	assert.Contains(t, string(log), "Checker progress")
}

func TestCICheckCompletion(t *testing.T) {
	const raw = `{"schema_version":3,"source_labels":["test"],"summary":"Two claims","verdict":"fail","findings":[{"severity":"high","kind":"defect","confidence":0.9,"problem":"Build claim","fix":"Fix build","sources":[1]},{"severity":"medium","kind":"defect","confidence":0.9,"problem":"Data loss","fix":"Preserve data","sources":[1]}]}`
	// "agent drop" runs a priced synthesis agent; the rest pass one review through.
	for _, mode := range []string{"failure", "failure after usage", "drop", "agent drop", "below severity threshold", "checker canceled", "checker deadline"} {
		t.Run(mode, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			tc.Pool.cfgGetter.Config().CodexCmd = "custom-codex"
			synthAgent := &synthesisEntrypointTestAgent{name: "checker-agent", result: raw}
			agent.RegisterForTest(t, synthAgent)
			run, members, _ := enqueuePanelRun(t, tc, "checker", []memberSpec{{name: "one", agent: "test"}})
			setSynthesisAgent(t, tc, run, synthAgent.name)
			snapshot := synthesisJobConfig{Check: &ciCheckConfig{Reasoning: "high"}}
			if mode == "agent drop" {
				snapshot.Instructions = "verify"
				synthAgent.streamLine = "{\"type\":\"thread.started\",\"thread_id\":\"synthesis-session\"}\n" + `{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":20}}`
				tc.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
					assert.Equal(t, "synthesis-session", sessionID)
					return &tokens.Usage{InputTokens: 12, OutputTokens: 24, CostUSD: 0.03, HasCost: true}, nil
				}
			}
			encoded, err := snapshot.encode()
			require.NoError(t, err)
			threshold := "high"
			if mode == "drop" {
				threshold = "medium"
			}
			_, err = tc.DB.Exec("UPDATE review_jobs SET panel_member_config_json = ?, min_severity = ? WHERE panel_run_uuid = ? AND panel_role = 'synthesis'", encoded, threshold, run)
			require.NoError(t, err)
			markMemberRunning(t, tc, members[0].ID)
			require.NoError(t, tc.DB.CompleteJobResult(members[0].ID, "test", storage.ReviewCompletion{Output: "two claims", Verdict: storage.VerdictFail, StructuredOutput: jsontext.Value(raw)}))
			job := releaseAndClaimSynthesis(t, tc, run)
			calls := 0
			tc.Pool.ciCheck = func(ctx context.Context, job *storage.ReviewJob, cfg ciCheckConfig, doc structuredreview.Document, env []string, out io.Writer) (structuredreview.Document, error) {
				calls++
				assert.Equal(t, "high", cfg.Reasoning)
				require.Len(t, doc.Findings, 2)
				if mode == "failure" {
					// The configured Codex command isn't installed.
					return tc.Pool.runCICheck(ctx, job, cfg, doc, env, out)
				}
				if ctx.Err() != nil {
					return structuredreview.Document{}, ctx.Err()
				}
				fmt.Fprintln(out, `{"type":"thread.started","thread_id":"checker-session"}`)
				// Capture usage even when the final event has no newline.
				fmt.Fprint(out, `{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":5}}`)
				if mode == "failure after usage" {
					return structuredreview.Document{}, fmt.Errorf("checker failed after reporting usage")
				}
				fmt.Fprintln(out)
				return applyCICheck(doc, jsontext.Value(`{"verdicts":[{"finding":1,"keep":false,"evidence":"build succeeds","reason":"refuted"}]}`), out)
			}
			doc, err := reviewpkg.DecodeStructuredReview(jsontext.Value(raw))
			require.NoError(t, err)
			original := synthesisResult{review: reviewpkg.ReviewResult{Agent: "test", Output: doc.Markdown(threshold), Verdict: storage.VerdictFail, StructuredOutput: jsontext.Value(raw)}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "checker deadline":
				ctx, cancel = context.WithDeadline(t.Context(), time.Unix(0, 0))
				defer cancel()
			case "below severity threshold", "checker canceled":
				cancel()
			}
			if strings.Contains(mode, "canceled") || mode == "checker deadline" || mode == "below severity threshold" {
				if mode == "below severity threshold" {
					doc.Findings = doc.Findings[:1]
					doc.Findings[0].Severity = "low"
					doc.Verdict = structuredreview.VerdictPass
					original.review.Verdict = storage.VerdictPass
					original.review.Output = doc.Markdown(threshold)
					original.review.StructuredOutput, err = json.Marshal(doc)
					require.NoError(t, err)
				}
				tc.Pool.checkAndCompleteSynthesis(ctx, testWorkerID, job, original, doc, snapshot.Check)
			} else {
				tc.Pool.processSynthesisJob(ctx, testWorkerID, job)
			}
			assert.Equal(t, map[bool]int{true: 0, false: 1}[mode == "below severity threshold"], calls)
			finished, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			if mode == "checker canceled" {
				assert.Equal(t, storage.JobStatusRunning, finished.Status)
				return
			}
			assert.Equal(t, storage.JobStatusDone, finished.Status)
			if mode == "failure" {
				var invoked bool
				require.NoError(t, tc.DB.QueryRow("SELECT agent_invoked FROM review_jobs WHERE id = ?", job.ID).Scan(&invoked))
				assert.False(t, invoked)
			}
			if mode == "drop" || mode == "agent drop" || mode == "failure after usage" {
				usage := tokens.ParseJSON(finished.TokenUsage)
				require.NotNil(t, usage)
				assert := assert.New(t)
				if mode == "agent drop" {
					assert.True(usage.HasCost)
					assert.InDelta(0.03, usage.CostUSD, 1e-9)
					assert.EqualValues(15, usage.InputTokens)
					assert.EqualValues(29, usage.OutputTokens)
					assert.Equal("synthesis-session", finished.SessionID)
					assert.Equal("synthesis-session", usage.ThreadID)
					assert.Equal([]string{"synthesis-session"}, usage.ProviderSessionIDs)
					assert.Equal(1, usage.ExpectedProviderSessions)
				} else {
					assert.False(usage.HasCost)
					assert.EqualValues(3, usage.InputTokens)
					assert.EqualValues(5, usage.OutputTokens)
					assert.Empty(finished.SessionID)
					assert.Empty(usage.ThreadID)
					assert.Empty(usage.ProviderSessionIDs)
					assert.Equal(1, usage.ExpectedProviderSessions)
				}
			}
			if mode == "below severity threshold" {
				return
			}
			stored, err := tc.DB.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			structured, err := json.Marshal(stored.StructuredOutput)
			require.NoError(t, err)
			checked, err := reviewpkg.DecodeStructuredReview(structured)
			require.NoError(t, err)
			if strings.HasSuffix(mode, "drop") {
				assert.NotContains(t, stored.Output, "Build claim")
				assert.Contains(t, stored.Output, "Data loss")
				// A remaining finding at the threshold still fails the review.
				if mode == "drop" {
					assert.Equal(t, storage.VerdictFail, stored.Verdict())
					assert.Equal(t, structuredreview.VerdictFail, checked.Verdict)
				} else {
					assert.Equal(t, storage.VerdictPass, stored.Verdict())
					assert.Equal(t, structuredreview.VerdictPass, checked.Verdict)
				}
				require.Len(t, checked.Findings, 1)
				assert.Equal(t, "Data loss", checked.Findings[0].Problem)
			} else {
				assert.Equal(t, original.review.Output, stored.Output)
				assert.Equal(t, original.review.Verdict, stored.Verdict())
				assert.Equal(t, doc, checked)
			}
			if mode == "failure" {
				log, err := os.ReadFile(JobLogPath(job.ID))
				require.NoError(t, err)
				assert.Contains(t, string(log), "CI checker failed; keeping original review: get checker codex")
				assert.Contains(t, string(log), "custom-codex")
			}
		})
	}
}
