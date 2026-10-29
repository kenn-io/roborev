package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/client/generated"
)

func experimentsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "experiments", Short: "Read enrolled review experiments"}
	cmd.AddCommand(experimentsReportCmd())
	return cmd
}

func experimentsReportCmd() *cobra.Command {
	var since, until string
	var minSubjects int
	var jsonOutput bool
	cmd := &cobra.Command{
		Use: "report <id>", Short: "Compare an enrolled experiment's arms", Args: cobra.ExactArgs(1),
		Long: `Compare branch subjects enrolled in an immutable experiment.

Time bounds select assignment cohorts, not historical snapshots of results.
Comparisons are exploratory and unadjusted; no overall winner is declared.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if minSubjects <= 0 {
				return fmt.Errorf("min-subjects must be a positive integer")
			}
			var bounds [2]time.Time
			for index, bound := range []struct{ name, value string }{{"since", since}, {"until", until}} {
				if cmd.Flags().Changed(bound.name) {
					parsed, err := time.Parse(time.RFC3339, bound.value)
					if err != nil {
						return fmt.Errorf("%s must be an RFC3339 timestamp", bound.name)
					}
					bounds[index] = parsed
				}
			}
			if cmd.Flags().Changed("since") && cmd.Flags().Changed("until") && bounds[0].After(bounds[1]) {
				return fmt.Errorf("since must not be after until")
			}
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}
			query := generated.GetExperimentReportQuery{MinSubjects: new(strconv.Itoa(minSubjects))}
			if since != "" {
				query.Since = new(since)
			}
			if until != "" {
				query.Until = new(until)
			}
			ep := getDaemonEndpoint()
			resp, err := ep.APIClient(0).GetExperimentReportRaw(cmd.Context(), &generated.GetExperimentReportRequestOptions{
				PathParams: &generated.GetExperimentReportPath{ID: args[0]}, Query: &query,
			})
			if err != nil {
				return fmt.Errorf("failed to connect to daemon: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("daemon returned %s", resp.Status)
			}
			var report storage.ExperimentReport
			if err := json.UnmarshalRead(resp.Body, &report); err != nil {
				return fmt.Errorf("failed to parse response: %w", err)
			}
			if jsonOutput {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout(), jsontext.WithIndent("  ")), report)
			}
			cmd.Print(formatExperimentReport(report))
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "inclusive assignment-time lower bound (RFC3339); default all time")
	cmd.Flags().StringVar(&until, "until", "", "exclusive assignment-time upper bound (RFC3339); default all time")
	cmd.Flags().IntVar(&minSubjects, "min-subjects", 4, "minimum contributing subjects per arm for each metric (positive integer)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "full structured report for scripting")
	return cmd
}

func formatExperimentReport(report storage.ExperimentReport) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Experiment %s (as of %s)\n", report.ID, report.AsOf.UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(&out, "Assignment cohort UTC: [%s, %s); minimum %d subjects per arm\n",
		experimentReportBound(report.Since), experimentReportBound(report.Until), report.MinSubjects)
	fmt.Fprintf(&out, "Outcome basis: %s\n", report.Method.OutcomeBasis)
	for _, arm := range []struct {
		name string
		data storage.ExperimentArmReport
	}{{"default", report.Default}, {"experiment", report.Experiment}} {
		a := arm.data
		fmt.Fprintf(&out, "%s: subjects=%d, review units=%d, terminal=%d, pending=%d, failed=%d, canceled=%d, skipped=%d\n",
			arm.name, a.Subjects, a.ReviewUnits, a.TerminalUnits, a.PendingUnits, a.FailedUnits, a.CanceledUnits, a.SkippedUnits)
		cost := "cost unknown"
		if a.Cost.RecordedUSD != nil {
			cost = fmt.Sprintf("recorded cost=$%.4f", *a.Cost.RecordedUSD)
		}
		fmt.Fprintf(&out, "  %s; %d/%d components priced; %d/%d units have complete cost; attribution=%d/%d units; readable outputs=%d/%d\n",
			cost, a.Cost.PricedComponents, a.Cost.EligibleComponents, a.Cost.CompleteUnits, a.ReviewUnits,
			a.AttributionCompleteUnits, a.ReviewUnits, a.OutputUnits, a.ReviewUnits)
		if a.Cost.Reason != "" {
			fmt.Fprintf(&out, "  %s\n", a.Cost.Reason)
		}
	}
	if !report.Integrity.Complete {
		fmt.Fprintf(&out, "Integrity: %s (conflicting subjects=%d, incomplete units=%d)\n",
			report.Integrity.Reason, report.Integrity.ConflictingSubjects, report.Integrity.IncompleteUnits)
	}
	fmt.Fprintf(&out, "Allocation: default=%d, experiment=%d, stored experimental ratio=%s, chi-square=%s, p=%s",
		report.Allocation.DefaultSubjects, report.Allocation.ExperimentalSubjects, experimentReportNumber(report.Ratio),
		experimentReportNumber(report.Allocation.ChiSquare), experimentReportNumber(report.Allocation.PValue))
	if report.Allocation.Approximate {
		out.WriteString(" (approximate)")
	}
	out.WriteByte('\n')
	if report.Allocation.Warning != "" {
		fmt.Fprintf(&out, "  %s\n", report.Allocation.Warning)
	}
	if !report.Labels.Available {
		fmt.Fprintf(&out, "Finding labels, label coverage, and refuted-review lists unavailable: %s\n", report.Labels.Reason)
	}
	if (!report.Default.Cost.Complete && report.Default.ReviewUnits > 0) || (!report.Experiment.Cost.Complete && report.Experiment.ReviewUnits > 0) {
		out.WriteString("Cost comparison requires full coverage in both arms. Run roborev backfill-tokens for missing usage; prices may remain unavailable.\n")
	}
	if report.Success != nil {
		bar := report.Success
		fmt.Fprintf(&out, "Success bar: %s (%s %s by at least %s)\n  %s\n", bar.State, bar.Metric, bar.Direction, bar.MinEffect, bar.Reason)
		for _, guard := range bar.Guards {
			fmt.Fprintf(&out, "  %s: %s; %s\n", guard.Guard, guard.State, guard.Reason)
		}
	}
	out.WriteString("\nMetrics (experiment minus default; costs in USD, durations in seconds, rates as fractions):\n")
	for _, metric := range report.Metrics {
		fmt.Fprintf(&out, "%s: %s\n", metric.Metric, metric.State)
		fmt.Fprintf(&out, "  default=%s; experiment=%s; delta=%s\n",
			experimentReportMetricValue(metric.Default), experimentReportMetricValue(metric.Experiment), experimentReportNumber(metric.Delta))
		if metric.Interval != nil {
			method := "exact"
			if metric.Approximate {
				method = "approximate"
			}
			fmt.Fprintf(&out, "  bootstrap 95%% interval=[%g, %g]; p=%s; 95%% randomization threshold=%s (%s, %d permutations)\n",
				metric.Interval.Low, metric.Interval.High, experimentReportNumber(metric.PValue),
				experimentReportNumber(metric.RandomizationThreshold), method, metric.Permutations)
		}
		if metric.Reason != "" {
			fmt.Fprintf(&out, "  %s\n", metric.Reason)
		}
	}
	fmt.Fprintf(&out, "\nMethod: exploratory, unadjusted subject-label permutation comparisons; %d resamples. Bootstrap intervals are approximate, including with exact permutations.\n", report.Method.Resamples)
	out.WriteString("The 95% randomization threshold is a null absolute-difference percentile; it is not statistical power or an A/A run.\n")
	return out.String()
}

func experimentReportBound(bound *time.Time) string {
	if bound == nil {
		return "unbounded"
	}
	return bound.UTC().Format(time.RFC3339Nano)
}

func experimentReportNumber(value *float64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatFloat(*value, 'g', 6, 64)
}

func experimentReportMetricValue(value storage.ExperimentMetricValue) string {
	coverage := "unavailable"
	if value.Coverage != nil {
		coverage = fmt.Sprintf("%.1f%%", *value.Coverage*100)
	}
	return fmt.Sprintf("%s (subjects=%d, units=%d/%d, coverage=%s)",
		experimentReportNumber(value.Value), value.Subjects, value.Units, value.ExpectedUnits, coverage)
}
