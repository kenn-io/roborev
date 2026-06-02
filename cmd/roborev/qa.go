package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/lint"
	"go.kenn.io/roborev/internal/mutate"
	"go.kenn.io/roborev/internal/qa"
)

func qaCmd() *cobra.Command {
	var (
		skipLint        bool
		skipMutate      bool
		jsonOut         bool
		maxLintFindings int
		minMutateScore  float64
	)

	cmd := &cobra.Command{
		Use:   "qa [paths...]",
		Short: "Run unified quality assurance: lint + mutation testing + review",
		Long: `Run all quality gates in sequence and produce a unified report.

Pipeline order:
  1. Lint (Semgrep)       — deterministic, fast (~seconds)
  2. Mutation testing     — measures test quality (~minutes)
  3. AI review            — triggered separately via roborev review

Each phase contributes to a composite score. Gates can enforce minimum
thresholds — the command exits non-zero if any gate fails.

Examples:
  roborev qa                              # run all phases
  roborev qa --skip-mutate                # lint only
  roborev qa --fail-lint 5                # max 5 lint findings
  roborev qa --fail-mutate 0.70           # require 70% mutation score
  roborev qa --json                       # machine-readable output
  roborev qa --skip-mutate --fail-lint 0  # zero-tolerance lint
`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			workDir, _ := os.Getwd()
			opts := qa.Options{
				SkipLint:        skipLint,
				SkipMutate:      skipMutate,
				MaxLintFindings: maxLintFindings,
				MinMutateScore:  minMutateScore,
				Paths:           args,
			}
			if len(opts.Paths) == 0 {
				opts.Paths = []string{"."}
			}

			// Run phases sequentially
			if !jsonOut {
				cmd.Println()
				cmd.Println("╔══════════════════════════════════════════╗")
				cmd.Println("║         roborev QA — Quality Gate        ║")
				cmd.Println("╚══════════════════════════════════════════╝")
				cmd.Println()
			}

			report := &qa.Report{}
			anyGateFailed := false

			// Phase 1: Lint
			if !skipLint {
				if !jsonOut {
					cmd.Println("── Phase 1: Lint (Semgrep) ────────────────")
				}
				lintReport, err := lint.Run(lint.Options{
					Paths:    opts.Paths,
					Config:   "auto",
					Severity: []string{"ERROR", "WARNING"},
					Exclude:  []string{"testdata", "vendor", "node_modules", "*.test.go"},
				})
				if err != nil {
					if !jsonOut {
						cmd.Printf("  Lint error: %v\n", err)
					}
				} else {
					report.Lint = lintReport
					if !jsonOut {
						printQALintPhase(cmd, lintReport)
					}
				}
			} else {
				report.Skipped = append(report.Skipped, "lint")
			}

			// Phase 2: Mutation testing
			if !skipMutate {
				if !jsonOut {
					cmd.Println()
					cmd.Println("── Phase 2: Mutation Testing ──────────────")
				}
				backend := mutate.Detect(workDir)
				if backend != nil && backend.Available() {
					if !jsonOut {
						cmd.Printf("  Using: %s\n", backend.Name())
						cmd.Println("  Running mutation tests (this may take minutes)...")
					}
					mutateResult, err := backend.Run(nil)
					if err != nil {
						if !jsonOut {
							cmd.Printf("  Mutation testing error: %v\n", err)
						}
					} else {
						report.Mutate = mutateResult
						if !jsonOut {
							printQAMutatePhase(cmd, mutateResult)
						}
					}
				} else {
					report.Skipped = append(report.Skipped, "mutate (no backend available)")
					if !jsonOut {
						lang := mutate.DetectLanguage(workDir)
						cmd.Printf("  Skipped: no mutation testing backend for %s\n", lang)
					}
				}
			} else {
				report.Skipped = append(report.Skipped, "mutate")
			}

			// Compute composite score
			report.ComputeScore()

			// Run gates
			report.RunGates(opts)

			// Print final verdict
			if !jsonOut {
				printQAVerdict(cmd, report)
			} else {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				enc.Encode(report)
			}

			// Check gates
			for _, g := range report.Gates {
				if !g.Passed {
					anyGateFailed = true
				}
			}

			if anyGateFailed {
				return fmt.Errorf("quality gates failed")
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&skipLint, "skip-lint", false, "Skip lint phase")
	cmd.Flags().BoolVar(&skipMutate, "skip-mutate", false, "Skip mutation testing phase")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().IntVar(&maxLintFindings, "fail-lint", 0, "Fail if lint findings exceed N (0 = no gate)")
	cmd.Flags().Float64Var(&minMutateScore, "fail-mutate", 0, "Fail if mutation score below this (0.0-1.0, 0 = no gate)")

	return cmd
}

func printQALintPhase(cmd *cobra.Command, r *lint.Report) {
	counts := r.CountBySeverity()
	total := len(r.Results)
	if total == 0 {
		cmd.Println("  ✓ No findings — clean lint")
		return
	}
	cmd.Printf("  Findings: %d", total)
	parts := make([]string, 0, len(counts))
	for sev, count := range counts {
		parts = append(parts, fmt.Sprintf("%s=%d", sev, count))
	}
	cmd.Printf(" (%s)\n", strings.Join(parts, ", "))

	// Show worst findings first (up to 5)
	shown := 0
	severityOrder := []string{"ERROR", "WARNING", "INFO"}
	for _, want := range severityOrder {
		for _, f := range r.Results {
			if shown >= 5 {
				break
			}
			sev := f.Severity
			if sev == "" {
				sev = f.Extra.Severity
			}
			if strings.EqualFold(sev, want) {
				msg := f.Extra.Message
				if len(msg) > 100 {
					msg = msg[:97] + "..."
				}
				cmd.Printf("    %-7s %s:%d  %s\n", "["+strings.ToUpper(sev)+"]", f.Path, f.Start.Line, msg)
				shown++
			}
		}
		if shown >= 5 {
			break
		}
	}
	if total > shown {
		cmd.Printf("    ... and %d more\n", total-shown)
	}
}

func printQAMutatePhase(cmd *cobra.Command, r *mutate.Result) {
	if r.Total == 0 {
		cmd.Println("  No mutants generated (check test config)")
		return
	}
	barLen := 30
	filled := int(r.Score * float64(barLen))
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barLen-filled)
	cmd.Printf("  Score: %5.1f%%  [%s]\n", r.Score*100, bar)
	cmd.Printf("  Killed=%d Survived=%d Total=%d\n", r.Killed, r.Survived, r.Total)
}

func printQAVerdict(cmd *cobra.Command, r *qa.Report) {
	cmd.Println()
	cmd.Println("╔══════════════════════════════════════════╗")
	cmd.Println("║              QA Verdict                  ║")
	cmd.Println("╚══════════════════════════════════════════╝")
	cmd.Println()

	// Composite score
	scoreBar := ""
	if r.Score > 0 {
		filled := int(r.Score * 30)
		scoreBar = strings.Repeat("█", filled) + strings.Repeat("░", 30-filled)
	}
	cmd.Printf("  Composite: %.1f%%  %s\n", r.Score*100, scoreBar)
	cmd.Println()

	// Individual scores
	if r.Lint != nil {
		count := len(r.Lint.Results)
		if count == 0 {
			cmd.Println("  Lint:     ✓ PASS  (clean)")
		} else {
			cmd.Printf("  Lint:     ✗ %d finding(s)\n", count)
		}
	}
	if r.Mutate != nil {
		if r.Mutate.Total > 0 {
			status := "PASS"
			if r.Mutate.Score < 0.5 {
				status = "FAIL"
			}
			cmd.Printf("  Mutate:   %s  (%.1f%% score, %d/%d killed)\n",
				status, r.Mutate.Score*100, r.Mutate.Killed, r.Mutate.Total)
		} else {
			cmd.Println("  Mutate:   SKIP  (no mutants)")
		}
	}
	for _, s := range r.Skipped {
		cmd.Printf("  %s:  SKIP\n", s)
	}

	// Gates
	if len(r.Gates) > 0 {
		cmd.Println()
		cmd.Println("── Quality Gates ──────────────────────────")
		for _, g := range r.Gates {
			mark := "✓"
			if !g.Passed {
				mark = "✗"
			}
			switch {
			case g.Count > 0 || strings.HasPrefix(g.Name, "lint"):
				cmd.Printf("  %s %-10s %d / %d findings  %s\n",
					mark, g.Name, g.Count, int(g.Limit), g.Details)
			default:
				cmd.Printf("  %s %-10s %5.1f%% / %5.1f%%  %s\n",
					mark, g.Name, g.Score*100, g.Limit*100, g.Details)
			}
		}
	}

	// Skipped
	if len(r.Skipped) > 0 {
		cmd.Println()
		cmd.Println("── Skipped ────────────────────────────────")
		for _, s := range r.Skipped {
			cmd.Printf("  • %s\n", s)
		}
	}

	// Overall verdict
	cmd.Println()
	if r.Score >= 0.8 {
		cmd.Println("  🏆 Ship it — all quality gates green.")
	} else if r.Score >= 0.5 {
		cmd.Println("  ⚠️  Review needed — some gates show warnings.")
	} else {
		cmd.Println("  ❌ Blocked — quality gates not met. Fix issues and re-run.")
	}
	cmd.Println()
}
