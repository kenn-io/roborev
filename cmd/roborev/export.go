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

const (
	exportReviewsMaxPageSize         = 5000
	exportReviewsCursorResetExitCode = 3
)

var errExportCursorDatabaseReset = errors.New("export cursor database reset")

type exportReviewsOpts struct {
	format     string
	profile    string
	since      string
	until      string
	cursor     string
	closedOnly bool
	repo       string
	project    string
	limit      int
}

func exportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export roborev data",
	}
	cmd.AddCommand(exportReviewsCmd())
	return cmd
}

func exportReviewsCmd() *cobra.Command {
	var opts exportReviewsOpts
	cmd := &cobra.Command{
		Use:   "reviews",
		Args:  cobra.NoArgs,
		Short: "Export completed reviews as JSON",
		Long: strings.TrimSpace(`
Export completed reviews as a JSON document.

Rows are ordered by completed_at, review_id ascending. Use --cursor with the
next_cursor value from a previous export to resume strictly after that position.
--cursor cannot be used with --since; --until, --limit, --profile, and filters
still apply. Every non-empty export includes next_cursor; truncated means more
matching rows are available immediately.

Cursor tokens are opaque and versioned for stability across invocations and
roborev upgrades. Export documents include database_id; a cursor from a
different database is rejected with exit code 3 so callers can discard it and
backfill. Other cursor rejections exit non-zero and should also be handled by
discarding the cursor and retrying with a window backfill. Reviews that complete
late with completed_at before an already consumed cursor position are not
returned by cursor resume; consumers that need convergence should use an
overlapping window separately.`),
		RunE: func(cmd *cobra.Command, args []string) error {
			limitSet := cmd.Flags().Changed("limit")
			if err := validateExportReviewsOpts(opts, limitSet); err != nil {
				return usageErr(cmd, err)
			}
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			doc, err := fetchAllExportReviews(getDaemonEndpoint(), opts, limitSet)
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
	cmd.Flags().StringVar(&opts.profile, "profile", string(storage.ExportProfileContent), "export profile (content or metadata)")
	cmd.Flags().StringVar(&opts.since, "since", "", "inclusive completed_at lower bound (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&opts.until, "until", "", "exclusive completed_at upper bound (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&opts.cursor, "cursor", "", "opaque next_cursor from a previous export; resumes after that cursor and cannot be used with --since")
	cmd.Flags().BoolVar(&opts.closedOnly, "closed-only", false, "only include reviews marked closed")
	cmd.Flags().StringVar(&opts.repo, "repo", "", "exact exported repo identifier filter")
	cmd.Flags().StringVar(&opts.project, "project", "", "exact project display-name filter")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "maximum number of top-level reviews to emit")
	return cmd
}

func validateExportReviewsOpts(opts exportReviewsOpts, limitSet bool) error {
	if opts.format != "json" {
		return fmt.Errorf("unsupported export format %q", opts.format)
	}
	if opts.profile != string(storage.ExportProfileContent) &&
		opts.profile != string(storage.ExportProfileMetadata) {
		return fmt.Errorf("unsupported export profile %q", opts.profile)
	}
	if limitSet && opts.limit <= 0 {
		return fmt.Errorf("--limit must be greater than 0")
	}
	if opts.cursor != "" && opts.since != "" {
		return fmt.Errorf("--cursor cannot be used with --since; cursor already defines the resume position")
	}
	return nil
}

func fetchAllExportReviews(ep daemon.DaemonEndpoint, opts exportReviewsOpts, limitSet bool) (*daemon.ExportReviewsDocument, error) {
	var out *daemon.ExportReviewsDocument
	cursor := opts.cursor
	remaining := opts.limit
	for {
		pageLimit := 0
		if limitSet {
			pageLimit = min(remaining, exportReviewsMaxPageSize)
		}
		page, err := fetchExportReviewsPage(ep, opts, cursor, pageLimit)
		if err != nil {
			return nil, err
		}
		if out == nil {
			copy := page
			copy.Reviews = append([]storage.ExportReview{}, page.Reviews...)
			out = &copy
		} else {
			out.Reviews = append(out.Reviews, page.Reviews...)
			out.Truncated = page.Truncated
			out.NextCursor = page.NextCursor
		}

		if limitSet {
			remaining -= len(page.Reviews)
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
		if len(page.Reviews) == 0 {
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

func fetchExportReviewsPage(ep daemon.DaemonEndpoint, opts exportReviewsOpts, cursor string, limit int) (daemon.ExportReviewsDocument, error) {
	params := url.Values{}
	params.Set("format", opts.format)
	params.Set("profile", opts.profile)
	if opts.since != "" && cursor == "" {
		params.Set("since", opts.since)
	}
	if opts.until != "" {
		params.Set("until", opts.until)
	}
	if opts.closedOnly {
		params.Set("closed_only", "true")
	}
	if opts.repo != "" {
		params.Set("repo", opts.repo)
	}
	if opts.project != "" {
		params.Set("project", opts.project)
	}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		params.Set("cursor", cursor)
	}

	resp, err := ep.HTTPClient(30 * time.Second).Get(ep.BaseURL() + "/api/export/reviews?" + params.Encode())
	if err != nil {
		return daemon.ExportReviewsDocument{}, fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusConflict {
			return daemon.ExportReviewsDocument{}, fmt.Errorf("%w: daemon returned %s: %s",
				errExportCursorDatabaseReset, resp.Status, strings.TrimSpace(string(body)))
		}
		return daemon.ExportReviewsDocument{}, fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var doc daemon.ExportReviewsDocument
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return daemon.ExportReviewsDocument{}, fmt.Errorf("failed to parse export response: %w", err)
	}
	return doc, nil
}
