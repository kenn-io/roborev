package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

type exportCIMetricsOpts struct {
	format string
	since  string
	until  string
	cursor string
	limit  int
	legacy bool
}

func exportCIMetricsCmd() *cobra.Command {
	var opts exportCIMetricsOpts
	cmd := &cobra.Command{
		Use:   "ci-metrics",
		Args:  cobra.NoArgs,
		Short: "Export finalized CI panel metrics as JSON",
		Long: strings.TrimSpace(`
Export finalized CI panel runs (terminal outcome, first-attempt and posting
timestamps, attempt count, and per-panel member/synthesis jobs) as a JSON
document, for external turnaround-time tracking.

Rows are ordered by posted_at, panel_id ascending. Use --cursor with the
next_cursor value from a previous export to resume strictly after that
position. --cursor cannot be used with --since; --until and --limit still
apply.

Cursor tokens are opaque and versioned. Export documents include
database_id; a cursor from a different database is rejected with exit code
3 so callers can discard it and backfill. Panels finalized before outcome
persistence existed export with outcome "unknown" and null
first_attempt_at.

Use --legacy to export the frozen pre-panel CI era instead: rows from the
retired ci_pr_reviews table (roughly 2026-02 through 2026-06), one per
reviewed PR head, with outcome "legacy_review". This is a one-time backfill
source, not an ongoing feed — legacy first_attempt_at is job enqueue time,
not comparable to panel-era first_attempt_at. Legacy cursors cannot be
resumed against a non-legacy export, or vice versa.`),
		RunE: func(cmd *cobra.Command, args []string) error {
			limitSet := cmd.Flags().Changed("limit")
			if err := validateExportCIMetricsOpts(opts, limitSet); err != nil {
				return usageErr(cmd, err)
			}
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			doc, err := fetchAllExportCIMetrics(getDaemonEndpoint(), opts, limitSet)
			if err != nil {
				if errors.Is(err, errExportCursorDatabaseReset) {
					return &exitError{code: exportReviewsCursorResetExitCode, cause: err}
				}
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(doc)
		},
	}
	cmd.Flags().StringVar(&opts.format, "format", "json", "output format")
	cmd.Flags().StringVar(&opts.since, "since", "", "inclusive posted_at lower bound (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&opts.until, "until", "", "exclusive posted_at upper bound (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&opts.cursor, "cursor", "", "opaque next_cursor from a previous export; cannot be used with --since")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "maximum number of panels to emit")
	cmd.Flags().BoolVar(&opts.legacy, "legacy", false,
		"export the frozen pre-panel CI era (ci_pr_reviews) instead of panel runs; for one-time backfill")
	return cmd
}

func validateExportCIMetricsOpts(opts exportCIMetricsOpts, limitSet bool) error {
	if opts.format != "json" {
		return fmt.Errorf("unsupported export format %q", opts.format)
	}
	if limitSet && opts.limit <= 0 {
		return fmt.Errorf("--limit must be greater than 0")
	}
	if opts.cursor != "" && opts.since != "" {
		return fmt.Errorf("--cursor cannot be used with --since; cursor already defines the resume position")
	}
	return nil
}

func fetchAllExportCIMetrics(ep daemon.DaemonEndpoint, opts exportCIMetricsOpts, limitSet bool) (*daemon.ExportCIMetricsDocument, error) {
	var out *daemon.ExportCIMetricsDocument
	cursor := opts.cursor
	remaining := opts.limit
	for {
		pageLimit := 0
		if limitSet {
			pageLimit = min(remaining, exportReviewsMaxPageSize)
		}
		page, err := fetchExportCIMetricsPage(ep, opts, cursor, pageLimit)
		if err != nil {
			return nil, err
		}
		if out == nil {
			copy := page
			copy.Panels = append([]storage.ExportCIPanel{}, page.Panels...)
			out = &copy
		} else {
			out.Panels = append(out.Panels, page.Panels...)
			out.Truncated = page.Truncated
			out.NextCursor = page.NextCursor
		}

		if limitSet {
			remaining -= len(page.Panels)
			if remaining <= 0 {
				break
			}
		}
		if !page.Truncated {
			break
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return nil, fmt.Errorf("daemon returned truncated export page without next cursor")
		}
		if len(page.Panels) == 0 {
			return nil, fmt.Errorf("daemon returned empty export page with next cursor")
		}
		cursor = *page.NextCursor
	}
	if out == nil {
		return nil, fmt.Errorf("daemon returned no export page")
	}
	if !limitSet {
		out.Truncated = false
	}
	return out, nil
}

func fetchExportCIMetricsPage(ep daemon.DaemonEndpoint, opts exportCIMetricsOpts, cursor string, limit int) (daemon.ExportCIMetricsDocument, error) {
	params := url.Values{}
	params.Set("format", opts.format)
	if opts.since != "" && cursor == "" {
		params.Set("since", opts.since)
	}
	if opts.until != "" {
		params.Set("until", opts.until)
	}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		params.Set("cursor", cursor)
	}
	if opts.legacy {
		params.Set("legacy", "true")
	}

	resp, err := ep.HTTPClient(30 * time.Second).Get(ep.BaseURL() + "/api/export/ci-metrics?" + params.Encode())
	if err != nil {
		return daemon.ExportCIMetricsDocument{}, fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusConflict {
			return daemon.ExportCIMetricsDocument{}, fmt.Errorf("%w: daemon returned %s: %s",
				errExportCursorDatabaseReset, resp.Status, strings.TrimSpace(string(body)))
		}
		return daemon.ExportCIMetricsDocument{}, fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var doc daemon.ExportCIMetricsDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return daemon.ExportCIMetricsDocument{}, fmt.Errorf("failed to parse export response: %w", err)
	}
	return doc, nil
}
