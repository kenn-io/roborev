package daemon

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
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
	"go.kenn.io/roborev/internal/streamfmt"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestCICheckApply(t *testing.T) {
	doc := structuredreview.Document{SchemaVersion: 3, Summary: "Original summary", Verdict: "fail", Findings: []structuredreview.Finding{
		{Severity: "high", Problem: "first", Fix: "fix"}, {Severity: "low", Problem: "second", Fix: "fix"},
	}}
	for _, tc := range []struct {
		name, raw string
		want      []string
	}{
		{name: "drop with evidence", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":"build succeeded","reason":"claim refuted"}]}`, want: []string{"second"}},
		{name: "explicit keep", raw: `{"verdicts":[{"finding":1,"keep":true,"evidence":"failed","reason":"reproduced"}]}`, want: []string{"first", "second"}},
		{name: "missing verdict", raw: `{"verdicts":[]}`, want: []string{"first", "second"}},
		{name: "duplicate", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":"ok","reason":"refuted"},{"finding":1,"keep":false,"evidence":"ok","reason":"refuted"}]}`, want: []string{"first", "second"}},
		{name: "empty evidence", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":" ","reason":"refuted"}]}`, want: []string{"first", "second"}},
		{name: "empty reason", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":"ok","reason":""}]}`, want: []string{"first", "second"}},
		{name: "missing keep", raw: `{"verdicts":[{"finding":1,"evidence":"ok","reason":"refuted"}]}`, want: []string{"first", "second"}},
		{name: "all dropped", raw: `{"verdicts":[{"finding":1,"keep":false,"evidence":"ok","reason":"refuted"},{"finding":2,"keep":false,"evidence":"ok","reason":"refuted"}]}`, want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			got, err := applyCICheck(doc, jsontext.Value(tc.raw), &log)
			require.NoError(t, err)
			problems := make([]string, 0, len(got.Findings))
			for _, f := range got.Findings {
				problems = append(problems, f.Problem)
			}
			assert.Equal(t, tc.want, problems)
			if len(got.Findings) < len(doc.Findings) {
				if len(got.Findings) == 1 {
					assert.Equal(t, "1 finding.", got.Summary)
				} else {
					assert.Equal(t, "No issues found.", got.Summary)
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
	// A reloaded invalid setting leaves the review unchecked rather than blocking the panel.
	in.repoCfg = nil
	cfg.CI.CheckReasoning = "invalid"
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	_, invalid, err := p.buildPanelOpts(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, off.PanelMemberConfigJSON, invalid.PanelMemberConfigJSON)
	assert.Contains(t, logs.String(), "CI checker disabled")
}

func TestCICheckConfiguredCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixture")
	}
	tc := newWorkerTestContext(t, 1)
	tools := t.TempDir()
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	cfg := config.DefaultConfig()
	cfg.CodexCmd = "custom-codex"
	tc.Pool.cfgGetter = NewStaticConfig(cfg)
	t.Setenv("PATH", tools)
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprint(installed), func(t *testing.T) {
			if installed {
				require.NoError(t, os.WriteFile(filepath.Join(tools, cfg.CodexCmd), []byte("native fixture"), 0o700))
			}
			_, err := tc.Pool.runCICheck(t.Context(), &storage.ReviewJob{RepoPath: tc.TmpDir, GitRef: "HEAD"}, ciCheckConfig{}, structuredreview.Document{}, nil, io.Discard)
			require.Error(t, err)
			if installed {
				// PATH contains the custom command but no git, so repository resolution fails.
				assert.Contains(t, err.Error(), "resolve review repository")
			} else {
				assert.Contains(t, err.Error(), "custom-codex")
				entries, err := os.ReadDir(scratch)
				require.NoError(t, err)
				assert.Empty(t, entries)
			}
		})
	}
}

func TestCICheckStoredReview(t *testing.T) {
	for _, path := range []string{"passthrough", "agent"} {
		t.Run(path, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			synthAgent := &synthesisEntrypointTestAgent{name: "checker-" + path, result: `{"schema_version":3,"summary":"Two claims","verdict":"pass","findings":[{"severity":"high","kind":"defect","confidence":0.9,"problem":"Build claim","fix":"Fix build","sources":[1]},{"severity":"medium","kind":"defect","confidence":0.9,"problem":"Data loss","fix":"Preserve data","sources":[1]}]}`}
			agent.RegisterForTest(t, synthAgent)
			for _, mode := range []string{"off", "drop"} {
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
				_, updates, unsubscribe := tc.Pool.outputBuffers.Subscribe(job.ID)
				defer unsubscribe()
				tc.Pool.ciCheck = func(_ context.Context, got *storage.ReviewJob, cfg ciCheckConfig, doc structuredreview.Document, _ []string, out io.Writer) (structuredreview.Document, error) {
					calls++
					select {
					case _, open := <-updates:
						require.True(t, open, "synthesis stream closed before checking")
					default:
					}
					fmt.Fprintln(out, "checker progress")
					var progress OutputLine
					select {
					case progress = <-updates:
					default:
					}
					assert.Equal(t, "checker progress", progress.Text)

					assert.Equal(t, job.ID, got.ID)
					assert.Equal(t, job.MinSeverity, got.MinSeverity)
					assert.Equal(t, "high", cfg.Reasoning)
					require.Len(t, doc.Findings, 2)
					return applyCICheck(doc, jsontext.Value(`{"verdicts":[{"finding":1,"keep":false,"evidence":"build succeeds","reason":"refuted"},{"finding":2,"keep":true,"evidence":"data lost","reason":"reproduced"}]}`), out)
				}
				tc.Pool.processSynthesisJob(t.Context(), testWorkerID, job)
				assert.Equal(t, map[bool]int{true: 0, false: 1}[mode == "off"], calls)
				stored, err := tc.DB.GetReviewByJobID(job.ID)
				require.NoError(t, err)
				finished, err := tc.DB.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Equal(t, storage.JobStatusDone, finished.Status)
				if mode == "off" {
					assert.Contains(t, stored.Output, "Two claims")
					assert.Contains(t, stored.Output, "Build claim")
					assert.Contains(t, stored.Output, "Data loss")
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
		})
	}
}

func TestCICheckLog(t *testing.T) {
	for _, provider := range []string{"codex", "claude-code", "gemini", "grok", "opencode", "pi"} {
		t.Run(provider, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			run, members, _ := enqueuePanelRun(t, tc, "checker-log", []memberSpec{{name: "one", agent: "test"}})
			completeMember(t, tc, members[0].ID, "test", "finding")
			job := releaseAndClaimSynthesis(t, tc, run)
			job.Agent = provider
			_, updates, unsubscribe := tc.Pool.outputBuffers.Subscribe(job.ID)
			defer unsubscribe()
			tc.Pool.ciCheck = func(_ context.Context, _ *storage.ReviewJob, _ ciCheckConfig, doc structuredreview.Document, _ []string, out io.Writer) (structuredreview.Document, error) {
				// Include a split record and an unterminated tail, as subprocess writes can do.
				fmt.Fprint(out, `{"type":"item.started","item":{"type":"command_execution",`)
				fmt.Fprintln(out, `"command":"go test ./example"}}`)
				fmt.Fprint(out, `{"type":"item.completed","item":{"type":"agent_message","text":"Checker progress"}}`)
				return doc, nil
			}
			doc := structuredreview.Document{Findings: []structuredreview.Finding{{Problem: "claim"}}}
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
			var rendered bytes.Buffer
			formatter := streamfmt.New(&rendered, true, streamfmt.DecoderForAgent(provider))
			require.NoError(t, streamfmt.RenderLogWith(strings.NewReader(string(log)), formatter))
			assert.Contains(t, rendered.String(), "go test ./example")
			assert.Contains(t, rendered.String(), "Checker progress")
		})
	}
}

func TestCICheckCompletion(t *testing.T) {
	raw := jsontext.Value(`{"schema_version":3,"source_labels":["test"],"summary":"Original","verdict":"pass","findings":[{"severity":"high","problem":"Build claim","fix":"Fix build","sources":[1]},{"severity":"medium","problem":"Data loss","fix":"Preserve data","sources":[1]}]}`)
	doc, err := reviewpkg.DecodeStructuredReview(raw)
	require.NoError(t, err)
	original := synthesisResult{review: reviewpkg.ReviewResult{Agent: "test", Output: doc.Markdown(""), Verdict: storage.VerdictPass, StructuredOutput: raw}}
	for _, mode := range []string{"failure", "keep", "below threshold", "empty canceled", "checker canceled", "checker deadline"} {
		t.Run(mode, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			run, members, _ := enqueuePanelRun(t, tc, "context-panel", []memberSpec{{name: "one", agent: "test"}})
			completeMember(t, tc, members[0].ID, "test", "finding")
			job := releaseAndClaimSynthesis(t, tc, run)
			job.MinSeverity = "high"
			if mode == "below threshold" {
				job.MinSeverity = "critical"
			}
			calls := 0
			tc.Pool.ciCheck = func(ctx context.Context, _ *storage.ReviewJob, _ ciCheckConfig, got structuredreview.Document, _ []string, out io.Writer) (structuredreview.Document, error) {
				calls++
				require.Len(t, got.Findings, 2)
				switch mode {
				case "failure":
					return structuredreview.Document{}, fmt.Errorf("forced checker failure")
				case "keep":
					return applyCICheck(got, jsontext.Value(`{"verdicts":[{"finding":1,"keep":true,"evidence":"failed","reason":"reproduced"},{"finding":2,"keep":true,"evidence":"failed","reason":"reproduced"}]}`), out)
				case "below threshold":
					return applyCICheck(got, jsontext.Value(`{"verdicts":[{"finding":1,"keep":false,"evidence":"build succeeds","reason":"refuted"},{"finding":2,"keep":true,"evidence":"data lost","reason":"reproduced"}]}`), out)
				default:
					return structuredreview.Document{}, ctx.Err()
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			switch mode {
			case "checker deadline":
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Unix(0, 0))
			case "empty canceled", "checker canceled":
				cancel()
			}
			defer cancel()
			inputDoc, input := doc, original
			if mode == "empty canceled" {
				inputDoc.Findings = nil
				inputDoc.Verdict = structuredreview.VerdictPass
				input.review.StructuredOutput, err = json.Marshal(inputDoc)
				require.NoError(t, err)
				input.review.Output = inputDoc.Markdown("")
			}
			tc.Pool.checkAndCompleteSynthesis(ctx, testWorkerID, job, input, inputDoc, &ciCheckConfig{})
			if mode == "empty canceled" {
				assert.Zero(t, calls)
			} else {
				assert.Equal(t, 1, calls)
			}
			finished, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			if mode == "checker canceled" {
				assert.Equal(t, storage.JobStatusRunning, finished.Status)
				return
			}
			assert.Equal(t, storage.JobStatusDone, finished.Status)
			stored, err := tc.DB.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			if mode == "failure" {
				log, err := os.ReadFile(JobLogPath(job.ID))
				require.NoError(t, err)
				assert.Contains(t, string(log), "CI checker failed; keeping original review: forced checker failure")
			}
			if mode == "below threshold" {
				assert.NotContains(t, stored.Output, "Build claim")
				assert.Contains(t, stored.Output, "Data loss")
				assert.Equal(t, storage.VerdictPass, stored.Verdict())
				return
			}
			assert.Equal(t, input.review.Output, stored.Output)
			assert.Equal(t, input.review.Verdict, stored.Verdict())
			encoded, err := json.Marshal(stored.StructuredOutput)
			require.NoError(t, err)
			assert.JSONEq(t, string(input.review.StructuredOutput), string(encoded))
		})
	}
}
