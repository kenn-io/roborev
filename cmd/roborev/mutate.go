package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/mutate"
)

func mutateCmd() *cobra.Command {
	var (
		lang       string
		jsonOut    bool
		diff       bool
		branch     string
		baseBranch string
		quiet      bool
	)

	cmd := &cobra.Command{
		Use:   "mutate [paths...]",
		Short: "Run mutation testing to measure test quality",
		Long: `Run mutation testing to measure how well your tests catch bugs.

Mutation testing introduces small changes ("mutants") into your code
and checks whether your tests detect them. A high mutation score means
your tests are thorough; a low score reveals blind spots.

Complements roborev's agent-based review and lint: after AI review finds
issues and lint catches mechanical problems, mutation testing verifies
your test suite can actually catch real bugs.

Supported backends:
  Python: mutmut (pip install mutmut)
  Go:     ooze  (go install github.com/gtramontina/ooze@latest)

Examples:
  roborev mutate                         # auto-detect language, run on project
  roborev mutate --lang python           # force Python/mutmut
  roborev mutate --diff                  # only show mutants in changed files
  roborev mutate --json                  # machine-readable JSON output
  roborev mutate ./src/models.py         # specific files
`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			workDir, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}

			repoRoot := workDir
			if root, err := gitrepo.Root(ctx, workDir); err == nil {
				repoRoot = root
			}

			// Auto-detect or use specified language
			detectedLang := lang
			if detectedLang == "" {
				detectedLang = mutate.DetectLanguage(repoRoot)
			}

			if detectedLang == "unknown" {
				return fmt.Errorf(
					"no supported project detected in %s\n\n"+
						"Currently supported:\n"+
						"  Python: projects with pyproject.toml, setup.cfg, or .py files\n"+
						"  Go:     projects with go.mod or .go files\n\n"+
						"Install the mutation testing tool for your language:\n"+
						"  Python: pip install mutmut\n"+
						"  Go:     go install github.com/gtramontina/ooze@latest",
					repoRoot,
				)
			}

			// Pick backend
			backend := mutate.Detect(repoRoot)
			if backend == nil {
				return fmt.Errorf(
					"mutation testing tool for %s not found\n\n"+
						"Install the appropriate tool:\n"+
						"  Python: pip install mutmut\n"+
						"  Go:     go install github.com/gtramontina/ooze@latest",
					detectedLang,
				)
			}

			// Check available
			if !backend.Available() {
				return fmt.Errorf("%s is not installed or not in PATH", backend.Name())
			}

			// Determine which files to mutate
			var files []string
			if diff {
				changed, err := getChangedFiles(repoRoot, branch, baseBranch)
				if err != nil {
					return fmt.Errorf("get changed files: %w", err)
				}
				// Filter to files matching the detected language
				for _, f := range changed {
					if matchesLanguage(f, detectedLang) {
						files = append(files, f)
					}
				}
				if len(files) == 0 {
					if !quiet {
						cmd.Printf("No %s files changed.\n", detectedLang)
					}
					return nil
				}
			} else if len(args) > 0 {
				files = args
			}

			if !quiet && !jsonOut {
				cmd.Printf("Running mutation testing with %s", backend.Name())
				if len(files) > 0 {
					cmd.Printf(" on %d file(s)", len(files))
				}
				cmd.Println("...")
				cmd.Println("(This may take a while — mutation testing runs your test suite repeatedly)")
			}

			result, err := backend.Run(files)
			if err != nil {
				return fmt.Errorf("%s failed: %w", backend.Name(), err)
			}

			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}

			printMutationResults(cmd, result)
			return nil
		},
	}

	cmd.Flags().StringVar(&lang, "lang", "", "Force language backend: python, go")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&diff, "diff", false, "Only show mutants in changed files")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to diff against (default: current)")
	cmd.Flags().StringVar(&baseBranch, "base", "", "Base branch for --diff (default: auto-detect)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress output")

	return cmd
}

func matchesLanguage(path, lang string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch lang {
	case "python":
		return ext == ".py"
	case "go":
		return ext == ".go"
	}
	return false
}

func printMutationResults(cmd *cobra.Command, r *mutate.Result) {
	cmd.Println()
	cmd.Printf("══ Mutation Testing Results (%s) ══\n", r.Backend)
	cmd.Println()

	if r.Total == 0 {
		cmd.Println("No mutants generated. Check your test configuration.")
		if r.Output != "" {
			cmd.Printf("\nTool output:\n%s\n", r.Output)
		}
		return
	}

	// Score bar
	barLen := 40
	filled := int(r.Score * float64(barLen))
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barLen-filled)
	cmd.Printf("Score:  %.1f%%  [%s]\n\n", r.Score*100, bar)

	// Breakdown
	type kv struct {
		K string
		V int
	}
	items := []kv{
		{"Killed", r.Killed},
		{"Survived", r.Survived},
		{"Timeout", r.Timeouted},
		{"Suspicious", r.Suspicious},
	}
	sort.Slice(items, func(i, j int) bool { return items[i].V > items[j].V })

	cmd.Printf("%-14s %s\n", "Category", "Count")
	cmd.Println(strings.Repeat("─", 30))
	for _, item := range items {
		if item.V > 0 {
			icon := "  "
			switch item.K {
			case "Killed":
				icon = "✓ "
			case "Survived":
				icon = "✗ "
			case "Timeout":
				icon = "⏱ "
			case "Suspicious":
				icon = "? "
			}
			cmd.Printf("%-14s %s%d\n", item.K, icon, item.V)
		}
	}
	cmd.Printf("%-14s %d\n", "Total", r.Total)
	cmd.Println()

	// Verdict
	switch {
	case r.Score >= 0.90:
		cmd.Println("Verdict: Excellent — your test suite catches nearly all mutations.")
	case r.Score >= 0.70:
		cmd.Println("Verdict: Good — most mutations are caught. Check survived mutants for blind spots.")
	case r.Score >= 0.50:
		cmd.Println("Verdict: Fair — significant gaps in test coverage. Review survived mutants.")
	case r.Total > 0:
		cmd.Println("Verdict: Weak — many mutations survive. Add tests for the mutated code paths.")
	}
}
