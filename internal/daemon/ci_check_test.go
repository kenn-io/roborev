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
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestCICheckApply(t *testing.T) {
	doc := structuredreview.Document{SchemaVersion: 3, Summary: "Original summary", Verdict: "fail", Findings: []structuredreview.Finding{
		{Severity: "high", Problem: "first", Fix: "fix"}, {Severity: "low", Problem: "second", Fix: "fix"},
	}}
	for _, tc := range []struct {
		name, raw, summary string
		want               []string
		kept               []int
	}{
		{name: "drop with evidence", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":"build succeeded","reason":"claim refuted"}]}`, summary: "1 finding.", want: []string{"second"}, kept: []int{2}},
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
				tc.want, tc.summary, tc.kept = []string{"first", "second"}, doc.Summary, []int{1, 2}
			}
			var log bytes.Buffer
			got, notes, err := applyCICheck(doc, jsontext.Value(tc.raw), &log)
			require.NoError(t, err)
			problems := make([]string, 0, len(got.Findings))
			for _, f := range got.Findings {
				problems = append(problems, f.Problem)
			}
			assert.Equal(t, tc.want, problems)
			assert.Equal(t, tc.kept, notes.Kept)
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
	cfg.CI.Rewrite = true
	_, rewriteOnly, err := p.buildPanelOpts(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, off.PanelMemberConfigJSON, rewriteOnly.PanelMemberConfigJSON)
	cfg.CI.Rewrite = false
	cfg.CI.Check = true
	cfg.CI.CheckModel = "checker-model"
	cfg.CI.CheckReasoning = " HIGH "
	_, on, err := p.buildPanelOpts(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, `{"instructions":"merge","check":{"model":"checker-model","reasoning":"high"}}`, on.PanelMemberConfigJSON) //nolint:testifylint // Rewrite off preserves the checker snapshot byte for byte.
	var snapshot synthesisJobConfig
	in.pullRequest = prompt.PullRequest{Title: "Export files", Description: "Keep the user's files"}
	cfg.CI.Rewrite = true
	_, rewritten, err := p.buildPanelOpts(t.Context(), in)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(rewritten.PanelMemberConfigJSON), &snapshot))
	require.NotNil(t, snapshot.Check)
	assert.True(t, snapshot.Check.Rewrite)
	assert.Equal(t, in.pullRequest.Title, snapshot.Check.Title)
	assert.Equal(t, in.pullRequest.Description, snapshot.Check.Description)
	in.repoCfg = &config.RepoConfig{CI: config.RepoCIConfig{Rewrite: new(false)}}
	_, repoOff, err := p.buildPanelOpts(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, on.PanelMemberConfigJSON, repoOff.PanelMemberConfigJSON)
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
	// "agent drop" reaches the checker through a synthesis agent and leaves no finding at its threshold; the rest pass one review through.
	for _, mode := range []string{"failure", "drop", "agent drop", "rewrite all kept", "below severity threshold", "empty canceled", "checker canceled", "checker deadline"} {
		t.Run(mode, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			tc.Pool.cfgGetter.Config().CodexCmd = "custom-codex"
			synthAgent := &synthesisEntrypointTestAgent{name: "checker-agent", result: raw}
			agent.RegisterForTest(t, synthAgent)
			run, members, _ := enqueuePanelRun(t, tc, "checker", []memberSpec{{name: "one", agent: "test"}})
			setSynthesisAgent(t, tc, run, synthAgent.name)
			snapshot := synthesisJobConfig{Check: &ciCheckConfig{Reasoning: "high", Rewrite: mode == "rewrite all kept"}}
			if mode == "agent drop" {
				snapshot.Instructions = "verify"
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
				if mode == "rewrite all kept" {
					doc.Findings = append([]structuredreview.Finding(nil), doc.Findings...)
					doc.Findings[0].Problem = "Users cannot build the project."
					doc.Findings[0].Fix = "Keep the build working. **Where:** `build`"
					return doc, nil
				}
				checked, _, err := applyCICheck(doc, jsontext.Value(`{"verdicts":[{"finding":1,"keep":false,"evidence":"build succeeds","reason":"refuted"}]}`), out)
				return checked, err
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
			case "empty canceled", "checker canceled":
				cancel()
			}
			if strings.Contains(mode, "canceled") || mode == "checker deadline" || mode == "below severity threshold" {
				if mode == "empty canceled" {
					doc.Findings = []structuredreview.Finding{}
				}
				if mode == "below severity threshold" {
					doc.Findings = doc.Findings[:1]
					doc.Findings[0].Severity = "low"
				}
				if mode == "empty canceled" || mode == "below severity threshold" {
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
			assert.Equal(t, map[bool]int{true: 0, false: 1}[mode == "empty canceled" || mode == "below severity threshold"], calls)
			finished, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			if mode == "checker canceled" {
				assert.Equal(t, storage.JobStatusRunning, finished.Status)
				return
			}
			assert.Equal(t, storage.JobStatusDone, finished.Status)
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
			} else if mode == "rewrite all kept" {
				assert.Contains(t, stored.Output, "Users cannot build the project.")
				doc.Findings[0].Problem, doc.Findings[0].Fix = checked.Findings[0].Problem, checked.Findings[0].Fix
				assert.Equal(t, doc, checked)
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

func TestCIRewriteRequiresNotesAboveThreshold(t *testing.T) {
	doc := structuredreview.Document{Findings: []structuredreview.Finding{{Severity: "high", Problem: "original", Fix: "original fix"}}}
	for _, tc := range []struct{ name, raw string }{
		{"missing", `{"verdicts":[]}`},
		{"duplicate", `{"verdicts":[{"finding":1,"keep":true,"evidence":"failed","reason":"reproduced"},{"finding":1,"keep":true,"evidence":"failed","reason":"reproduced"}]}`},
		{"blank evidence", `{"verdicts":[{"finding":1,"keep":true,"evidence":" ","reason":"reproduced"}]}`},
		{"blank reason", `{"verdicts":[{"finding":1,"keep":true,"evidence":"failed","reason":" "}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checked, notes, err := applyCICheck(doc, jsontext.Value(tc.raw), io.Discard)
			require.NoError(t, err)
			got, err := (&WorkerPool{}).rewriteCIFindings(t.Context(), &storage.ReviewJob{MinSeverity: "high"}, ciCheckConfig{}, checked, notes, nil, "", io.Discard)
			require.ErrorContains(t, err, "incomplete checker notes")
			assert.Equal(t, checked, got)
		})
	}
	checked, notes, err := applyCICheck(doc, jsontext.Value(`{"verdicts":[{"finding":1,"keep":true,"evidence":"failed","reason":"reproduced"}]}`), io.Discard)
	require.NoError(t, err)
	got, err := (&WorkerPool{}).rewriteCIFindings(t.Context(), &storage.ReviewJob{MinSeverity: "critical"}, ciCheckConfig{}, checked, notes, nil, "", io.Discard)
	require.NoError(t, err)
	assert.Equal(t, doc, got)
}
