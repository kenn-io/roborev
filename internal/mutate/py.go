package mutate

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// mutmutBackend wraps the mutmut Python mutation testing tool.
type mutmutBackend struct {
	path string
}

func (b *mutmutBackend) Name() string { return "mutmut" }

func (b *mutmutBackend) Available() bool {
	b.path = findExecutable("mutmut")
	return b.path != ""
}

func (b *mutmutBackend) Run(files []string) (*Result, error) {
	args := []string{"run"}

	cmd := exec.Command(b.path, args...)
	out, err := cmd.CombinedOutput()
	output := string(out)

	result := &Result{
		Backend: "mutmut",
		Output:  output,
	}

	if err != nil {
		if len(out) == 0 {
			return result, fmt.Errorf("mutmut failed: %w", err)
		}
	}

	b.parseResults(result, output)
	return result, nil
}

func (b *mutmutBackend) parseResults(r *Result, output string) {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "killed") {
			r.Killed++
		} else if strings.Contains(line, "timeout") {
			r.Timeouted++
		} else if strings.Contains(line, "suspicious") {
			r.Suspicious++
		} else if strings.Contains(line, "survived") {
			r.Survived++
		}
	}

	// Try summary line parsing if per-line didn't work
	if r.Killed == 0 && r.Survived == 0 {
		for _, line := range lines {
			if strings.Contains(line, "killed") && strings.Contains(line, "total") {
				b.parseSummaryLine(r, line)
				break
			}
		}
	}

	// Fallback: mutmut results
	if r.Killed == 0 && r.Survived == 0 && r.Timeouted == 0 {
		b.tryResultsCommand(r)
	}

	r.Total = r.Killed + r.Survived + r.Timeouted + r.Suspicious
	if r.Total > 0 {
		r.Score = float64(r.Killed+r.Timeouted) / float64(r.Total)
	}
}

func (b *mutmutBackend) parseSummaryLine(r *Result, line string) {
	// Strip trailing punctuation from each token to handle
	// mutmut output like "8 survived. 0 timeout."
	fields := strings.Fields(strings.ToLower(line))
	clean := make([]string, len(fields))
	for i, f := range fields {
		clean[i] = strings.TrimRight(f, ".,;:!")
	}
	for i, f := range clean {
		switch f {
		case "killed":
			// "Killed 42 out of 50" — count follows
			if n := parseNumAfter(clean, i); n >= 0 {
				r.Killed = n
			}
			// "42 killed" — count precedes
			if n := parseNumBefore(clean, i); n >= 0 {
				r.Killed = n
			}
		case "survived":
			if n := parseNumAfter(clean, i); n >= 0 {
				r.Survived = n
			}
			if n := parseNumBefore(clean, i); n >= 0 {
				r.Survived = n
			}
		case "timeout":
			if n := parseNumAfter(clean, i); n >= 0 {
				r.Timeouted = n
			}
			if n := parseNumBefore(clean, i); n >= 0 {
				r.Timeouted = n
			}
		case "suspicious":
			if n := parseNumAfter(clean, i); n >= 0 {
				r.Suspicious = n
			}
			if n := parseNumBefore(clean, i); n >= 0 {
				r.Suspicious = n
			}
		}
	}
}

// parseNumAfter returns the integer at clean[i+1], or -1 if not found.
func parseNumAfter(clean []string, i int) int {
	if i+1 < len(clean) {
		if n, err := strconv.Atoi(clean[i+1]); err == nil {
			return n
		}
	}
	return -1
}

// parseNumBefore returns the integer at clean[i-1], or -1 if not found.
func parseNumBefore(clean []string, i int) int {
	if i-1 >= 0 {
		if n, err := strconv.Atoi(clean[i-1]); err == nil {
			return n
		}
	}
	return -1
}

func (b *mutmutBackend) tryResultsCommand(r *Result) {
	cmd := exec.Command(b.path, "results")
	out, err := cmd.Output()
	if err != nil {
		return
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.Contains(line, "killed") && strings.Contains(line, "total") {
			b.parseSummaryLine(r, line)
		}
	}
}

// goBackend wraps go-mutesting (Go mutation testing CLI).
type goBackend struct {
	path string
}

func (b *goBackend) Name() string { return "go-mutesting" }

func (b *goBackend) Available() bool {
	b.path = findExecutable("go-mutesting")
	return b.path != ""
}

func (b *goBackend) Run(files []string) (*Result, error) {
	args := []string{"./..."}

	cmd := exec.Command(b.path, args...)
	out, err := cmd.CombinedOutput()
	output := string(out)

	result := &Result{
		Backend: "go-mutesting",
		Output:  output,
	}

	if err != nil {
		if len(out) == 0 {
			return result, fmt.Errorf("go-mutesting failed: %w", err)
		}
	}

	b.parseResults(result, output)
	return result, nil
}

// parseResults extracts mutation counts from go-mutesting output.
// Format: "The mutation score is 0.850 (17 passed, 3 failed, 0 duplicated, 0 skipped, total is 20)"
func (b *goBackend) parseResults(r *Result, output string) {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "mutation score") && strings.Contains(lower, "passed") {
			// Parse: "mutation score is N (X passed, Y failed, Z duplicated, W skipped, total is T)"
			b.parseMutationScore(r, lower)
			break
		}
	}
}

func (b *goBackend) parseMutationScore(r *Result, line string) {
	fields := strings.Fields(line)
	for i, f := range fields {
		switch {
		case f == "passed" && i > 0:
			if n, err := strconv.Atoi(fields[i-1]); err == nil {
				r.Killed = n
			}
		case f == "failed" && i > 0:
			if n, err := strconv.Atoi(fields[i-1]); err == nil {
				r.Survived = n
			}
		case f == "total" && i+2 < len(fields) && fields[i+1] == "is":
			if n, err := strconv.Atoi(fields[i+2]); err == nil {
				r.Total = n
			}
		}
	}

	if r.Total > 0 {
		r.Score = float64(r.Killed) / float64(r.Total)
	}
}
