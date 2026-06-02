package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/lint"
)

func lintCmd() *cobra.Command {
	var (
		config     string
		severity   string
		autofix    bool
		jsonOut    bool
		diff       bool
		branch     string
		baseBranch string
		quiet      bool
	)

	cmd := &cobra.Command{
		Use:   "lint [paths...]",
		Short: "Run deterministic static analysis with Semgrep",
		Long: `Run Semgrep static analysis on files to catch security vulnerabilities,
code quality issues, and bugs — deterministically, with no AI hallucination risk.

Complements roborev's agent-based review: lint catches the mechanical issues
so the AI reviewer can focus on architecture, logic, and design.

By default, runs Semgrep's "auto" ruleset which selects the right rules for
each language detected in the repository.

Examples:
  roborev lint                          # scan entire repo
  roborev lint ./cmd/...                # scan specific path
  roborev lint --diff                   # scan only changed files (git diff)
  roborev lint --severity ERROR         # only show errors
  roborev lint --severity ERROR,WARNING # errors and warnings
  roborev lint --fix                    # auto-apply safe fixes
  roborev lint --json                   # machine-readable JSON output
  roborev lint --config p/security-audit  # custom ruleset
`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// Determine what to scan
			var paths []string
			if diff {
				workDir, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("get working directory: %w", err)
				}
				repoRoot := workDir
				if root, err := gitrepo.Root(ctx, workDir); err == nil {
					repoRoot = root
				}
				changed, err := getChangedFiles(repoRoot, branch, baseBranch)
				if err != nil {
					return fmt.Errorf("get changed files: %w", err)
				}
				if len(changed) == 0 {
					if !quiet {
						cmd.Println("No changed files to lint.")
					}
					return nil
				}
				paths = changed
			} else if len(args) > 0 {
				paths = args
			} else {
				paths = []string{"."}
			}

			opts := lint.Options{
				Paths:   paths,
				Config:  config,
				Autofix: autofix,
				Exclude: []string{"testdata", "vendor", "node_modules", ".git"},
			}

			if severity != "" {
				for _, s := range strings.Split(severity, ",") {
					opts.Severity = append(opts.Severity, strings.TrimSpace(s))
				}
			}

			report, err := lint.Run(opts)
			if err != nil {
				return fmt.Errorf("lint failed: %w", err)
			}

			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}

			if !report.HasFindings() {
				if !quiet {
					cmd.Println("No issues found.")
				}
				return nil
			}

			printFindings(cmd, report)
			return nil
		},
	}

	cmd.Flags().StringVar(&config, "config", "", "Semgrep ruleset (default: auto)")
	cmd.Flags().StringVarP(&severity, "severity", "s", "", "Filter by severity: ERROR,WARNING,INFO (comma-separated)")
	cmd.Flags().BoolVar(&autofix, "fix", false, "Auto-apply safe fixes")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&diff, "diff", false, "Scan only changed files (git diff)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to diff against (default: current branch)")
	cmd.Flags().StringVar(&baseBranch, "base", "", "Base branch for --diff (default: auto-detect)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress output, exit non-zero if findings")

	return cmd
}

// getChangedFiles returns code files changed on the current branch vs base.
func getChangedFiles(repoRoot, branch, base string) ([]string, error) {
	var target string
	if branch != "" && branch != "HEAD" {
		target = branch
	} else if base != "" {
		target = base
	} else {
		// Auto-detect base
		out, err := runGit(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("get current branch: %w", err)
		}
		currentBranch := strings.TrimSpace(out)
		for _, candidate := range []string{"main", "master", "develop"} {
			if _, err := runGit(repoRoot, "merge-base", candidate, currentBranch); err == nil {
				target = candidate
				break
			}
		}
		if target == "" {
			target = "main"
		}
	}

	out, err := runGit(repoRoot, "diff", "--name-only", target+"...")
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}

	nonCodeExts := map[string]bool{
		".md": true, ".txt": true, ".json": true, ".yaml": true, ".yml": true,
		".toml": true, ".xml": true, ".svg": true, ".csv": true,
	}

	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if nonCodeExts[strings.ToLower(filepath.Ext(line))] {
			continue
		}
		files = append(files, filepath.Join(repoRoot, line))
	}

	return files, nil
}

func runGit(repoRoot string, args ...string) (string, error) {
	gitArgs := append([]string{"-C", repoRoot}, args...)
	cmd := exec.Command("git", gitArgs...)
	out, err := cmd.Output()
	return string(out), err
}

func printFindings(cmd *cobra.Command, report *lint.Report) {
	counts := report.CountBySeverity()
	total := len(report.Results)

	cmd.Printf("\nFound %d finding(s):\n", total)
	for _, sev := range []string{"ERROR", "WARNING", "INFO"} {
		if c := counts[sev]; c > 0 {
			cmd.Printf("  %s: %d\n", sev, c)
		}
	}
	cmd.Println()

	byFile := make(map[string][]lint.Finding)
	for _, f := range report.Results {
		byFile[f.Path] = append(byFile[f.Path], f)
	}

	var fileNames []string
	for name := range byFile {
		fileNames = append(fileNames, name)
	}
	sort.Strings(fileNames)

	for _, fname := range fileNames {
		findings := byFile[fname]
		cmd.Printf("── %s (%d issue(s)) ──\n", fname, len(findings))
		for _, f := range findings {
			sev := f.Severity
			if sev == "" {
				sev = f.Extra.Severity
			}
			if sev == "" {
				sev = "UNKNOWN"
			}
			prefix := sevToIcon(sev)
			cmd.Printf("  %s [%s] L%d:%d — %s\n",
				prefix, f.CheckID, f.Start.Line, f.Start.Col, f.Extra.Message)
		}
		cmd.Println()
	}

	if len(report.Errors) > 0 {
		cmd.Printf("── Semgrep engine messages ──\n")
		for _, e := range report.Errors {
			if e.Level == "warn" {
				cmd.Printf("  ⚠  %s: %s\n", e.Path, e.Message)
			} else {
				cmd.Printf("  ✗  %s: %s\n", e.Path, e.Message)
			}
		}
	}
}

func sevToIcon(sev string) string {
	switch strings.ToUpper(sev) {
	case "ERROR":
		return "✗"
	case "WARNING":
		return "⚠"
	case "INFO":
		return "ℹ"
	default:
		return "•"
	}
}
