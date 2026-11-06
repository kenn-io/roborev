package review

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
)

func TestQualityPlanIncludesEveryCaseAndRepeat(t *testing.T) {
	rows, err := qualityPlan(3)
	require.NoError(t, err)
	require.Len(t, rows, 24)
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.Case]++
	}
	assert.Equal(t, map[string]int{"invoice/vulnerable": 3, "invoice/fixed": 3, "invoice/safe": 3, "cursor/vulnerable": 3, "cursor/fixed": 3, "cursor/safe": 3, "synthesis/true_duplicate": 3, "synthesis/two_distinct": 3}, counts)
	_, err = qualityPlan(0)
	require.Error(t, err)
}

func TestQualityRunnerUsesRealReviewAndSynthesis(t *testing.T) {
	rows, err := qualityPlan(1)
	require.NoError(t, err)
	require.Len(t, rows, 8)
	for _, index := range []int{3, 7} {
		row := rows[index]
		prepared, err := qualityPrepare(t.Context(), t.TempDir(), row)
		require.NoError(t, err)
		a := agent.NewTestAgent()
		a.Output = `{"schema_version":3,"summary":"Boundary issue.","verdict":"fail","findings":[{"severity":"medium","kind":"defect","confidence":0.9,"problem":"The exclusive cursor includes equal values.","fix":"Return strictly greater values.","file":"cursor.go","start_line":7,"end_line":7,"sources":[1]}]}`
		var stream bytes.Buffer
		doc, err := qualityInvoke(t.Context(), a, prepared, &stream)
		require.NoError(t, err)
		require.Len(t, doc.Findings, 1)
		assert.Equal(t, "The exclusive cursor includes equal values.", doc.Findings[0].Problem)
		assert.Equal(t, qualityHash([]byte(prepared.Prompt)), prepared.Row.PromptHash)
		// Invalid provider output is a failed invocation, never a clean review.
		a.Output = `"Not a review document."`
		_, err = qualityInvoke(t.Context(), a, prepared, &stream)
		require.Error(t, err)
	}
}

func TestQualityPrepareRepeatPromptsAreIdentical(t *testing.T) {
	rows, err := qualityPlan(2)
	require.NoError(t, err)
	require.Len(t, rows, 16)
	first, err := qualityPrepare(t.Context(), t.TempDir(), rows[0])
	require.NoError(t, err)
	second, err := qualityPrepare(t.Context(), t.TempDir(), rows[8])
	require.NoError(t, err)
	assert.Equal(t, first.Row.PromptHash, second.Row.PromptHash)
	assert.Equal(t, first.GitRef, second.GitRef)
}

func TestQualityAdjudicationBindsOutputAndActor(t *testing.T) {
	rows := []qualityObservation{{ID: "invoice-1", OutputHash: "current", Expected: []string{"invoice"}, Findings: make([]StructuredFinding, 1)}}
	input := qualityAdjudication{Actor: "independent-source-reviewer", Method: "external agent source review", Entries: map[string]qualityJudgedRow{"invoice-1": {OutputHash: "stale", Judgments: []qualityJudgment{{Finding: 1, Mechanisms: []string{"invoice"}, Reason: "Ownership omitted."}}}}}
	_, err := qualityApplyJudgments(rows, input)
	require.Error(t, err)
	current := input.Entries["invoice-1"]
	current.OutputHash = "current"
	input.Entries["invoice-1"] = current
	got, err := qualityApplyJudgments(rows, input)
	require.NoError(t, err)
	assert.Equal(t, 1, got.Recall.Numerator)
	input.Actor = ""
	_, err = qualityApplyJudgments(rows, input)
	require.Error(t, err)
}

func TestQualityLiveChildEnvironmentUsesOnlyExplicitAuth(t *testing.T) {
	home := t.TempDir()
	env, err := qualityLiveEnv(home, []string{"PATH=/bin", "CODEX_HOME=private", "CLAUDE_CONFIG_DIR=private", "PI_CODING_AGENT_DIR=private", "UNRELATED_SECRET=secret", "ROBOREV_QUALITY_MODEL=zai/test", "ROBOREV_RUN_QUALITY_EVAL=1"})
	require.NoError(t, err)
	assert.NotContains(t, env, "UNRELATED_SECRET=secret")
	assert.NotContains(t, env, "CODEX_HOME=private")
	assert.NotContains(t, env, "CLAUDE_CONFIG_DIR=private")
	assert.NotContains(t, env, "PI_CODING_AGENT_DIR=private")
	assert.Contains(t, env, "HOME="+home)
	assert.Contains(t, env, "ROBOREV_RUN_QUALITY_EVAL=1")
	authFile := filepath.Join(t.TempDir(), "auth.json")
	require.NoError(t, os.WriteFile(authFile, []byte(`{"zai":{"type":"api_key","key":"synthetic-key"},"another-provider":{"key":"excluded"}}`), 0o600))
	require.NoError(t, qualityCopyAuth(authFile, home))
	content, err := os.ReadFile(filepath.Join(home, ".pi", "agent", "auth.json"))
	require.NoError(t, err)
	var auth map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(content, &auth))
	assert.Len(t, auth, 1)
	assert.JSONEq(t, `{"type":"api_key","key":"synthetic-key"}`, string(auth["zai"]))
}

func TestQualitySourceGuardRejectsUndeclaredProductFiles(t *testing.T) {
	for _, change := range []string{"untracked", "modified", "ignored_go", "untracked_template", "ignored_template", "untracked_analyze", "ignored_analyze", "unlinked_metadata"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			_, err := qualityCommand(t.Context(), root, "git", "init", "-q")
			require.NoError(t, err)
			_, err = qualityCommand(t.Context(), root, "git", "config", "user.name", "Fixture Reviewer")
			require.NoError(t, err)
			_, err = qualityCommand(t.Context(), root, "git", "config", "user.email", "reviewer@example.test")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/guard\n\ngo 1.27.3\n"), 0o600))
			_, err = qualityCommand(t.Context(), root, "git", "add", "go.mod")
			require.NoError(t, err)
			dir := filepath.Join(root, "internal", "review")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			tracked := filepath.Join(dir, "owner.go")
			require.NoError(t, os.WriteFile(tracked, []byte("package review\nimport _ \"example.test/guard/internal/prompt\"\n"), 0o600))
			for name, data := range map[string]string{
				"internal/prompt/owner.go":                    "package prompt\nimport (\"embed\";_ \"example.test/guard/internal/prompt/analyze\")\n//go:embed templates/*.md.gotmpl\nvar Templates embed.FS\n",
				"internal/prompt/templates/initial.md.gotmpl": "initial prompt",
				"internal/prompt/analyze/owner.go":            "package analyze\nimport \"embed\"\n//go:embed *.txt\nvar Templates embed.FS\n",
				"internal/prompt/analyze/initial.txt":         "initial analyze prompt",
			} {
				path := filepath.Join(root, name)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
			}
			_, err = qualityCommand(t.Context(), root, "git", "add", "internal/review/owner.go", "internal/prompt")
			require.NoError(t, err)
			_, err = qualityCommand(t.Context(), root, "git", "commit", "-q", "-m", "test: record fixture source")
			require.NoError(t, err)
			require.NoError(t, qualityCheckSource(t.Context(), root))
			name := tracked
			switch change {
			case "untracked":
				name = filepath.Join(dir, "unrecorded.go")
			case "ignored_go":
				name = filepath.Join(dir, "ignored.go")
				require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.go\n"), 0o600))
			case "untracked_analyze", "ignored_analyze":
				name = filepath.Join(root, "internal", "prompt", "analyze", "extra.txt")
				if change == "ignored_analyze" {
					require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.txt\n"), 0o600))
				}
			case "unlinked_metadata":
				name = filepath.Join(dir, ".hegel", "example.data")
				require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".hegel/\n"), 0o600))
			case "untracked_template", "ignored_template":
				name = filepath.Join(root, "internal", "prompt", "templates", "extra.md.gotmpl")
				if change == "ignored_template" {
					require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.md.gotmpl\n"), 0o600))
				}
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
			require.NoError(t, os.WriteFile(name, []byte("package review\nvar undeclared = 1\n"), 0o600))
			if change == "unlinked_metadata" {
				require.NoError(t, qualityCheckSource(t.Context(), root))
			} else {
				require.Error(t, qualityCheckSource(t.Context(), root), "%s code must not be mislabeled as the recorded product head", change)
			}
		})
	}
}

func TestQualityAdjudicationRequiresMethod(t *testing.T) {
	for _, method := range []string{"", " ", "\t\r\n", " \u2003 "} {
		_, err := qualityApplyJudgments(nil, qualityAdjudication{Actor: "independent-reviewer", Method: method})
		require.Error(t, err, "blank method must never imply human or external source review")
	}
	for _, method := range []string{"human source review", "external agent source review"} {
		_, err := qualityApplyJudgments(nil, qualityAdjudication{Actor: "independent-reviewer", Method: method})
		require.NoError(t, err)
	}
}

func qualityLinkedFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.sum": "",
		"go.mod": "module example.test/guard\n\ngo 1.27.3\nrequire (\nexample.test/dependency v0.0.0\nexample.test/unreferenced v0.0.0\n)\nreplace example.test/dependency => ./dependency\nreplace example.test/unreferenced => ./unreferenced\n",
		"internal/review/owner.go": `package review
import (_ "example.test/guard/internal/git";_ "example.test/guard/internal/procutil";_ "example.test/guard/internal/storage";_ "example.test/dependency")
`,
		"internal/git/dep.go":      "package git\nimport \"embed\"\n//go:embed *.txt\nvar Notes embed.FS\n",
		"internal/git/keep.txt":    "another selected embedded input\n",
		"internal/git/note.txt":    "source-owned prompt input\n",
		"internal/procutil/dep.go": "package procutil\n",
		"internal/storage/keep.go": "package storage\n",
		"internal/storage/dep.go":  "package storage\n",
		"dependency/go.mod":        "module example.test/dependency\n\ngo 1.27.3\n",
		"dependency/dep.go":        "package dependency\n",
		"unreferenced/go.mod":      "module example.test/unreferenced\n\ngo 1.27.3\n",
		".gitignore":               "ignored.go\nextra.txt\n.hegel/\n",
	}
	for name, data := range files {
		p := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	}
	for _, args := range [][]string{{"git", "init", "-q"}, {"git", "config", "user.name", "Fixture Reviewer"}, {"git", "config", "user.email", "reviewer@example.test"}, {"git", "add", "."}, {"git", "commit", "-q", "-m", "test: record linked input graph"}} {
		_, err := qualityCommand(t.Context(), root, args...)
		require.NoError(t, err)
	}
	return root
}

func TestQualitySourceGuardCoversLinkedDependencies(t *testing.T) {
	for _, tc := range []struct{ Dir, Change string }{
		{"internal/git", "modified"}, {"internal/procutil", "staged"}, {"internal/storage", "deleted"}, {"internal/git", "untracked"}, {"internal/procutil", "ignored"}, {"internal/git", "embedded"}, {"internal/git", "deleted_embed"}, {"dependency", "modified"}, {"dependency", "module_metadata"}, {".", "sum_deleted"},
	} {
		t.Run(tc.Dir+"/"+tc.Change, func(t *testing.T) {
			root := qualityLinkedFixture(t)
			require.NoError(t, qualityCheckSource(t.Context(), root))
			name := filepath.Join(root, tc.Dir, "dep.go")
			switch tc.Change {
			case "sum_deleted":
				require.NoError(t, os.Remove(filepath.Join(root, "go.sum")))
			case "deleted_embed":
				require.NoError(t, os.Remove(filepath.Join(root, tc.Dir, "note.txt")))
			case "deleted":
				require.NoError(t, os.Remove(name))
			case "untracked":
				name = filepath.Join(root, tc.Dir, "extra.go")
				require.NoError(t, os.WriteFile(name, []byte("package git\nvar added=1\n"), 0o600))
			case "ignored":
				name = filepath.Join(root, tc.Dir, "ignored.go")
				require.NoError(t, os.WriteFile(name, []byte("package procutil\nvar added=1\n"), 0o600))
			case "embedded":
				require.NoError(t, os.WriteFile(filepath.Join(root, tc.Dir, "extra.txt"), []byte("unrecorded embedded prompt"), 0o600))
			case "module_metadata":
				name = filepath.Join(root, tc.Dir, "go.mod")
				require.NoError(t, os.WriteFile(name, []byte("module example.test/dependency\n\ngo 1.27.3\n// undeclared metadata edit\n"), 0o600))
			default:
				data, err := os.ReadFile(name)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(name, append(data, []byte("var altered=1\n")...), 0o600))
				if tc.Change == "staged" {
					_, err = qualityCommand(t.Context(), root, "git", "add", name)
					require.NoError(t, err)
				}
			}
			require.Error(t, qualityCheckSource(t.Context(), root), "linked dependency input must not escape the recorded product identity")
		})
	}
}

func TestQualitySourceInventoryIncludesModulesAndEmbeds(t *testing.T) {
	root := qualityLinkedFixture(t)
	inputs, err := qualityCollectSourceInputs(t.Context(), root, "", "", root)
	require.NoError(t, err)
	require.NoError(t, qualityValidateSourceInputs(t.Context(), inputs))
	assert.NotEmpty(t, inputs.ModuleCacheRoot)
	for _, name := range []string{"internal/git/dep.go", "internal/procutil/dep.go", "internal/storage/dep.go", "internal/git/note.txt", "go.mod", "module:example.test/dependency@v0.0.0/dep.go", "module:example.test/dependency@v0.0.0/go.mod"} {
		assert.Contains(t, inputs.Hashes, name)
	}
	assert.Contains(t, inputs.Modules, "example.test/dependency")
	assert.Contains(t, inputs.Modules, "example.test/unreferenced")
	assert.Contains(t, inputs.SnapshotData, "module:example.test/dependency@v0.0.0/dep.go")
	assert.Contains(t, inputs.Hashes, "module:example.test/unreferenced@v0.0.0/go.mod")
	before := inputs.Hashes["internal/git/note.txt"]
	require.NoError(t, os.WriteFile(filepath.Join(root, "internal", "git", "note.txt"), []byte("changed embedded source"), 0o600))
	after, err := qualityCollectSourceInputs(t.Context(), root, "", "", root)
	require.NoError(t, err)
	assert.NotEqual(t, before, after.Hashes["internal/git/note.txt"])
}

func TestQualitySourceInventoryCapturesSelectedModfileAndSum(t *testing.T) {
	root := qualityLinkedFixture(t)
	modData, err := os.ReadFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	modfile := filepath.Join(root, "selected.mod")
	sumfile := filepath.Join(root, "selected.sum")
	require.NoError(t, os.WriteFile(modfile, modData, 0o600))
	sumData := []byte("")
	require.NoError(t, os.WriteFile(sumfile, sumData, 0o600))
	_, err = qualityCommand(t.Context(), root, "git", "add", "selected.mod", "selected.sum")
	require.NoError(t, err)
	_, err = qualityCommand(t.Context(), root, "git", "commit", "-q", "-m", "test: capture selected module file")
	require.NoError(t, err)

	inputs, err := qualityCollectSourceInputs(t.Context(), root, "", modfile, root)
	require.NoError(t, err)
	assert.Equal(t, modData, inputs.SnapshotData["selected-go.mod"])
	assert.Equal(t, qualityHash(modData), inputs.Hashes["selected-go.mod"])
	assert.Equal(t, sumData, inputs.SnapshotData["selected-go.sum"])
	assert.Equal(t, qualityHash(sumData), inputs.Hashes["selected-go.sum"])
}

func TestQualitySourceInventoryRejectsLocalReplacementOutsideGit(t *testing.T) {
	root := qualityLinkedFixture(t)
	replacement := filepath.Join(t.TempDir(), "replacement")
	require.NoError(t, os.MkdirAll(replacement, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(replacement, "go.mod"), []byte("module example.test/dependency\n\ngo 1.27.3\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(replacement, "dep.go"), []byte("package dependency\n"), 0o600))

	modFile := filepath.Join(root, "go.mod")
	mod, err := os.ReadFile(modFile)
	require.NoError(t, err)
	mod = []byte(strings.Replace(string(mod), "replace example.test/dependency => ./dependency", "replace example.test/dependency => "+replacement, 1))
	require.NoError(t, os.WriteFile(modFile, mod, 0o600))

	_, err = qualityCollectSourceInputs(t.Context(), root, "", "", root)
	require.ErrorContains(t, err, "local replacement")
}

func TestQualitySourceInventoryRejectsSymlinkOutsideModule(t *testing.T) {
	root := qualityLinkedFixture(t)
	target := filepath.Join(t.TempDir(), "dep.go")
	require.NoError(t, os.WriteFile(target, []byte("package git\nvar external = true\n"), 0o600))
	link := filepath.Join(root, "internal", "git", "dep.go")
	require.NoError(t, os.Remove(link))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}
	_, err := qualityCommand(t.Context(), root, "git", "add", "internal/git/dep.go")
	require.NoError(t, err)
	_, err = qualityCommand(t.Context(), root, "git", "commit", "-q", "-m", "test: track linked source")
	require.NoError(t, err)

	_, err = qualityCollectSourceInputs(t.Context(), root, "", "", root)
	require.ErrorContains(t, err, "selected source outside module")
}

func TestQualityPathContainmentResolvesSymlinkedRoot(t *testing.T) {
	root := qualityLinkedFixture(t)
	alias := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}

	assert.True(t, qualityIsWithin(alias, filepath.Join(root, "internal", "git", "dep.go")))
	inputs, err := qualityCollectSourceInputs(t.Context(), alias, "", "", alias)
	require.NoError(t, err)
	require.NoError(t, qualityValidateSourceInputs(t.Context(), inputs))
}

func TestQualitySourceGuardAllowsUnlinkedCachesAndPackages(t *testing.T) {
	root := qualityLinkedFixture(t)
	for name, data := range map[string]string{
		"internal/git/.hegel/cached.go": "package invalid cache syntax",
		"internal/notlinked/extra.go":   "package notlinked",
	} {
		p := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	}
	require.NoError(t, qualityCheckSource(t.Context(), root))
}

func TestQualityDeclaredHarnessMatchesCompiledHelpers(t *testing.T) {
	root := t.TempDir()
	names, err := fs.Glob(qualityCompiledSources, "quality_eval*_test.go")
	require.NoError(t, err)
	dir := filepath.Join(root, "internal", "review")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	for _, name := range names {
		data, err := qualityCompiledSources.ReadFile(name)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	require.NoError(t, qualityCheckHarness(root))
	for _, name := range names {
		data, err := qualityCompiledSources.ReadFile(name)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), append(data, []byte("\n// different helper bytes\n")...), 0o600))
		require.Error(t, qualityCheckHarness(root), "every compiled helper must bind its declared source")
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
}

func TestQualitySourceOverlayRejectsProductionReplacement(t *testing.T) {
	root, harness := t.TempDir(), t.TempDir()
	overlay := filepath.Join(t.TempDir(), "overlay.json")
	data, err := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(root, "internal", "git", "git.go"): filepath.Join(harness, "changed.go")}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(overlay, data, 0o600))
	_, err = qualityReadOverlay(root, harness, overlay)
	require.Error(t, err)
}

func TestQualitySourceSnapshotOverlayPinsCapturedBytes(t *testing.T) {
	root, harness := t.TempDir(), t.TempDir()
	target := filepath.Join(root, "internal", "review", "quality_eval_helper_test.go")
	source := filepath.Join(harness, "quality_eval_helper_test.go")
	original := []byte("package review\nconst helper = 1\n")
	require.NoError(t, os.WriteFile(source, original, 0o600))
	overlayFile := filepath.Join(t.TempDir(), "overlay.json")
	data, err := json.Marshal(map[string]any{"Replace": map[string]string{target: source}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(overlayFile, data, 0o600))
	inputs := qualitySourceInputs{
		Files:         map[string]string{"helper": source},
		Hashes:        map[string]string{"helper": qualityHash(original)},
		SnapshotData:  map[string][]byte{"helper": original},
		CompilerPaths: map[string]string{"helper": target},
	}
	snapshot, err := qualityWriteSourceSnapshotOverlay(root, harness, overlayFile, t.TempDir(), inputs)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(source, []byte("package review\nconst helper = 2\n"), 0o600))
	data, err = os.ReadFile(snapshot)
	require.NoError(t, err)
	var compiled struct{ Replace map[string]string }
	require.NoError(t, json.Unmarshal(data, &compiled))
	copyPath, ok := compiled.Replace[filepath.Clean(target)]
	require.True(t, ok)
	got, err := os.ReadFile(copyPath)
	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestQualityModuleCacheSnapshotPinsCapturedBytes(t *testing.T) {
	cache := t.TempDir()
	name := filepath.Join("example.test", "dependency@v1.2.3", "dep.go")
	moduleDir := filepath.Join(cache, "example.test", "dependency@v1.2.3")
	metadataOnlyModule := filepath.Join("example.test", "unreferenced@v0.0.0")
	metadataOnlyDir := filepath.Join(cache, metadataOnlyModule)
	source := filepath.Join(cache, name)
	original := []byte("package dependency\nconst captured = 1\n")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o700))
	require.NoError(t, os.WriteFile(source, original, 0o600))
	metadataRel := filepath.Join("cache", "download", "example.test", "dependency", "@v")
	metadataDir := filepath.Join(cache, metadataRel)
	require.NoError(t, os.MkdirAll(metadataDir, 0o700))
	modData := []byte("module example.test/dependency\ngo 1.27.3\n")
	modPath := filepath.Join(metadataDir, "v1.2.3.mod")
	require.NoError(t, os.WriteFile(modPath, modData, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDir, "v1.2.3.info"), []byte("{\"Version\":\"v1.2.3\"}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDir, "v1.2.3.ziphash"), []byte("h1:captured"), 0o600))
	unreferencedMetadata := filepath.Join(cache, "cache", "download", "example.test", "unreferenced", "@v")
	require.NoError(t, os.MkdirAll(unreferencedMetadata, 0o700))
	require.NoError(t, os.MkdirAll(metadataOnlyDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(unreferencedMetadata, "v0.0.0.info"), []byte("{\"Version\":\"v0.0.0\"}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(unreferencedMetadata, "v0.0.0.ziphash"), []byte("h1:captured"), 0o600))
	modKey := "module:example.test/dependency@v1.2.3/go.mod"
	inputs := qualitySourceInputs{
		ModuleCacheRoot: cache,
		ModuleCacheDirs: map[string]string{
			"example.test/dependency@v1.2.3":   moduleDir,
			"example.test/unreferenced@v0.0.0": metadataOnlyDir,
		},
		Files: map[string]string{
			"module:example.test/dependency@v1.2.3/dep.go": source,
			modKey: modPath,
		},
		Hashes: map[string]string{
			"module:example.test/dependency@v1.2.3/dep.go": qualityHash(original),
			modKey: qualityHash(modData),
		},
		SnapshotData: map[string][]byte{
			"module:example.test/dependency@v1.2.3/dep.go": original,
			modKey: modData,
		},
	}
	snapshot, err := qualityWriteModuleCacheSnapshot(t.TempDir(), inputs)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(source, []byte("package dependency\nconst captured = 2\n"), 0o600))
	got, err := os.ReadFile(filepath.Join(snapshot, name))
	require.NoError(t, err)
	assert.Equal(t, original, got)
	gotMod, err := os.ReadFile(filepath.Join(snapshot, metadataRel, "v1.2.3.mod"))
	require.NoError(t, err)
	assert.Equal(t, modData, gotMod)
	assert.DirExists(t, filepath.Join(snapshot, metadataOnlyModule))
	for _, suffix := range []string{"info", "ziphash"} {
		assert.FileExists(t, filepath.Join(snapshot, metadataRel, "v1.2.3."+suffix))
	}
}

func TestQualityModuleCacheSnapshotSkipsInputsOnAnotherDrive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows paths are required to exercise filepath.Rel across volumes")
	}
	cache := t.TempDir()
	otherVolume := "Z:"
	if strings.EqualFold(filepath.VolumeName(cache), otherVolume) {
		otherVolume = "Y:"
	}
	outside := filepath.Join(otherVolume+string(filepath.Separator), "external", "selected.go")
	_, err := qualityWriteModuleCacheSnapshot(t.TempDir(), qualitySourceInputs{
		ModuleCacheRoot: cache,
		Files:           map[string]string{"local-source": outside},
	})
	require.NoError(t, err)
}

func TestQualitySourceSnapshotModfileUsesCapturedInputs(t *testing.T) {
	directory := t.TempDir()
	mod, sum := []byte("module example.test/selected\ngo 1.27.3\n"), []byte("example.test/dep v1.2.3 h1:captured\n")
	inputs := qualitySourceInputs{
		Hashes:       map[string]string{"selected-go.mod": qualityHash(mod), "selected-go.sum": qualityHash(sum)},
		SnapshotData: map[string][]byte{"selected-go.mod": mod, "selected-go.sum": sum},
	}
	filename, err := qualityWriteSourceSnapshotModfile(directory, filepath.Join(directory, "original.mod"), inputs)
	require.NoError(t, err)
	gotMod, err := os.ReadFile(filename)
	require.NoError(t, err)
	gotSum, err := os.ReadFile(strings.TrimSuffix(filename, ".mod") + ".sum")
	require.NoError(t, err)
	assert.Equal(t, mod, gotMod)
	assert.Equal(t, sum, gotSum)
}

func TestQualityCompiledSourceInventoryRejectsMismatch(t *testing.T) {
	inputs := qualitySourceInputs{GoVersion: "go1.27.3", Modules: map[string]string{"example.test/dep": "v1.2.3"}, Hashes: map[string]string{"dep.go": "committed-hash"}}
	compiled, err := qualitySourceInventoryHash(inputs)
	require.NoError(t, err)
	require.NoError(t, qualityCheckCompiledSourceInventory(compiled, inputs))
	require.Error(t, qualityCheckCompiledSourceInventory("", inputs))
	inputs.Hashes["dep.go"] = "different-hash"
	require.Error(t, qualityCheckCompiledSourceInventory(compiled, inputs))
}

func TestQualityAuthRejectsMissingSelectedProvider(t *testing.T) {
	source := filepath.Join(t.TempDir(), "auth.json")
	require.NoError(t, os.WriteFile(source, []byte(`{"other":{"type":"api_key","key":"synthetic"}}`), 0o600))
	require.Error(t, qualityCopyAuth(source, t.TempDir()))
	require.Error(t, qualityCopyAuth("", t.TempDir()))
}

func TestQualityChildEnvironmentPreservesParentAndDropsDuplicateOverrides(t *testing.T) {
	inherited := []string{"CODEX_HOME=one", "CODEX_HOME=two", "CLAUDE_CONFIG_DIR=private", "PI_CODING_AGENT_DIR=private", "HOME=private", "XDG_CONFIG_HOME=private", "UNRELATED_PROVIDER_KEY=private", "ROBOREV_QUALITY_PI_AUTH=private"}
	before := slices.Clone(inherited)
	child, err := qualityLiveEnv(t.TempDir(), inherited)
	require.NoError(t, err)
	assert.Equal(t, before, inherited)
	for _, entry := range inherited {
		assert.NotContains(t, child, entry)
	}
}

func TestQualityBuildSettingsRejectsUnsupportedSelections(t *testing.T) {
	base := []debug.BuildSetting{{Key: "-tags", Value: "qualityeval"}, {Key: "-compiler", Value: "gc"}}
	require.NoError(t, qualitySupportedBuildSettings(base))
	for _, extra := range []debug.BuildSetting{{Key: "-tags", Value: "qualityeval,extra"}, {Key: "-race", Value: "true"}, {Key: "-gcflags", Value: "all=-lang=go1.21"}, {Key: "CGO_CFLAGS", Value: "-DOTHER"}, {Key: "GOEXPERIMENT", Value: "loopvar"}} {
		settings := append(slices.Clone(base), extra)
		require.Error(t, qualitySupportedBuildSettings(settings), "unsupported compiler selection must not reach model invocation")
	}
	require.Error(t, qualitySupportedBuildSettings(nil))
}

func TestQualitySelectionFingerprintBindsModuleAndOverlayBytes(t *testing.T) {
	root, harness := t.TempDir(), t.TempDir()
	modfile := filepath.Join(root, "selected.mod")
	require.NoError(t, os.WriteFile(modfile, []byte("module example.test/selected\ngo 1.27.3\n"), 0o600))
	overlay := filepath.Join(root, "overlay.json")
	require.NoError(t, os.WriteFile(overlay, []byte(`{"Replace":{}}`), 0o600))
	before, err := qualitySelectionFingerprint(root, harness, overlay, modfile)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(modfile, []byte("module example.test/selected\ngo 1.27.2\n"), 0o600))
	changedModule, err := qualitySelectionFingerprint(root, harness, overlay, modfile)
	require.NoError(t, err)
	assert.NotEqual(t, before, changedModule)
	require.NoError(t, os.WriteFile(overlay, []byte("{\"Replace\":{}}\n"), 0o600))
	changedOverlay, err := qualitySelectionFingerprint(root, harness, overlay, modfile)
	require.NoError(t, err)
	assert.NotEqual(t, changedModule, changedOverlay)
}

func TestQualitySourceGuardRejectsNativeRemoval(t *testing.T) {
	for _, ext := range []string{".s", ".c", ".swig", ".swigcxx"} {
		for _, operation := range []string{"delete", "move"} {
			t.Run(ext+"/"+operation, func(t *testing.T) {
				if ext == ".c" {
					enabled, err := qualityCommand(t.Context(), ".", "go", "env", "CGO_ENABLED")
					require.NoError(t, err)
					if enabled != "1" {
						t.Skip("native C fixture requires cgo")
					}
				}
				root := qualityLinkedFixture(t)
				dir := filepath.Join(root, "internal", "native")
				require.NoError(t, os.MkdirAll(dir, 0o700))
				source := "package native\nfunc Marker() int {return 1}\n"
				if ext == ".c" {
					source = "package native\nimport \"C\"\nfunc Marker() int{return 1}\n"
				}
				require.NoError(t, os.WriteFile(filepath.Join(dir, "native.go"), []byte(source), 0o600))
				name := filepath.Join(dir, "selected"+ext)
				content := "// Selected assembly source.\n"
				if ext == ".c" {
					content = "static void __attribute__((constructor)) initialize(void) {}\n"
				}
				if ext == ".swig" || ext == ".swigcxx" {
					content = "%module selected\n%inline %{ int marker(void) { return 1; } %}\n"
				}
				require.NoError(t, os.WriteFile(name, []byte(content), 0o600))
				owner := filepath.Join(root, "internal", "review", "native.go")
				require.NoError(t, os.WriteFile(owner, []byte("package review\nimport _ \"example.test/guard/internal/native\"\n"), 0o600))
				_, err := qualityCommand(t.Context(), root, "git", "add", "internal/native", "internal/review/native.go")
				require.NoError(t, err)
				_, err = qualityCommand(t.Context(), root, "git", "commit", "-q", "-m", "test: record native input")
				require.NoError(t, err)
				require.NoError(t, qualityCheckSource(t.Context(), root))
				if operation == "delete" {
					require.NoError(t, os.Remove(name))
				} else {
					require.NoError(t, os.Rename(name, filepath.Join(root, "moved"+ext)))
				}
				require.Error(t, qualityCheckSource(t.Context(), root), "removed native input must not disappear from source identity")
			})
		}
	}
}

func TestQualityTrackedNativeMatcherRespectsGoConstraints(t *testing.T) {
	root := qualityLinkedFixture(t)
	dir := filepath.Join(root, "internal", "git")
	wrongOS := "windows"
	if runtime.GOOS == "windows" {
		wrongOS = "linux"
	}
	files := map[string]string{"inactive.s": "//go:build extra\n\n// excluded assembly\n", "other_" + wrongOS + ".s": "// other platform\n", "unrelated.txt": "not a native input\n", "_hidden.s": "// excluded filename\n"}
	for name, data := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600))
	}
	_, err := qualityCommand(t.Context(), root, "git", "add", "internal/git")
	require.NoError(t, err)
	_, err = qualityCommand(t.Context(), root, "git", "commit", "-q", "-m", "test: record excluded native controls")
	require.NoError(t, err)
	for name := range files {
		require.NoError(t, os.Remove(filepath.Join(dir, name)))
		selected, err := qualityTrackedNativeInput(t.Context(), root, "internal/git/"+name, false)
		require.NoError(t, err)
		assert.False(t, selected, "Go must exclude this input from the current selection")
	}
}

type qualityBoundaryAgent struct {
	*agent.TestAgent
	CWD, Prompt string
}

func (a *qualityBoundaryAgent) Review(_ context.Context, cwd, _ string, prompt string, _ io.Writer) (string, error) {
	a.CWD = cwd
	a.Prompt = prompt
	return `{"schema_version":3,"summary":"No findings.","verdict":"pass","findings":[]}`, nil
}

func TestQualityModelBoundaryDoesNotExposeConditionLabels(t *testing.T) {
	rows, err := qualityPlan(1)
	require.NoError(t, err)
	prepared, err := qualityPrepareCases(t.Context(), t.TempDir(), rows, qualityFixture)
	require.NoError(t, err)
	for _, row := range rows {
		a := &qualityBoundaryAgent{TestAgent: agent.NewTestAgent()}
		_, err := qualityInvoke(t.Context(), a, prepared[row.Case], io.Discard)
		require.NoError(t, err)
		for _, label := range []string{"vulnerable", "fixed", "safe", "true_duplicate", "two_distinct"} {
			assert.NotContains(t, a.CWD, label, "agent system prompts may expose the entire working-directory path")
		}
		for _, label := range []string{row.Case, strings.ReplaceAll(row.Case, "/", "-")} {
			assert.NotContains(t, a.Prompt, label, "condition IDs belong only in external artifacts")
		}
	}
}

func TestQualityCasePreparationUsesFrozenCorpusBytes(t *testing.T) {
	root, head := qualityFrozenCorpusFixture(t)
	snapshot, err := qualityLoadFrozenCorpus(t.Context(), root, head)
	require.NoError(t, err)
	expected, err := snapshot.Fixture("cursor", "fixed")
	require.NoError(t, err)
	target := filepath.Join(root, "internal", "review", "testdata", "quality", "cursor", "fixed", "fixture.go")
	require.NoError(t, os.WriteFile(target, []byte(strings.ReplaceAll(string(expected), "> cursor", ">= cursor")), 0o600))
	t.Setenv("ROBOREV_QUALITY_HARNESS_DIR", root)
	row := qualityObservation{ID: "cursor-fixed-1", Case: "cursor/fixed", Variant: "fixed", Kind: "review", Targets: []string{"cursor"}}
	cases, err := qualityPrepareCases(t.Context(), t.TempDir(), []qualityObservation{row}, snapshot.Fixture)
	require.NoError(t, err)
	prepared := cases[row.Case]
	observed, err := os.ReadFile(filepath.Join(prepared.RepoPath, "cursor.go"))
	require.NoError(t, err)
	assert.Equal(t, expected, observed, "the model checkout must use the committed snapshot, not a later mutable read")
	a := &qualityBoundaryAgent{TestAgent: agent.NewTestAgent()}
	_, err = qualityInvoke(t.Context(), a, prepared, io.Discard)
	require.NoError(t, err)
	assert.Contains(t, a.Prompt, "+"+strings.ReplaceAll(strings.TrimSuffix(string(expected), "\n"), "\n", "\n+"), "the real review diff carries the same committed source")
}

func TestQualitySourceGuardAllowsOnlyUnselectedStagedNative(t *testing.T) {
	excludedOS := "windows"
	if runtime.GOOS == excludedOS {
		excludedOS = "linux"
	}
	for _, tc := range []struct {
		Name, Source string
		Selected     bool
	}{
		{"_excluded.s", "// Excluded by Go's filename rules.\n", false},
		{"excluded_" + excludedOS + ".s", "// Excluded on the active platform.\n", false},
		{"excluded.s", "//go:build qualityeval_excluded\n\n// Inactive build constraint.\n", false},
		{"selected.s", "// Selected native input.\n", true},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			root := qualityLinkedFixture(t)
			require.NoError(t, qualityCheckSource(t.Context(), root))
			relative := "internal/storage/" + tc.Name
			require.NoError(t, os.WriteFile(filepath.Join(root, filepath.FromSlash(relative)), []byte(tc.Source), 0o600))
			_, err := qualityCommand(t.Context(), root, "git", "add", "--", relative)
			require.NoError(t, err)
			err = qualityCheckSource(t.Context(), root)
			if tc.Selected {
				require.Error(t, err, "a staged selected input must not inherit the committed source identity")
			} else {
				require.NoError(t, err, "an excluded staged file has no historical input to validate")
			}
		})
	}
}

func TestQualityHistoricalNativeInputPropagatesGitFailure(t *testing.T) {
	_, err := qualityTrackedNativeInput(t.Context(), t.TempDir(), "missing.s", false)
	require.Error(t, err, "repository errors must not be treated as an absent historical path")
}
