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

type exportCICostOpts struct {
	format string
	since  string
	until  string
	cursor string
	limit  int
	legacy bool
}

func exportCICostCmd() *cobra.Command {
	var opts exportCICostOpts
	cmd := &cobra.Command{
		Use:   "ci-costs",
		Args:  cobra.NoArgs,
		Short: "Export job-level CI costs as JSON",
		Long: strings.TrimSpace(`
Export cost-eligible CI jobs as a JSON document for external cost reporting.
Successful, failed, and canceled attempts are included when an agent ran;
jobs without usable pricing remain in the export with cost_usd set to null.

Rows are ordered by finished_at and job_id for stable pagination. A fresh
export over an overlapping window returns current pricing, allowing an
idempotent consumer to pick up late prices. Use --cursor with the next_cursor
value from a previous export. The cursor retains the original time bounds and
cannot be used with --since or --until. A cursor from a different database is
rejected with exit code 3.

Use --legacy to export eligible jobs from the frozen pre-panel CI era. The
same structural grouping used by ci-metrics identifies historical CI units.
Legacy cursors cannot be resumed against a regular export, or vice versa.`),
		RunE: func(cmd *cobra.Command, args []string) error {
			limitSet := cmd.Flags().Changed("limit")
			if err := validateExportCICostOpts(opts, limitSet); err != nil {
				return usageErr(cmd, err)
			}
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			doc, err := fetchAllExportCICosts(getDaemonEndpoint(), opts, limitSet)
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
	cmd.Flags().StringVar(&opts.since, "since", "", "inclusive finished_at lower bound (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&opts.until, "until", "", "finished_at upper bound (RFC3339 exclusive; YYYY-MM-DD includes that UTC day)")
	cmd.Flags().StringVar(&opts.cursor, "cursor", "", "opaque next_cursor from a previous export; cannot be used with time bounds")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "maximum number of jobs to emit")
	cmd.Flags().BoolVar(&opts.legacy, "legacy", false,
		"export structurally identified pre-panel CI jobs; for one-time backfill")
	return cmd
}

func validateExportCICostOpts(opts exportCICostOpts, limitSet bool) error {
	if opts.format != "json" {
		return fmt.Errorf("unsupported export format %q", opts.format)
	}
	if limitSet && opts.limit <= 0 {
		return errors.New("--limit must be greater than 0")
	}
	if opts.cursor != "" && (opts.since != "" || opts.until != "") {
		return errors.New("--cursor cannot be used with --since or --until; cursor already defines the export window")
	}
	return nil
}

func fetchAllExportCICosts(
	ep daemon.DaemonEndpoint, opts exportCICostOpts, limitSet bool,
) (*daemon.ExportCICostDocument, error) {
	var out *daemon.ExportCICostDocument
	cursor := opts.cursor
	remaining := opts.limit
	for {
		pageLimit := 0
		if limitSet {
			pageLimit = min(remaining, exportReviewsMaxPageSize)
		}
		page, err := fetchExportCICostPage(ep, opts, cursor, pageLimit)
		if err != nil {
			return nil, err
		}
		if out == nil {
			copy := page
			copy.Jobs = append([]storage.ExportCICostJob{}, page.Jobs...)
			out = &copy
		} else {
			out.Jobs = append(out.Jobs, page.Jobs...)
			out.Truncated = page.Truncated
			out.NextCursor = page.NextCursor
		}

		if limitSet {
			remaining -= len(page.Jobs)
			if remaining <= 0 {
				break
			}
		}
		if !page.Truncated {
			break
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return nil, errors.New("daemon returned truncated export page without next cursor")
		}
		if len(page.Jobs) == 0 {
			return nil, errors.New("daemon returned empty truncated export page with next cursor")
		}
		cursor = *page.NextCursor
	}
	if out == nil {
		return nil, errors.New("daemon returned no export page")
	}
	if !limitSet {
		out.Truncated = false
	}
	return out, nil
}

func fetchExportCICostPage(
	ep daemon.DaemonEndpoint, opts exportCICostOpts, cursor string, limit int,
) (daemon.ExportCICostDocument, error) {
	params := url.Values{}
	params.Set("format", opts.format)
	if opts.since != "" && cursor == "" {
		params.Set("since", opts.since)
	}
	if opts.until != "" && cursor == "" {
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

	resp, err := ep.HTTPClient(30 * time.Second).Get(ep.BaseURL() + "/api/export/ci-costs?" + params.Encode())
	if err != nil {
		return daemon.ExportCICostDocument{}, fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusConflict {
			return daemon.ExportCICostDocument{}, fmt.Errorf("%w: daemon returned %s: %s",
				errExportCursorDatabaseReset, resp.Status, strings.TrimSpace(string(body)))
		}
		return daemon.ExportCICostDocument{}, fmt.Errorf("daemon returned %s: %s",
			resp.Status, strings.TrimSpace(string(body)))
	}
	var doc daemon.ExportCICostDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return daemon.ExportCICostDocument{}, fmt.Errorf("failed to parse export response: %w", err)
	}
	return doc, nil
}
