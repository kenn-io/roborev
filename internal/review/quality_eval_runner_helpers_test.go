package review

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	promptpkg "go.kenn.io/roborev/internal/prompt"
)

type qualityPrepared struct {
	Row                      qualityObservation
	Prompt, RepoPath, GitRef string
	Reviews                  []ReviewResult
}
type qualityJudgedRow struct {
	OutputHash string
	Judgments  []qualityJudgment
}
type qualityAdjudication struct {
	Actor string
	// Describe whether adjudication was human or externally source-reviewed by
	// an agent. The harness never calls a semantic judge or claims human review.
	Method  string
	Entries map[string]qualityJudgedRow
}

func qualityPlan(repeats int) ([]qualityObservation, error) {
	if repeats < 1 {
		return nil, fmt.Errorf("repeats must be positive")
	}
	var rows []qualityObservation
	for repeat := 1; repeat <= repeats; repeat++ {
		for _, mechanism := range []string{"invoice", "cursor"} {
			for _, variant := range []string{"vulnerable", "fixed", "safe"} {
				row := qualityObservation{Case: mechanism + "/" + variant, Variant: variant, Repeat: repeat, Kind: "review", Targets: []string{mechanism}}
				row.ID = fmt.Sprintf("%s-%s-%d", mechanism, variant, repeat)
				if variant == "vulnerable" {
					row.Expected = []string{mechanism}
				}
				rows = append(rows, row)
			}
		}
		for _, control := range []string{"true_duplicate", "two_distinct"} {
			expected := []string{"invoice"}
			if control == "two_distinct" {
				expected = append(expected, "cursor")
			}
			rows = append(rows, qualityObservation{ID: fmt.Sprintf("synthesis-%s-%d", control, repeat), Case: "synthesis/" + control, Variant: control, Kind: "synthesis", Repeat: repeat, Expected: expected, Targets: slices.Clone(expected)})
		}
	}
	return rows, nil
}

func qualitySeedFinding(mechanism string) StructuredFinding {
	finding := StructuredFinding{Severity: "high", Kind: "defect", Confidence: 0.9, File: mechanism + ".go", StartLine: 7, EndLine: 7}
	if mechanism == "invoice" {
		finding.Problem = "An existing invoice requested by another tenant returns its amount because lookup checks only ID, not ownership."
		finding.Fix = "Reject cross-tenant reads; preserve own-tenant reads and rejection of missing invoices."
	} else {
		finding.Problem = "When the exclusive cursor equals a value, the result repeats that boundary value instead of starting after it."
		finding.Fix = "Return strictly greater values while preserving all greater records and empty/exhausted results."
	}
	return finding
}

// qualityPrepareCases is shared by the real live path and boundary tests.
func qualityPrepareCases(ctx context.Context, work string, rows []qualityObservation, fixture func(string, string) ([]byte, error)) (map[string]qualityPrepared, error) {
	prepared := map[string]qualityPrepared{}
	for _, row := range rows {
		if _, exists := prepared[row.Case]; exists {
			continue
		}
		checkout, err := os.MkdirTemp(work, "q-")
		if err != nil {
			return nil, err
		}
		p, err := qualityPrepareFrom(ctx, checkout, row, fixture)
		if err != nil {
			return nil, err
		}
		prepared[row.Case] = p
	}
	return prepared, nil
}

func qualityPrepare(ctx context.Context, root string, row qualityObservation) (qualityPrepared, error) {
	return qualityPrepareFrom(ctx, root, row, qualityFixture)
}

func qualityPrepareFrom(ctx context.Context, root string, row qualityObservation, fixture func(string, string) ([]byte, error)) (qualityPrepared, error) {
	p := qualityPrepared{Row: row, RepoPath: root}
	write := func(name string, data []byte) error { return os.WriteFile(filepath.Join(root, name), data, 0o600) }
	if err := os.MkdirAll(root, 0o700); err != nil {
		return p, err
	}
	for _, args := range [][]string{{"git", "init", "-q"}, {"git", "config", "user.name", "Fixture Reviewer"}, {"git", "config", "user.email", "reviewer@example.test"}, {"git", "config", "core.hooksPath", filepath.Join(root, "no-hooks")}} {
		if _, err := qualityCommand(ctx, root, args...); err != nil {
			return p, err
		}
	}
	// A fixed author/date makes the reviewed commits and prompt metadata identical
	// between product source trees. No machine-local path enters the source diff.
	if err := write("go.mod", []byte("module example.test/ledger\n\ngo 1.27.3\n")); err != nil {
		return p, err
	}
	if err := write("README.md", []byte("# Ledger helpers\n\nReadInvoice returns an amount only for the requesting tenant's invoice.\nMissing and other-tenant invoices are rejected; own-tenant reads succeed.\nAfter accepts ascending sorted integers and an exclusive cursor. It returns\nonly values strictly greater than the cursor, including all greater records.\nEmpty input and exhausted cursors return no records.\n")); err != nil {
		return p, err
	}
	if _, err := qualityCommand(ctx, root, "git", "add", "go.mod", "README.md"); err != nil {
		return p, err
	}
	if _, err := qualityCommand(ctx, root, "git", "commit", "-q", "-m", "chore: define helper contracts"); err != nil {
		return p, err
	}
	mechanisms := row.Targets
	if row.Kind == "synthesis" {
		mechanisms = []string{"invoice", "cursor"}
	}
	var fixtureBytes []byte
	for _, mechanism := range mechanisms {
		variant := row.Variant
		if row.Kind == "synthesis" {
			variant = "vulnerable"
		}
		source, err := fixture(mechanism, variant)
		if err != nil {
			return p, err
		}
		fixtureBytes = append(fixtureBytes, []byte(mechanism+"\x00")...)
		fixtureBytes = append(fixtureBytes, source...)
		if err := write(mechanism+".go", source); err != nil {
			return p, err
		}
	}
	if _, err := qualityCommand(ctx, root, "git", "add", "."); err != nil {
		return p, err
	}
	if _, err := qualityCommand(ctx, root, "git", "commit", "-q", "-m", "feat: add ledger helpers"); err != nil {
		return p, err
	}
	sha, err := qualityCommand(ctx, root, "git", "rev-parse", "HEAD")
	if err != nil {
		return p, err
	}
	p.GitRef = sha
	p.Row.FixtureHash = qualityHash(fixtureBytes)
	if row.Kind == "synthesis" {
		inputMechanisms := []string{"invoice", "invoice"}
		if row.Variant == "two_distinct" {
			inputMechanisms[1] = "cursor"
		}
		for _, mechanism := range inputMechanisms {
			doc := StructuredReview{SchemaVersion: 3, Summary: "One actionable issue.", Verdict: "fail", Findings: []StructuredFinding{qualitySeedFinding(mechanism)}}
			p.Reviews = append(p.Reviews, ReviewResult{Agent: "fixture", ReviewType: "default", Structured: &doc, Output: doc.Markdown(""), Status: ResultDone})
		}
		p.Prompt = BuildSynthesisPrompt(p.Reviews, false, "")
	} else {
		builder := promptpkg.NewBuilderWithConfig(nil, &config.Config{}).WithContext(ctx).ForRepo(root, 0).WithRepoConfig(&config.RepoConfig{}, "").WithStructuredOutput(true)
		result, err := builder.BuildWithSnapshot(sha, 0, "pi", "default", "", nil)
		if err != nil {
			return p, err
		}
		if result.Cleanup != nil {
			defer result.Cleanup()
		}
		// Fixtures are small enough for inline transport. Referenced files would be
		// removed by this deferred cleanup, so do not silently run a broken prompt.
		if result.FilePath != "" {
			return p, fmt.Errorf("fixture prompt unexpectedly requires file transport")
		}
		p.Prompt = result.Prompt
	}
	p.Row.PromptHash = qualityHash([]byte(p.Prompt))
	return p, nil
}

func qualityInvoke(ctx context.Context, a agent.Agent, p qualityPrepared, out io.Writer) (StructuredReview, error) {
	readOnly, ok := a.(agent.ReadOnlyReviewAgent)
	if !ok || !readOnly.SupportsReadOnlyReview() {
		return StructuredReview{}, fmt.Errorf("quality evaluation needs a read-only adapter")
	}
	ctx = agent.WithReadOnlyReview(ctx)
	if p.Row.Kind == "synthesis" {
		return RunSynthesisAgent(ctx, a, p.Reviews, p.Prompt, "", out, SynthesisHooks{GlobalConfig: &config.Config{}, Checkout: func() (SynthesisCheckout, error) {
			return SynthesisCheckout{RepoPath: p.RepoPath, GitRef: p.GitRef}, nil
		}})
	}
	result, err := RunAgentReview(ctx, a, p.RepoPath, p.GitRef, p.Prompt, "default", "", nil, out)
	if err != nil {
		return StructuredReview{}, err
	}
	if result.Structured == nil {
		return StructuredReview{}, fmt.Errorf("review returned no structured document")
	}
	return *result.Structured, nil
}

func qualityApplyJudgments(rows []qualityObservation, input qualityAdjudication) (qualitySummary, error) {
	if strings.TrimSpace(input.Actor) == "" {
		return qualitySummary{}, fmt.Errorf("adjudication needs explicit actor attribution")
	}
	if strings.TrimSpace(input.Method) == "" {
		return qualitySummary{}, fmt.Errorf("adjudication needs an explicit method; identify human or external agent source review")
	}
	judgments := map[string][]qualityJudgment{}
	for id, entry := range input.Entries {
		index := slices.IndexFunc(rows, func(row qualityObservation) bool { return row.ID == id })
		if index < 0 {
			return qualitySummary{}, fmt.Errorf("judgment names unknown invocation %q", id)
		}
		if entry.OutputHash == "" || entry.OutputHash != rows[index].OutputHash {
			return qualitySummary{}, fmt.Errorf("judgment for %s does not match the current output hash", id)
		}
		judgments[id] = entry.Judgments
	}
	return qualitySummarize(rows, judgments)
}

// qualityLiveEnv constructs the consuming child's environment from an allowlist.
// Neither inherited agent config overrides nor unrelated provider credentials
// survive. Only an explicit auth path is copied, narrowed to the zai entry.
func qualityLiveEnv(home string, inherited []string) ([]string, error) {
	var env []string
	for _, entry := range inherited {
		key, _, _ := strings.Cut(entry, "=")
		if key == "ROBOREV_QUALITY_PI_AUTH" {
			continue
		}
		if strings.HasPrefix(key, "ROBOREV_QUALITY_") || key == "ROBOREV_RUN_QUALITY_EVAL" {
			env = append(env, entry)
			continue
		}
		switch key {
		case "PATH", "SystemRoot", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "TMPDIR", "TMP", "TEMP", "GOCACHE", "GOMODCACHE", "GOPATH":
			env = append(env, entry)
		}
	}
	for _, dir := range []string{home, filepath.Join(home, ".pi", "agent"), filepath.Join(home, "config"), filepath.Join(home, "cache"), filepath.Join(home, "roborev")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	return append(env, "HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_CACHE_HOME="+filepath.Join(home, "cache"), "ROBOREV_DATA_DIR="+filepath.Join(home, "roborev"), "GOMAXPROCS=2", "GOFLAGS=-p=2", "GOWORK=off", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(home, "gitconfig"), "GIT_TERMINAL_PROMPT=0"), nil
}

func qualityCopyAuth(source, home string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read explicitly selected Pi auth: %w", err)
	}
	var providers map[string]jsontext.Value
	if err := json.Unmarshal(data, &providers); err != nil {
		return fmt.Errorf("decode explicitly selected Pi auth: %w", err)
	}
	selected, ok := providers["zai"]
	if !ok {
		return fmt.Errorf("explicit auth file has no zai entry")
	}
	auth, err := json.Marshal(map[string]jsontext.Value{"zai": selected})
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600)
}

func qualityCheckSource(ctx context.Context, root string) error {
	inputs, err := qualityCollectSourceInputs(ctx, root, "", "", root)
	if err != nil {
		return err
	}
	return qualityValidateSourceInputs(ctx, inputs)
}
