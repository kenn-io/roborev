// Package lint provides deterministic static analysis by wrapping Semgrep.
// This complements roborev's agent-based review with fast, rule-based
// security and code-quality checks that never hallucinate.
package lint

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// StringOrSlice handles Semgrep fields that can be either a single
// string or a JSON array (e.g. `"cwe": "CWE-123"` or `"cwe": ["CWE-123"]`).
type StringOrSlice []string

func (s *StringOrSlice) UnmarshalJSON(data []byte) error {
	// Try array first
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*s = arr
		return nil
	}
	// Fall back to single string
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return fmt.Errorf("string_or_slice: expected string or array, got %s", string(data))
	}
	*s = []string{str}
	return nil
}

// Finding represents a single Semgrep finding.
type Finding struct {
	CheckID  string   `json:"check_id"`
	Path     string   `json:"path"`
	Start    Position `json:"start"`
	End      Position `json:"end"`
	Extra    Extra    `json:"extra"`
	Severity string   `json:"severity"`
}

// Position is a line/column in a source file.
type Position struct {
	Line   int `json:"line"`
	Col    int `json:"col"`
	Offset int `json:"offset"`
}

// Extra holds the message and metadata for a finding.
type Extra struct {
	Message  string   `json:"message"`
	Metadata Metadata `json:"metadata"`
	Severity string   `json:"severity"`
}

// Metadata carries classification data from the Semgrep rule.
// Field types use StringOrSlice to handle Semgrep's inconsistent
// JSON output (sometimes a single string, sometimes an array).
type Metadata struct {
	CWE                StringOrSlice `json:"cwe"`
	OWASP              StringOrSlice `json:"owasp"`
	Category           string        `json:"category"`
	Technology         StringOrSlice `json:"technology"`
	Confidence         string        `json:"confidence"`
	VulnerabilityClass StringOrSlice `json:"vulnerability_class"`
}

// Report holds the full Semgrep scan output.
type Report struct {
	Results []Finding `json:"results"`
	Errors  []Error   `json:"errors"`
}

// Error is a Semgrep parsing/internal error.
type Error struct {
	Code    int    `json:"code"`
	Level   string `json:"level"`
	Message string `json:"message"`
	Path    string `json:"path"`
}

// Options configures a Semgrep scan.
type Options struct {
	// Paths to scan (files or directories).
	Paths []string
	// Config is the Semgrep ruleset (default: "auto").
	Config string
	// Severity filters: only return findings at these levels (e.g. "ERROR", "WARNING").
	Severity []string
	// Autofix applies Semgrep's --autofix to safe findings.
	Autofix bool
	// Exclude patterns (e.g. "testdata", "vendor").
	Exclude []string
}

// DefaultConfig is the Semgrep ruleset used when none is specified.
const DefaultConfig = "auto"

// Run executes Semgrep with the given options and returns parsed results.
func Run(opts Options) (*Report, error) {
	if opts.Config == "" {
		opts.Config = DefaultConfig
	}

	args := []string{"scan", "--config", opts.Config, "--json", "--quiet", "--no-git-ignore"}

	if opts.Autofix {
		args = append(args, "--autofix")
	}

	for _, ex := range opts.Exclude {
		args = append(args, "--exclude", ex)
	}

	if len(opts.Severity) > 0 {
		for _, s := range opts.Severity {
			args = append(args, "--severity", s)
		}
	}

	args = append(args, opts.Paths...)

	cmd := exec.Command("semgrep", args...)
	out, err := cmd.Output()
	if err != nil {
		// Semgrep exits non-zero when findings are present — that's expected.
		// Only treat it as an error if we got no output at all.
		if len(out) == 0 {
			return nil, fmt.Errorf("semgrep failed: %w", err)
		}
	}

	var report Report
	if err := json.Unmarshal(out, &report); err != nil {
		return nil, fmt.Errorf("parse semgrep output: %w", err)
	}

	return &report, nil
}

// HasFindings returns true if the report contains actual findings
// (ignoring parse errors and warnings).
func (r *Report) HasFindings() bool {
	return len(r.Results) > 0
}

// CountBySeverity returns counts of findings at each severity level.
func (r *Report) CountBySeverity() map[string]int {
	counts := make(map[string]int)
	for _, f := range r.Results {
		sev := f.Severity
		if sev == "" {
			sev = f.Extra.Severity
			if sev == "" {
				sev = "UNKNOWN"
			}
		}
		counts[sev]++
	}
	return counts
}

// FilterBySeverity returns a new report containing only findings
// at the specified severity levels.
func (r *Report) FilterBySeverity(severities []string) *Report {
	if len(severities) == 0 {
		return r
	}
	set := make(map[string]bool)
	for _, s := range severities {
		set[strings.ToUpper(s)] = true
	}
	filtered := &Report{}
	for _, f := range r.Results {
		sev := strings.ToUpper(f.Severity)
		if sev == "" {
			sev = strings.ToUpper(f.Extra.Severity)
		}
		if set[sev] {
			filtered.Results = append(filtered.Results, f)
		}
	}
	return filtered
}
