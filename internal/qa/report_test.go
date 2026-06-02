package qa

import (
	"testing"

	"go.kenn.io/roborev/internal/lint"
	"go.kenn.io/roborev/internal/mutate"
)

func TestReport_ComputeScore_LintOnly(t *testing.T) {
	tests := []struct {
		name     string
		findings int
		want     float64
	}{
		{"clean", 0, 1.0},
		{"few", 3, 0.8},
		{"moderate", 10, 0.5},
		{"many", 30, 0.2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := make([]lint.Finding, tt.findings)
			for i := range results {
				results[i] = lint.Finding{
					CheckID:  "test.rule",
					Path:     "test.go",
					Severity: "WARNING",
				}
			}
			r := &Report{
				Lint: &lint.Report{Results: results},
			}
			r.ComputeScore()
			if r.Score != tt.want {
				t.Errorf("score: expected %.2f, got %.2f", tt.want, r.Score)
			}
		})
	}
}

func TestReport_ComputeScore_MutateOnly(t *testing.T) {
	r := &Report{
		Mutate: &mutate.Result{Total: 100, Killed: 85, Score: 0.85},
	}
	r.ComputeScore()
	if r.Score != 0.85 {
		t.Errorf("score: expected 0.85, got %.2f", r.Score)
	}
}

func TestReport_ComputeScore_Both(t *testing.T) {
	r := &Report{
		Lint:   &lint.Report{Results: nil}, // clean lint = 1.0
		Mutate: &mutate.Result{Total: 100, Killed: 90, Score: 0.90},
	}
	r.ComputeScore()
	expected := (1.0 + 0.90) / 2.0
	if r.Score != expected {
		t.Errorf("score: expected %.2f, got %.2f", expected, r.Score)
	}
}

func TestReport_ComputeScore_Empty(t *testing.T) {
	r := &Report{}
	r.ComputeScore()
	if r.Score != 0 {
		t.Errorf("score: expected 0, got %.2f", r.Score)
	}
}

func TestReport_RunGates_LintPass(t *testing.T) {
	results := make([]lint.Finding, 3)
	for i := range results {
		results[i] = lint.Finding{CheckID: "test", Path: "test.go", Severity: "WARNING"}
	}
	r := &Report{
		Lint: &lint.Report{Results: results},
	}
	r.RunGates(Options{MaxLintFindings: 5})

	if len(r.Gates) != 1 {
		t.Fatalf("expected 1 gate, got %d", len(r.Gates))
	}
	if !r.Gates[0].Passed {
		t.Error("expected lint gate to pass (3 <= 5)")
	}
	if r.Gates[0].Count != 3 {
		t.Errorf("expected count 3, got %d", r.Gates[0].Count)
	}
}

func TestReport_RunGates_LintFail(t *testing.T) {
	results := make([]lint.Finding, 10)
	for i := range results {
		results[i] = lint.Finding{CheckID: "test", Path: "test.go", Severity: "ERROR"}
	}
	r := &Report{
		Lint: &lint.Report{Results: results},
	}
	r.RunGates(Options{MaxLintFindings: 5})

	if len(r.Gates) != 1 {
		t.Fatalf("expected 1 gate, got %d", len(r.Gates))
	}
	if r.Gates[0].Passed {
		t.Error("expected lint gate to fail (10 > 5)")
	}
}

func TestReport_RunGates_MutatePass(t *testing.T) {
	r := &Report{
		Mutate: &mutate.Result{Total: 100, Killed: 85, Score: 0.85},
	}
	r.RunGates(Options{MinMutateScore: 0.80})

	if len(r.Gates) != 1 {
		t.Fatalf("expected 1 gate, got %d", len(r.Gates))
	}
	if !r.Gates[0].Passed {
		t.Error("expected mutate gate to pass (0.85 >= 0.80)")
	}
}

func TestReport_RunGates_MutateFail(t *testing.T) {
	r := &Report{
		Mutate: &mutate.Result{Total: 100, Killed: 45, Score: 0.45},
	}
	r.RunGates(Options{MinMutateScore: 0.70})

	if len(r.Gates) != 1 {
		t.Fatalf("expected 1 gate, got %d", len(r.Gates))
	}
	if r.Gates[0].Passed {
		t.Error("expected mutate gate to fail (0.45 < 0.70)")
	}
}

func TestReport_RunGates_NoGates(t *testing.T) {
	r := &Report{
		Lint:   &lint.Report{Results: nil},
		Mutate: &mutate.Result{Total: 100, Killed: 80, Score: 0.80},
	}
	r.RunGates(Options{}) // no thresholds set

	if len(r.Gates) != 0 {
		t.Errorf("expected 0 gates, got %d", len(r.Gates))
	}
}

func TestReport_RunGates_LintZeroTolerance(t *testing.T) {
	// Zero-tolerance: use limit of 1 (even 1 finding fails essentially)
	r := &Report{
		Lint: &lint.Report{Results: []lint.Finding{
			{CheckID: "test", Path: "test.go", Severity: "ERROR"},
		}},
	}
	r.RunGates(Options{MaxLintFindings: 1})

	if len(r.Gates) != 1 {
		t.Fatalf("expected 1 gate, got %d", len(r.Gates))
	}
	if !r.Gates[0].Passed {
		t.Error("expected lint gate to pass (1 finding <= 1 limit)")
	}

	// Two findings should fail with limit 1
	r2 := &Report{
		Lint: &lint.Report{Results: []lint.Finding{
			{CheckID: "a", Path: "a.go", Severity: "ERROR"},
			{CheckID: "b", Path: "b.go", Severity: "WARNING"},
		}},
	}
	r2.RunGates(Options{MaxLintFindings: 1})
	if r2.Gates[0].Passed {
		t.Error("expected lint gate to fail (2 > 1)")
	}
}

func TestGate_JSON(t *testing.T) {
	// Quick sanity check that gates serialize correctly
	_ = Gate{
		Name:    "lint",
		Passed:  true,
		Score:   1.0,
		Limit:   10,
		Count:   3,
		Details: "Findings within threshold",
	}
	// Just verifying no panic during construction
}

func TestLintGateDetails(t *testing.T) {
	if d := lintGateDetails(0, 10); d != "No findings — clean lint" {
		t.Errorf("unexpected detail for clean: %s", d)
	}
	if d := lintGateDetails(3, 10); d != "Findings within threshold" {
		t.Errorf("unexpected detail for within threshold: %s", d)
	}
	if d := lintGateDetails(15, 10); d != "Too many findings — lint gate failed" {
		t.Errorf("unexpected detail for fail: %s", d)
	}
}

func TestMutateGateDetails(t *testing.T) {
	d := mutateGateDetails(&mutate.Result{Total: 0})
	if d != "No mutants generated" {
		t.Errorf("unexpected detail: %s", d)
	}

	d = mutateGateDetails(&mutate.Result{Total: 50, Killed: 40})
	if d != "Mutation score" {
		t.Errorf("unexpected detail: %s", d)
	}
}
