package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestPrintSummary_CI(t *testing.T) {
	ci := &storage.CISummary{
		TotalPanels: 9, ActivePanels: 1, PostedPanels: 1, RetiredUnpostedPanels: 7, ClosedBeforePost: 1,
		Outcomes: map[string]int{"review_posted": 1}, RetiredByReason: map[string]int{"closed_pr": 2, "unknown": 5},
		DistinctPRs: 2, RunsPerPRP50: 4.5, RunsPerPRP90: 7.3, RunsPerPRMax: 8,
		TurnaroundSamples: 1, TurnaroundP50Secs: 120, TurnaroundP90Secs: 120,
		Cost:                storage.CostAggregate{TotalUSD: 5, JobsWithCost: 2, JobsTotal: 3},
		RetiredUnpostedCost: storage.CostAggregate{TotalUSD: 0, JobsWithCost: 1, JobsTotal: 1, Complete: true},
		PRs:                 []storage.CIPRSummary{{GithubRepo: "acme/api", PRNumber: 7, Runs: 8, Cost: storage.CostAggregate{TotalUSD: 5, JobsWithCost: 2, JobsTotal: 3}}, {GithubRepo: "acme/web", PRNumber: 7, Runs: 1}},
		HotPRs:              []storage.CIPRSummary{{GithubRepo: "acme/api", PRNumber: 7, Runs: 8}},
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	printSummary(cmd, storage.Summary{Since: time.Now(), CI: ci})
	a := assert.New(t)
	for _, want := range []string{"CI Run Health", "Panels: 9", "Active: 1", "Finalized: 1", "Retired unposted: 7", "review_posted: 1", "closed_pr: 2", "unknown: 5", "p50=4.5", "p90=7.3", "max=8", "acme/api#7: 8", "p50=2.0m", "1 samples", "$5.00 (2/3 eligible jobs priced)", "Retired unposted cost: $0.00 (1/1 eligible jobs priced)", "acme/web#7: 1 runs; unavailable (0/0 eligible jobs priced)"} {
		a.Contains(out.String(), want)
	}
	a.NotContains(out.String(), "No review data")
	a.Less(bytes.Index(out.Bytes(), []byte("closed_pr: 2")), bytes.Index(out.Bytes(), []byte("unknown: 5")))
}

func TestPrintSummary_CIUnavailable(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	printSummary(cmd, storage.Summary{Since: time.Now(), CI: &storage.CISummary{TotalPanels: 1, ActivePanels: 1, Cost: storage.CostAggregate{JobsTotal: 2}}})
	assert.Contains(t, out.String(), "Turnaround: unavailable (0 samples)")
	assert.Contains(t, out.String(), "Total cost: unavailable (0/2 eligible jobs priced)")
	out.Reset()
	printSummary(cmd, storage.Summary{Since: time.Now()})
	assert.NotContains(t, out.String(), "CI Run Health")
	assert.Contains(t, out.String(), "No review data")
}
