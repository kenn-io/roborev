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
	fields := strings.Fields(strings.ToLower(line))
	for i, f := range fields {
		switch f {
		case "killed":
			if i+1 < len(fields) {
				if n, err := strconv.Atoi(fields[i+1]); err == nil {
					r.Killed = n
				}
			}
		case "survived":
			if i+1 < len(fields) {
				if n, err := strconv.Atoi(fields[i+1]); err == nil {
					r.Survived = n
				}
			}
		case "timeout":
			if i+1 < len(fields) {
				if n, err := strconv.Atoi(fields[i+1]); err == nil {
					r.Timeouted = n
				}
			}
		case "suspicious":
			if i+1 < len(fields) {
				if n, err := strconv.Atoi(fields[i+1]); err == nil {
					r.Suspicious = n
				}
			}
		}
	}
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

// goBackend wraps ooze (Go mutation testing).
type goBackend struct {
	path string
}

func (b *goBackend) Name() string { return "ooze" }

func (b *goBackend) Available() bool {
	b.path = findExecutable("ooze")
	return b.path != ""
}

func (b *goBackend) Run(files []string) (*Result, error) {
	return &Result{
		Backend: "ooze",
		Output:  "Go mutation testing requires ooze. Install: go install github.com/gtramontina/ooze@latest",
	}, nil
}
