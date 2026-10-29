package daemon

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
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
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestCICheckApply(t *testing.T) {
	doc := structuredreview.Document{SchemaVersion: 3, Summary: "Original summary", Verdict: "fail", Findings: []structuredreview.Finding{
		{Severity: "high", Problem: "first", Fix: "fix"}, {Severity: "low", Problem: "second", Fix: "fix"},
	}}
	for _, tc := range []struct {
		name, raw string
		want      []string
		verdict   string
		bad       bool
	}{
		{name: "drop with evidence", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":"build succeeded","reason":"claim refuted"}]}`, want: []string{"second"}, verdict: "pass"},
		{name: "explicit keep", raw: `{"verdicts":[{"finding":1,"keep":true,"evidence":"failed","reason":"reproduced"}]}`, want: []string{"first", "second"}, verdict: "fail"},
		{name: "missing verdict", raw: `{"verdicts":[]}`, want: []string{"first", "second"}, verdict: "fail"},
		{name: "duplicate", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":"ok","reason":"refuted"},{"finding":1,"keep":false,"evidence":"ok","reason":"refuted"}]}`, want: []string{"first", "second"}, verdict: "fail"},
		{name: "empty evidence", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":" ","reason":"refuted"}]}`, want: []string{"first", "second"}, verdict: "fail"},
		{name: "empty reason", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":"ok","reason":""}]}`, want: []string{"first", "second"}, verdict: "fail"},
		{name: "missing keep", raw: `{"verdicts":[{"finding":1,"evidence":"ok","reason":"refuted"}]}`, want: []string{"first", "second"}, verdict: "fail"},
		{name: "all dropped", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":"ok","reason":"refuted"},{"finding":2,"keep":false,"evidence":"ok","reason":"refuted"}]}`, want: []string{}, verdict: "pass"},
		{name: "malformed", raw: `{"verdicts":`, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			got, err := applyCICheck(doc, jsontext.Value(tc.raw), "high", &log)
			if tc.bad {
				require.Error(t, err)
				assert.Equal(t, doc, got)
				return
			}
			require.NoError(t, err)
			problems := make([]string, 0, len(got.Findings))
			for _, f := range got.Findings {
				problems = append(problems, f.Problem)
			}
			assert.Equal(t, tc.want, problems)
			assert.Equal(t, tc.verdict, got.Verdict)
			if len(got.Findings) < len(doc.Findings) {
				if len(got.Findings) == 1 {
					assert.Equal(t, "1 finding.", got.Summary)
				} else {
					assert.Equal(t, "0 findings.", got.Summary)
				}
				assert.NotContains(t, got.Markdown("high"), doc.Summary)
				if len(got.Findings) == 0 {
					assert.Equal(t, 1, strings.Count(got.Markdown("high"), "No issues found."))
				}
			} else {
				assert.Equal(t, doc.Summary, got.Summary)
			}
			if len(got.Findings) != 2 {
				assert.Contains(t, log.String(), `"keep":false`)
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

func TestCICheckMiseShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixture")
	}
	tools := t.TempDir()
	installed := filepath.Join(tools, "installed", "codex")
	require.NoError(t, os.Mkdir(filepath.Dir(installed), 0o700))
	require.NoError(t, os.WriteFile(installed, []byte("native fixture"), 0o700))
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1 $2\" = 'which codex' ] || exit 1\nprintf '%%s\\n' '%s'\n", installed)
	require.NoError(t, os.WriteFile(filepath.Join(tools, "mise"), []byte(script), 0o700))
	require.NoError(t, os.Symlink("mise", filepath.Join(tools, "codex")))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := resolveCICheckCommand(t.Context(), t.TempDir(), "codex")
	require.NoError(t, err)
	assert.Equal(t, installed, got)
}

func TestCICheckStoredReview(t *testing.T) {
	for _, path := range []string{"passthrough", "agent"} {
		t.Run(path, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			synthAgent := &synthesisEntrypointTestAgent{name: "checker-" + path, result: `{"schema_version":3,"summary":"Two claims","verdict":"fail","findings":[{"severity":"high","kind":"defect","confidence":0.9,"problem":"Build claim","fix":"Fix build","sources":[1]},{"severity":"medium","kind":"defect","confidence":0.9,"problem":"Data loss","fix":"Preserve data","sources":[1]}]}`}
			agent.RegisterForTest(t, synthAgent)
			type result struct {
				output     string
				structured any
				verdict    *string
			}
			results := make(map[string]result)
			for _, mode := range []string{"off", "failure", "drop"} {
				run, members, _ := enqueuePanelRun(t, tc, "checker-"+mode, []memberSpec{{name: "one", agent: "test"}})
				setSynthesisAgent(t, tc, run, synthAgent.name)
				snapshot := synthesisJobConfig{}
				if path == "agent" {
					snapshot.Instructions = "verify"
				}
				if mode != "off" {
					snapshot.Check = &ciCheckConfig{Reasoning: "high"}
				}
				raw, err := snapshot.encode()
				require.NoError(t, err)
				_, err = tc.DB.Exec("UPDATE review_jobs SET panel_member_config_json = ?, min_severity = 'high' WHERE panel_run_uuid = ? AND panel_role = 'synthesis'", raw, run)
				require.NoError(t, err)
				markMemberRunning(t, tc, members[0].ID)
				require.NoError(t, tc.DB.CompleteJobResult(members[0].ID, "test", storage.ReviewCompletion{Output: "two claims", Verdict: storage.VerdictFail, StructuredOutput: jsontext.Value(synthAgent.result)}))
				job := releaseAndClaimSynthesis(t, tc, run)
				calls := 0
				tc.Pool.ciCheck = func(_ context.Context, got *storage.ReviewJob, cfg ciCheckConfig, doc structuredreview.Document, _ agent.CodexSandbox, _ []string, out io.Writer) (structuredreview.Document, error) {
					calls++
					assert.Equal(t, job.ID, got.ID)
					assert.Equal(t, "high", got.MinSeverity)
					assert.Equal(t, "high", cfg.Reasoning)
					require.Len(t, doc.Findings, 2)
					if mode == "failure" {
						return structuredreview.Document{}, fmt.Errorf("forced checker failure")
					}
					return applyCICheck(doc, jsontext.Value(`{"verdicts":[{"finding":1,"keep":false,"evidence":"build succeeds","reason":"refuted"},{"finding":2,"keep":true,"evidence":"data lost","reason":"reproduced"}]}`), got.MinSeverity, out)
				}
				tc.Pool.processSynthesisJob(t.Context(), testWorkerID, job)
				assert.Equal(t, map[bool]int{true: 0, false: 1}[mode == "off"], calls)
				stored, err := tc.DB.GetReviewByJobID(job.ID)
				require.NoError(t, err)
				finished, err := tc.DB.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Equal(t, storage.JobStatusDone, finished.Status)
				results[mode] = result{stored.Output, stored.StructuredOutput, finished.Verdict}
				if mode == "failure" {
					log, err := os.ReadFile(JobLogPath(job.ID))
					require.NoError(t, err)
					assert.Contains(t, string(log), "CI checker failed; keeping original review: forced checker failure")
				}
				if mode == "drop" {
					encoded, err := json.Marshal(stored.StructuredOutput)
					require.NoError(t, err)
					doc, err := reviewpkg.DecodeStructuredReview(encoded)
					require.NoError(t, err)
					require.Len(t, doc.Findings, 1)
					assert.Equal(t, "Data loss", doc.Findings[0].Problem)
					assert.Equal(t, structuredreview.VerdictPass, doc.Verdict)
					assert.NotContains(t, stored.Output, "Build claim")
					assert.Equal(t, doc.Markdown("high"), stored.Output)
					assert.Equal(t, storage.VerdictPass, stored.Verdict())
				}
			}
			assert.Equal(t, results["off"], results["failure"])
		})
	}
}

// The checker-off path keeps its cancellation behavior, and an expired
// deadline during the check still stores the original review.
func TestCICheckCompletionContext(t *testing.T) {
	raw := jsontext.Value(`{"schema_version":3,"source_labels":["test"],"summary":"Original","verdict":"fail","findings":[{"severity":"high","problem":"Data loss","fix":"Preserve data","sources":[1]}]}`)
	doc, err := reviewpkg.DecodeStructuredReview(raw)
	require.NoError(t, err)
	original := synthesisResult{review: reviewpkg.ReviewResult{Agent: "test", Output: doc.Markdown(""), Verdict: storage.VerdictFail, StructuredOutput: raw}}
	for _, mode := range []string{"off canceled", "checker canceled", "checker deadline"} {
		t.Run(mode, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			run, members, _ := enqueuePanelRun(t, tc, "context-panel", []memberSpec{{name: "one", agent: "test"}})
			completeMember(t, tc, members[0].ID, "test", "finding")
			job := releaseAndClaimSynthesis(t, tc, run)
			if mode != "off canceled" {
				job.PanelMemberConfigJSON = `{"check":{}}`
			}
			tc.Pool.ciCheck = func(ctx context.Context, _ *storage.ReviewJob, _ ciCheckConfig, _ structuredreview.Document, _ agent.CodexSandbox, _ []string, _ io.Writer) (structuredreview.Document, error) {
				return structuredreview.Document{}, ctx.Err()
			}
			ctx, cancel := context.WithCancel(t.Context())
			if mode == "checker deadline" {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Unix(0, 0))
			} else {
				cancel()
			}
			defer cancel()
			tc.Pool.checkAndCompleteSynthesis(ctx, testWorkerID, job, original)
			finished, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			if mode == "checker canceled" {
				assert.Equal(t, storage.JobStatusRunning, finished.Status)
				return
			}
			assert.Equal(t, storage.JobStatusDone, finished.Status)
			stored, err := tc.DB.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, original.review.Output, stored.Output)
			encoded, err := json.Marshal(stored.StructuredOutput)
			require.NoError(t, err)
			assert.JSONEq(t, string(raw), string(encoded))
		})
	}
}
