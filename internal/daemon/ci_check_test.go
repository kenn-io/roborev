package daemon

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
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
			if len(got.Findings) == 0 {
				assert.Equal(t, "No issues found.", got.Summary)
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
	cfg.CI.CheckReasoning = "high"
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

func TestCICheckFailurePreservesSynthesis(t *testing.T) {
	for _, path := range []string{"passthrough", "agent"} {
		t.Run(path, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			// Resolve the proxy key beside this loaded config, where it is absent.
			tc.Pool.cfgGetter = NewConfigWatcher(filepath.Join(t.TempDir(), "config.toml"), config.DefaultConfig(), nil, nil)
			synthAgent := &synthesisEntrypointTestAgent{name: "checker-failure-" + path, result: `{"schema_version":3,"summary":"Verified","verdict":"fail","findings":[{"severity":"high","kind":"defect","confidence":0.9,"problem":"Nil map write.","fix":"Initialize the map.","file":"alpha.go","start_line":3,"end_line":3,"sources":[1]}]}`}
			agent.RegisterForTest(t, synthAgent)
			type result struct {
				output     string
				structured any
				verdict    *string
			}
			var results []result
			for _, check := range []bool{false, true} {
				run, members, _ := enqueuePanelRun(t, tc, "checker-panel", []memberSpec{{name: "one", agent: "test"}})
				setSynthesisAgent(t, tc, run, synthAgent.name)
				snapshot := synthesisJobConfig{}
				if path == "agent" {
					snapshot.Instructions = "verify"
				}
				if check {
					snapshot.Check = &ciCheckConfig{}
				}
				raw, err := snapshot.encode()
				require.NoError(t, err)
				_, err = tc.DB.Exec("UPDATE review_jobs SET panel_member_config_json = ? WHERE panel_run_uuid = ? AND panel_role = 'synthesis'", raw, run)
				require.NoError(t, err)
				markMemberRunning(t, tc, members[0].ID)
				require.NoError(t, tc.DB.CompleteJobResult(members[0].ID, "test", storage.ReviewCompletion{Output: "high finding", Verdict: storage.VerdictFail, StructuredOutput: jsontext.Value(synthAgent.result)}))
				job := releaseAndClaimSynthesis(t, tc, run)
				tc.Pool.processSynthesisJob(t.Context(), testWorkerID, job)
				review, err := tc.DB.GetReviewByJobID(job.ID)
				require.NoError(t, err)
				finished, err := tc.DB.GetJobByID(job.ID)
				require.NoError(t, err)
				results = append(results, result{review.Output, review.StructuredOutput, finished.Verdict})
				if check {
					log, err := os.ReadFile(JobLogPath(job.ID))
					require.NoError(t, err)
					assert.Contains(t, string(log), "CI checker failed; keeping original review: read credential proxy key")
				}
			}
			assert.Equal(t, results[0], results[1])
		})
	}
}

func TestCICheckCompletionContext(t *testing.T) {
	for _, mode := range []string{"canceled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			run, members, _ := enqueuePanelRun(t, tc, "context-panel", []memberSpec{{name: "one", agent: "test"}})
			completeMember(t, tc, members[0].ID, "test", "No issues found.")
			job := releaseAndClaimSynthesis(t, tc, run)
			ctx, cancel := context.WithCancel(t.Context())
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Unix(0, 0))
			} else {
				cancel()
			}
			defer cancel()
			tc.Pool.checkAndCompleteSynthesis(ctx, testWorkerID, job, synthesisResult{review: reviewpkg.ReviewResult{Agent: "test", Output: "Original review", Verdict: storage.VerdictPass, StructuredOutput: jsontext.Value(`{"schema_version":2,"summary":"Original review","verdict":"pass","findings":[]}`)}})
			finished, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			if mode == "canceled" {
				assert.Equal(t, storage.JobStatusRunning, finished.Status)
			} else {
				assert.Equal(t, storage.JobStatusDone, finished.Status)
			}
		})
	}
}
