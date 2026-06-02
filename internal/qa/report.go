// Package qa provides unified quality assurance orchestration and reporting.
// It runs lint (Semgrep), mutation testing (mutmut/ooze), and optional AI review,
// then merges findings into a single pass/fail report with composite scoring.
package qa

import (
	"go.kenn.io/roborev/internal/lint"
	"go.kenn.io/roborev/internal/mutate"
)

// Report is the unified quality assurance report merging all phases.
type Report struct {
	// Phase results — nil if the phase was skipped or failed.
	Lint   *lint.Report   `json:"lint,omitempty"`
	Mutate *mutate.Result `json:"mutate,omitempty"`

	// Gates track pass/fail for each quality threshold.
	Gates []Gate `json:"gates"`

	// Score is a composite 0.0–1.0 across all phases.
	Score float64 `json:"score"`

	// Skipped lists which phases were skipped and why.
	Skipped []string `json:"skipped,omitempty"`
}

// Gate represents a single quality threshold check.
type Gate struct {
	Name    string  `json:"name"`
	Passed  bool    `json:"passed"`
	Score   float64 `json:"score"`
	Limit   float64 `json:"limit"`
	Count   int     `json:"count,omitempty"` // for count-based gates (lint)
	Details string  `json:"details"`
}

// Options configures what phases run and their thresholds.
type Options struct {
	// Skip phases entirely.
	SkipLint   bool
	SkipMutate bool

	// Thresholds for gate checks. Zero means no gate.
	MaxLintFindings int     // fail if finding count exceeds this
	MinMutateScore  float64 // fail if mutation score is below this (0.0-1.0)

	// Paths to scan (for lint). Empty means project root.
	Paths []string
}

// ComputeScore calculates a composite score from all available phases.
// Lint score: 1.0 if clean, scales down with finding count.
// Mutate score: direct mutation score.
func (r *Report) ComputeScore() {
	var scores []float64

	if r.Lint != nil {
		count := len(r.Lint.Results)
		if count == 0 {
			scores = append(scores, 1.0)
		} else if count <= 5 {
			scores = append(scores, 0.8)
		} else if count <= 20 {
			scores = append(scores, 0.5)
		} else {
			scores = append(scores, 0.2)
		}
	}

	if r.Mutate != nil && r.Mutate.Total > 0 {
		scores = append(scores, r.Mutate.Score)
	}

	if len(scores) == 0 {
		r.Score = 0
		return
	}

	sum := 0.0
	for _, s := range scores {
		sum += s
	}
	r.Score = sum / float64(len(scores))
}

// RunGates checks all configured gates against the results.
func (r *Report) RunGates(opts Options) {
	if opts.MaxLintFindings > 0 && r.Lint != nil {
		count := len(r.Lint.Results)
		passed := count <= opts.MaxLintFindings
		score := 1.0
		if count > 0 && opts.MaxLintFindings > 0 {
			score = 1.0 - float64(count)/float64(opts.MaxLintFindings*2)
			if score < 0 {
				score = 0
			}
		}
		r.Gates = append(r.Gates, Gate{
			Name:    "lint",
			Passed:  passed,
			Score:   score,
			Limit:   float64(opts.MaxLintFindings),
			Count:   count,
			Details: lintGateDetails(count, opts.MaxLintFindings),
		})
	}

	if opts.MinMutateScore > 0 && r.Mutate != nil {
		passed := r.Mutate.Score >= opts.MinMutateScore
		r.Gates = append(r.Gates, Gate{
			Name:    "mutate",
			Passed:  passed,
			Score:   r.Mutate.Score,
			Limit:   opts.MinMutateScore,
			Details: mutateGateDetails(r.Mutate),
		})
	}
}

func lintGateDetails(count, max int) string {
	if count == 0 {
		return "No findings — clean lint"
	}
	if count <= max {
		return "Findings within threshold"
	}
	return "Too many findings — lint gate failed"
}

func mutateGateDetails(r *mutate.Result) string {
	if r.Total == 0 {
		return "No mutants generated"
	}
	return "Mutation score"
}
