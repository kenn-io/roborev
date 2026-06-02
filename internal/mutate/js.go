package mutate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// strykerBackend wraps StrykerJS mutation testing via npx.
type strykerBackend struct {
	path string // npx path (but we always use "npx" to launch)
}

func (b *strykerBackend) Name() string { return "stryker" }

func (b *strykerBackend) Available() bool {
	b.path = findExecutable("npx")
	if b.path == "" {
		return false
	}
	// Verify stryker is usable by checking the version
	cmd := exec.Command(b.path, "--yes", "@stryker-mutator/core", "--version")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

func (b *strykerBackend) Run(files []string) (*Result, error) {
	// Stryker always runs on the whole project; individual file targeting
	// is handled via the --mutate flag in stryker.conf.json, not CLI args.
	args := []string{"--yes", "@stryker-mutator/core", "run",
		"--reporter", "clear-text,dashboard",
	}

	cmd := exec.Command(b.path, args...)
	out, err := cmd.CombinedOutput()
	output := string(out)

	result := &Result{
		Backend: "stryker",
		Output:  output,
	}

	if err != nil {
		if len(out) == 0 {
			return result, fmt.Errorf("stryker failed: %w", err)
		}
		// Stryker may exit non-zero even with useful output (e.g., when
		// mutation score is below threshold). Keep parsing.
	}

	b.parseResults(result, output)
	return result, nil
}

func (b *strykerBackend) parseResults(r *Result, output string) {
	lines := strings.Split(output, "\n")

	// Strategy 1: Look for "Mutation score:" line
	// "[Stryker] info    [Report] Mutation score: 85.71%"
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "mutation score") && strings.Contains(lower, "%") {
			b.parseScoreLine(r, line)
			break
		}
	}

	// Strategy 2: Look for tested summary line
	// "[Stryker] info    [Mutant] 42/50 tested (84.00% score)"
	if r.Total == 0 {
		for _, line := range lines {
			lower := strings.ToLower(line)
			if strings.Contains(lower, "[mutant]") && strings.Contains(lower, "tested") {
				b.parseMutantLine(r, line)
				break
			}
		}
	}

	// Strategy 3: Parse per-category counts from clear-text reporter
	// "Killed: 35" / "Survived: 7" / "Timeout: 0" / "No coverage: 2"
	// Always run — category lines give the most granular breakdown.
	for _, line := range lines {
		lower := strings.ToLower(strings.TrimSpace(line))
		switch {
			case strings.HasPrefix(lower, "killed:"):
				if n, err := strconv.Atoi(strings.TrimSpace(line[strings.Index(line, ":")+1:])); err == nil {
					r.Killed = n
				}
			case strings.HasPrefix(lower, "survived:"):
				if n, err := strconv.Atoi(strings.TrimSpace(line[strings.Index(line, ":")+1:])); err == nil {
					r.Survived = n
				}
			case strings.HasPrefix(lower, "timeout:"):
				if n, err := strconv.Atoi(strings.TrimSpace(line[strings.Index(line, ":")+1:])); err == nil {
					r.Timeouted = n
				}
			case strings.HasPrefix(lower, "no coverage:"):
				if n, err := strconv.Atoi(strings.TrimSpace(line[strings.Index(line, ":")+1:])); err == nil {
					r.Suspicious = n // map "no coverage" to suspicious
				}
			}
		}

	r.Total = r.Killed + r.Survived + r.Timeouted + r.Suspicious
	if r.Total > 0 {
		r.Score = float64(r.Killed+r.Timeouted) / float64(r.Total)
	}
}

// parseScoreLine handles: "Mutation score: 85.71%"
func (b *strykerBackend) parseScoreLine(r *Result, line string) {
	fields := strings.Fields(line)
	for _, f := range fields {
		if strings.HasSuffix(f, "%") {
			f = strings.TrimSuffix(f, "%")
			if score, err := strconv.ParseFloat(f, 64); err == nil {
				r.Score = score / 100.0
			}
		}
	}
}

// parseMutantLine handles: "42/50 tested (84.00% score)"
// Only sets Total and Score — individual counts come from category lines.
func (b *strykerBackend) parseMutantLine(r *Result, line string) {
	fields := strings.Fields(line)
	for _, f := range fields {
		// Look for "N/M" pattern
		if strings.Contains(f, "/") {
			parts := strings.Split(f, "/")
			if len(parts) == 2 {
				total, err1 := strconv.Atoi(parts[1])
				if err1 == nil && total > 0 {
					r.Total = total
				}
			}
		}
		// Look for percentage
		if strings.HasSuffix(f, "%") && strings.Contains(f, "(") {
			f = strings.Trim(f, "()")
			f = strings.TrimSuffix(f, "%")
			f = strings.TrimSuffix(f, "score")
			f = strings.TrimSpace(f)
			if score, err := strconv.ParseFloat(f, 64); err == nil {
				r.Score = score / 100.0
			}
		}
	}
}

// hasJSProject checks for JavaScript/TypeScript project indicators.
func hasJSProject(dir string) bool {
	indicators := []string{
		"package.json", "node_modules",
		"tsconfig.json", "jsconfig.json",
	}
	for _, f := range indicators {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	// Check for .js/.ts/.jsx/.tsx files
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		ext := strings.ToLower(e.Name())
		if strings.HasSuffix(ext, ".js") || strings.HasSuffix(ext, ".ts") ||
			strings.HasSuffix(ext, ".jsx") || strings.HasSuffix(ext, ".tsx") ||
			strings.HasSuffix(ext, ".mjs") || strings.HasSuffix(ext, ".cjs") {
			return true
		}
	}
	return false
}
