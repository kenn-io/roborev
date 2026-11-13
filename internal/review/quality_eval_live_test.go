//go:build qualityeval

package review

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/tokens"
)

type qualityManifest struct {
	GoVersion                                            string
	CompilerSelection                                    string
	BuildSettings                                        map[string]string
	Modules                                              map[string]string
	Version                                              int
	ProductHead, HarnessHead, HarnessHash, RubricHash    string
	SourceInventoryHash                                  string
	SourceHashes, FixtureHashes                          map[string]string
	Agent, Model, Reasoning, AgentVersion, ExtensionHash string
	Repeats, PlannedInvocations, HarnessRetries          int
	PlanOnly                                             bool
	Limitations                                          []string
}
type qualityRun struct {
	Manifest qualityManifest
	Rows     []qualityObservation
	Usage    map[string]*tokens.Usage
	Seconds  map[string]float64
	Summary  qualitySummary
}

func qualityWriteJSON(name string, value any) error {
	data, err := json.Marshal(value, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	// Replace atomically so cancellation never leaves a partly written manifest.
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

// The tag AND explicit opt-in are required. Ordinary tests never find auth or
// start an agent. The parent launches a consuming child with a fresh HOME;
// inherited account overrides and unrelated credentials do not reach adapters.
func TestReviewQualityEvaluation(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_QUALITY_EVAL") != "1" {
		t.Skip("opt-in live evaluation; see docs/development.md")
	}
	settings, err := qualityCurrentBuildSettings()
	require.NoError(t, err)
	require.NoError(t, qualitySupportedBuildSettings(settings))
	if os.Getenv("ROBOREV_QUALITY_CHILD") != "1" {
		require.NoDirExists(t, os.Getenv("ROBOREV_QUALITY_OUT"), "use a new artifact directory")
		home := t.TempDir()
		if os.Getenv("ROBOREV_QUALITY_PLAN_ONLY") != "1" {
			auth := os.Getenv("ROBOREV_QUALITY_PI_AUTH")
			require.NotEmpty(t, auth, "explicit zai authentication file required")
			require.NoError(t, qualityCopyAuth(auth, home))
		}
		env, err := qualityLiveEnv(home, os.Environ())
		require.NoError(t, err)
		for _, key := range []string{"GOCACHE", "GOMODCACHE"} {
			value, err := qualityCommand(t.Context(), ".", "go", "env", key)
			require.NoError(t, err)
			env = append(env, key+"="+value)
		}
		root, err := qualityCommand(t.Context(), ".", "git", "rev-parse", "--show-toplevel")
		require.NoError(t, err)
		binary := filepath.Join(home, "quality-evaluation.test")
		require.NoError(t, qualityBuildEvaluationBinary(t.Context(), root, os.Getenv("ROBOREV_QUALITY_HARNESS_DIR"), os.Getenv("ROBOREV_QUALITY_OVERLAY"), os.Getenv("ROBOREV_QUALITY_MODFILE"), binary))
		cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestReviewQualityEvaluation$", "-test.v", "-test.timeout=0")
		cmd.Env = append(env, qualitySelectionEnv(settings)...)
		cmd.Env = append(cmd.Env, "ROBOREV_QUALITY_CHILD=1")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		require.NoError(t, cmd.Run())
		resultPath := filepath.Join(os.Getenv("ROBOREV_QUALITY_OUT"), "run.json")
		require.FileExists(t, resultPath, "isolated child must execute the plan, not silently skip")
		data, err := os.ReadFile(resultPath)
		require.NoError(t, err)
		var run qualityRun
		require.NoError(t, json.Unmarshal(data, &run, json.RejectUnknownMembers(true)))
		require.Equal(t, os.Getenv("ROBOREV_QUALITY_PRODUCT_HEAD"), run.Manifest.ProductHead)
		require.Equal(t, os.Getenv("ROBOREV_QUALITY_HARNESS_HEAD"), run.Manifest.HarnessHead)
		require.NotEmpty(t, run.Manifest.SourceInventoryHash, "child manifest must bind the source inventory used to compile it")
		repeats := 3
		if text := os.Getenv("ROBOREV_QUALITY_REPEATS"); text != "" {
			repeats, err = strconv.Atoi(text)
			require.NoError(t, err)
		}
		plan, err := qualityPlan(repeats)
		require.NoError(t, err)
		require.Equal(t, len(plan), run.Manifest.PlannedInvocations)
		require.Len(t, run.Rows, len(plan))
		require.Equal(t, os.Getenv("ROBOREV_QUALITY_PLAN_ONLY") == "1", run.Manifest.PlanOnly)
		return
	}
	qualityRunLive(t)
}

func qualityRunLive(t *testing.T) {
	ctx := t.Context()
	productRoot, err := qualityCommand(ctx, ".", "git", "rev-parse", "--show-toplevel")
	require.NoError(t, err)
	productHead, err := qualityCommand(ctx, productRoot, "git", "rev-parse", "HEAD")
	require.NoError(t, err)
	require.NotEmpty(t, os.Getenv("ROBOREV_QUALITY_PRODUCT_HEAD"), "record and require the intended linked product head")
	require.Equal(t, os.Getenv("ROBOREV_QUALITY_PRODUCT_HEAD"), productHead)
	harnessRoot := os.Getenv("ROBOREV_QUALITY_HARNESS_DIR")
	require.True(t, filepath.IsAbs(harnessRoot), "explicit absolute frozen harness directory required")
	harnessHead, err := qualityCommand(ctx, harnessRoot, "git", "rev-parse", "HEAD")
	require.NoError(t, err)
	require.NotEmpty(t, os.Getenv("ROBOREV_QUALITY_HARNESS_HEAD"))
	require.Equal(t, os.Getenv("ROBOREV_QUALITY_HARNESS_HEAD"), harnessHead)
	repeats := 3
	if text := os.Getenv("ROBOREV_QUALITY_REPEATS"); text != "" {
		repeats, err = strconv.Atoi(text)
		require.NoError(t, err)
	}
	rows, err := qualityPlan(repeats)
	require.NoError(t, err)
	model := os.Getenv("ROBOREV_QUALITY_MODEL")
	if model == "" {
		model = "zai/glm-5.3-flash"
	}
	require.True(t, strings.HasPrefix(model, "zai/"), "initial auth-isolated harness supports the explicitly selected zai provider")
	reasoning := os.Getenv("ROBOREV_QUALITY_REASONING")
	if reasoning == "" {
		reasoning = "max"
	}
	require.Contains(t, agent.ReasoningLevels(), reasoning)
	outdir := os.Getenv("ROBOREV_QUALITY_OUT")
	require.True(t, filepath.IsAbs(outdir), "explicit absolute artifact destination required")
	require.NoError(t, os.MkdirAll(filepath.Dir(outdir), 0o700))
	require.NoError(t, os.Mkdir(outdir, 0o700), "use a new run directory; never overwrite prior observations")
	manifest := qualityManifest{Version: 2, ProductHead: productHead, HarnessHead: harnessHead, Agent: "pi", Model: model, Reasoning: reasoning, Repeats: repeats, PlannedInvocations: len(rows), PlanOnly: os.Getenv("ROBOREV_QUALITY_PLAN_ONLY") == "1", HarnessRetries: 0, Limitations: []string{
		"Two synthetic mechanisms, not evidence of universal review precision.",
		"Semantic matching is externally adjudicated under frozen rubric with actor/method attribution; no automatic LLM judge.",
		"Unmatched or ambiguous findings remain unjudged. Operational failures retain planned denominators and widen bounds.",
		"Wilson intervals describe variability conditional on this fixture set, not arbitrary codebases; repeats may be correlated.",
		"Provider model aliases may change remotely. Usage/cost is unavailable when the provider stream omits it.",
		"No harness retries; adapters/provider SDKs may have their own retries, retained in raw streams when emitted.",
	}}
	// Source bytes are from the tree in which this Go test was linked. Overlay
	// supplies only quality_eval test files; production files must be committed.
	require.NoError(t, qualityCheckHarness(harnessRoot))
	corpus, err := qualityLoadFrozenCorpus(ctx, harnessRoot, harnessHead)
	require.NoError(t, err)
	overlayFile := os.Getenv("ROBOREV_QUALITY_OVERLAY")
	modfile := os.Getenv("ROBOREV_QUALITY_MODFILE")
	if harnessRoot != productRoot {
		require.NotEmpty(t, overlayFile, "declare the exact compiler overlay for dependency binding")
		require.NotEmpty(t, modfile, "declare the exact compiler module file")
	}
	require.NoError(t, qualityCheckCompilationBinding(productRoot, harnessRoot, overlayFile, modfile))
	inputs, err := qualityCollectSourceInputs(ctx, productRoot, overlayFile, modfile, harnessRoot)
	require.NoError(t, err)
	require.NoError(t, qualityValidateSourceInputs(ctx, inputs), "commit selected linked inputs before measuring them")
	require.NoError(t, qualityCheckCompiledSourceInventory(qualityCompiledSourceInventory, inputs))
	manifest.SourceInventoryHash, err = qualitySourceInventoryHash(inputs)
	require.NoError(t, err)
	manifest.SourceHashes = inputs.Hashes
	manifest.Modules = inputs.Modules
	manifest.GoVersion = inputs.GoVersion
	manifest.CompilerSelection = qualityCompiledSelection
	manifest.BuildSettings = map[string]string{}
	settings, err := qualityCurrentBuildSettings()
	require.NoError(t, err)
	for _, setting := range settings {
		manifest.BuildSettings[setting.Key] = setting.Value
	}
	files, err := filepath.Glob(filepath.Join(harnessRoot, "internal", "review", "quality_eval*_test.go"))
	require.NoError(t, err)
	var harnessBytes []byte
	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		harnessBytes = append(harnessBytes, []byte(filepath.Base(file)+"\x00")...)
		harnessBytes = append(harnessBytes, data...)
	}
	manifest.HarnessHash = qualityHash(harnessBytes)
	rubric := corpus.Files["rubric.json"]
	manifest.RubricHash = qualityHash(rubric)
	manifest.FixtureHashes = map[string]string{}
	for _, mechanism := range []string{"invoice", "cursor"} {
		for _, variant := range []string{"vulnerable", "fixed", "safe", "unchanged", "harmless"} {
			data, err := corpus.Fixture(mechanism, variant)
			require.NoError(t, err)
			manifest.FixtureHashes[mechanism+"/"+variant] = qualityHash(data)
		}
	}
	var selected agent.Agent
	if !manifest.PlanOnly {
		command, err := exec.LookPath("pi")
		require.NoError(t, err)
		version, err := qualityCommand(ctx, productRoot, command, "--version")
		require.NoError(t, err)
		manifest.AgentVersion = version
		extension := os.Getenv("ROBOREV_QUALITY_PI_EXTENSION")
		require.True(t, filepath.IsAbs(extension), "explicit installed schema extension entrypoint required")
		data, err := os.ReadFile(extension)
		require.NoError(t, err)
		manifest.ExtensionHash = qualityHash(data)
		pi := agent.NewPiAgent(command)
		pi.JSONSchemaExtension = extension
		pi.LaunchArgs = []string{"--no-session", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files"}
		selected = pi.WithModel(model).WithReasoning(agent.ParseReasoningLevel(reasoning)).WithAgentic(false)
	}
	work := t.TempDir()
	prepared, err := qualityPrepareCases(ctx, work, rows, corpus.Fixture)
	require.NoError(t, err)
	for i := range rows {
		p := prepared[rows[i].Case]
		rows[i].PromptHash = p.Row.PromptHash
		rows[i].FixtureHash = p.Row.FixtureHash
		rows[i].State = "planned"
		require.NoError(t, os.WriteFile(filepath.Join(outdir, rows[i].ID+".prompt.md"), []byte(p.Prompt), 0o600))
	}
	run := qualityRun{Manifest: manifest, Rows: rows, Usage: map[string]*tokens.Usage{}, Seconds: map[string]float64{}}
	save := func() {
		summary, err := qualitySummarize(run.Rows, nil)
		require.NoError(t, err)
		run.Summary = summary
		require.NoError(t, qualityWriteJSON(filepath.Join(outdir, "run.json"), run))
	}
	save()
	t.Logf("frozen plan: product=%s harness=%s agent=pi model=%s reasoning=%s repeats=%d invocations=%d plan_only=%v", productHead, harnessHead, model, reasoning, repeats, len(rows), manifest.PlanOnly)
	if manifest.PlanOnly {
		return
	}
	// Every expected row is persisted BEFORE any model call. A partial or failed
	// run remains inspectable with planned/completed/failed/pending counts.
	for i := range run.Rows {
		row := &run.Rows[i]
		p := prepared[row.Case]
		p.Row = *row
		rawPath := filepath.Join(outdir, row.ID+".jsonl")
		raw, err := os.OpenFile(rawPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		capture := agent.NewSessionCaptureWriter(raw, nil)
		start := time.Now()
		doc, invokeErr := qualityInvoke(ctx, selected, p, capture)
		run.Seconds[row.ID] = time.Since(start).Seconds()
		capture.Flush()
		require.NoError(t, raw.Close())
		row.State = "completed"
		if invokeErr != nil {
			row.State = "failed"
			row.Error = invokeErr.Error()
		} else {
			row.Findings = doc.Findings
			data, err := json.Marshal(doc)
			require.NoError(t, err)
			row.OutputHash = qualityHash(data)
			require.NoError(t, os.WriteFile(filepath.Join(outdir, row.ID+".document.json"), data, 0o600))
		}
		stream, err := os.Open(rawPath)
		require.NoError(t, err)
		usage, usageErr := tokens.ParseSchemaUsage("pi", stream)
		require.NoError(t, stream.Close())
		if usageErr != nil {
			t.Logf("%s: usage unavailable: %v", row.ID, usageErr)
		}
		run.Usage[row.ID] = usage
		save()
		t.Logf("%s: %s (%d findings)", row.ID, row.State, len(row.Findings))
	}
	require.Zero(t, run.Summary.Failed, "operational failures preserved in run.json; do not treat these as model quality successes")
}

// Rescore saved output with attributed, hash-bound external judgments. This
// entrypoint is entirely offline and never starts the evaluation or an agent.
func TestReviewQualityAdjudication(t *testing.T) {
	resultPath := os.Getenv("ROBOREV_QUALITY_RESULTS")
	if resultPath == "" {
		t.Skip("set saved run.json and judgment paths for offline adjudication")
	}
	data, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	var run qualityRun
	require.NoError(t, json.Unmarshal(data, &run, json.RejectUnknownMembers(true)))
	judgmentPath := os.Getenv("ROBOREV_QUALITY_JUDGMENTS")
	require.NotEmpty(t, judgmentPath)
	data, err = os.ReadFile(judgmentPath)
	require.NoError(t, err)
	var judgments qualityAdjudication
	require.NoError(t, json.Unmarshal(data, &judgments, json.RejectUnknownMembers(true)))
	require.NotEmpty(t, strings.TrimSpace(judgments.Method), "record human or external agent source adjudication honestly")
	summary, err := qualityApplyJudgments(run.Rows, judgments)
	require.NoError(t, err)
	require.NoError(t, qualityWriteJSON(filepath.Join(filepath.Dir(resultPath), fmt.Sprintf("adjudicated-v%d.json", qualityScoringVersion)), struct {
		Adjudication qualityAdjudication
		Summary      qualitySummary
	}{judgments, summary}))
	t.Logf("%s", fmt.Sprint(summary))
}
