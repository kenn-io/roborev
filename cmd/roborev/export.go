package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

const (
	exportReviewsMaxPageSize         = 5000
	exportReviewsCursorResetExitCode = 3
)

var errExportCursorDatabaseReset = errors.New("export cursor database reset")

type exportReviewsOpts struct {
	format           string
	profile          string
	schemaVersionRaw string
	schemaVersion    int
	schemaVersionSet bool
	since            string
	until            string
	cursor           string
	closedOnly       bool
	updatedSince     string
	repo             string
	project          string
	limit            int
}

type exportReviewsRawPage struct {
	raw           map[string]jsontext.Value
	schemaVersion int
	databaseID    string
	profile       string
	truncated     bool
	nextCursor    *string
	reviews       []jsontext.Value
}

func exportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export roborev data",
	}
	cmd.AddCommand(exportReviewsCmd())
	cmd.AddCommand(exportCIMetricsCmd())
	cmd.AddCommand(exportCICostCmd())
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

The content profile exports each review and subagent in two fields. document is
the stored JSON review document, the canonical form. content is the Markdown
rendering of it. Both are null in the metadata profile and for a review that
has no stored document.

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
overlapping window separately.

Each review reports closed and updated_at. updated_at advances when a review is
closed or reopened, but completed_at does not, so cursor resume never returns a
review again after its closed state changes. --updated-since is an inclusive
updated_at lower bound that works as a filter: it combines with --since,
--until, --cursor, --limit, --profile, and the other filters, and rows stay
ordered by completed_at. Consumers that track closed state should combine the
completed_at cursor pull with a separate --updated-since pull.

Use --schema-version to pin a specific export schema version (supported: 2).
Omitting --schema-version uses the connected daemon's current default; consumers
that require stability across upgrades should pass --schema-version explicitly.
Within a schema version, changes are strictly additive: existing fields are
never removed, renamed, or changed in type or nullability, and consumers should
ignore unknown fields. Export schema_version 2 emits structured document objects
in document version 3 without unlikely or nit (and version 0 for historical
unstructured Markdown reviews).`),
		RunE: func(cmd *cobra.Command, args []string) error {
			limitSet := cmd.Flags().Changed("limit")
			opts.schemaVersionSet = cmd.Flags().Changed("schema-version")
			if err := validateExportReviewsOpts(&opts, limitSet); err != nil {
				return usageErr(cmd, err)
			}
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}

			doc, err := fetchAllExportReviews(cmd.Context(), getDaemonEndpoint(), opts, limitSet)
			if err != nil {
				if errors.Is(err, errExportCursorDatabaseReset) {
					return &exitError{code: exportReviewsCursorResetExitCode, cause: err}
				}
				return err
			}
			return writeExportReviewsDocument(cmd.OutOrStdout(), doc)
		},
	}
	cmd.Flags().StringVar(&opts.format, "format", "json", "output format")
	cmd.Flags().StringVar(&opts.profile, "profile", string(storage.ExportProfileContent), "export profile (content or metadata)")
	cmd.Flags().StringVar(
		&opts.schemaVersionRaw,
		"schema-version",
		"",
		fmt.Sprintf("export schema version (supported: %s; omit for daemon default %d)", daemon.FormatSupportedExportReviewsSchemaVersions(), daemon.ExportReviewsSchemaVersion),
	)
	cmd.Flags().StringVar(&opts.since, "since", "", "inclusive completed_at lower bound (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&opts.until, "until", "", "exclusive completed_at upper bound (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&opts.cursor, "cursor", "", "opaque next_cursor from a previous export; resumes after that cursor and cannot be used with --since")
	cmd.Flags().BoolVar(&opts.closedOnly, "closed-only", false, "only include reviews marked closed")
	cmd.Flags().StringVar(&opts.updatedSince, "updated-since", "", "inclusive updated_at lower bound (RFC3339 or YYYY-MM-DD); a filter that combines with --cursor and the other flags")
	cmd.Flags().StringVar(&opts.repo, "repo", "", "exact exported repo identifier filter")
	cmd.Flags().StringVar(&opts.project, "project", "", "exact project display-name filter")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "maximum number of top-level reviews to emit")
	return cmd
}

func validateExportReviewsOpts(opts *exportReviewsOpts, limitSet bool) error {
	if opts.format != "json" {
		return invalidValue(fmt.Errorf("unsupported export format %q", opts.format))
	}
	if opts.profile != string(storage.ExportProfileContent) &&
		opts.profile != string(storage.ExportProfileMetadata) {
		return invalidValue(fmt.Errorf("unsupported export profile %q", opts.profile))
	}
	if opts.schemaVersionSet {
		raw := opts.schemaVersionRaw
		if raw == "" || strings.TrimSpace(raw) != raw {
			return invalidValue(fmt.Errorf("invalid export schema version %q", raw))
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			return invalidValue(fmt.Errorf("invalid export schema version %q (must be a positive integer)", raw))
		}
		// Let the connected daemon decide which positive schema versions it
		// supports so an older CLI can pin a newer daemon's version.
		opts.schemaVersion = v
	}
	if limitSet && opts.limit <= 0 {
		return invalidValue(fmt.Errorf("--limit must be greater than 0"))
	}
	if opts.cursor != "" && opts.since != "" {
		return fmt.Errorf("--cursor cannot be used with --since; cursor already defines the resume position")
	}
	return nil
}

func fetchAllExportReviews(ctx context.Context, ep daemon.DaemonEndpoint, opts exportReviewsOpts, limitSet bool) (map[string]jsontext.Value, error) {
	var out *exportReviewsRawPage
	cursor := opts.cursor
	remaining := opts.limit
	for {
		pageLimit := 0
		if limitSet {
			pageLimit = min(remaining, exportReviewsMaxPageSize)
		}
		page, err := fetchExportReviewsPage(ctx, ep, opts, cursor, pageLimit)
		if err != nil {
			return nil, err
		}
		if opts.schemaVersionSet && page.schemaVersion != opts.schemaVersion {
			return nil, invalidValue(fmt.Errorf(
				"daemon returned export schema_version %d, requested %d",
				page.schemaVersion, opts.schemaVersion,
			))
		}
		if out == nil {
			copied := page
			copied.reviews = append([]jsontext.Value{}, page.reviews...)
			out = &copied
		} else {
			if page.schemaVersion != out.schemaVersion {
				return nil, invalidValue(fmt.Errorf(
					"daemon changed export schema_version mid-export from %d to %d",
					out.schemaVersion, page.schemaVersion,
				))
			}
			if page.databaseID != out.databaseID {
				return nil, fmt.Errorf(
					"%w: daemon changed export database_id mid-export from %q to %q",
					errExportCursorDatabaseReset, out.databaseID, page.databaseID,
				)
			}
			if page.profile != out.profile {
				return nil, fmt.Errorf(
					"daemon changed export profile mid-export from %q to %q",
					out.profile, page.profile,
				)
			}
			out.reviews = append(out.reviews, page.reviews...)
			out.truncated = page.truncated
			if len(page.reviews) > 0 {
				out.nextCursor = page.nextCursor
			}
		}

		if limitSet {
			remaining -= len(page.reviews)
			if remaining <= 0 {
				break
			}
		}
		if !page.truncated {
			break
		}
		if page.nextCursor == nil || *page.nextCursor == "" {
			return nil, fmt.Errorf("daemon returned truncated export page without next cursor")
		}
		if len(page.reviews) == 0 {
			return nil, fmt.Errorf("daemon returned empty export page with next cursor")
		}
		cursor = *page.nextCursor
	}
	if out == nil {
		return nil, fmt.Errorf("daemon returned no export page")
	}
	if !limitSet {
		out.truncated = false
	}

	// Strip Huma's transport-level $schema URL (bound to local daemon host/port)
	// while keeping all export fields and unknown additive fields.
	delete(out.raw, "$schema")

	rawReviews, err := json.Marshal(out.reviews)
	if err != nil {
		return nil, fmt.Errorf("encode export reviews: %w", err)
	}
	rawTruncated, err := json.Marshal(out.truncated)
	if err != nil {
		return nil, fmt.Errorf("encode export truncated flag: %w", err)
	}
	rawNextCursor, err := json.Marshal(out.nextCursor)
	if err != nil {
		return nil, fmt.Errorf("encode export next_cursor: %w", err)
	}
	out.raw["reviews"] = jsontext.Value(rawReviews)
	out.raw["truncated"] = jsontext.Value(rawTruncated)
	out.raw["next_cursor"] = jsontext.Value(rawNextCursor)
	return out.raw, nil
}

func isDaemonSchemaVersionError(bodyText string) bool {
	detail := bodyText
	var problem struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal([]byte(bodyText), &problem) == nil && problem.Detail != "" {
		detail = problem.Detail
	}
	return strings.HasPrefix(detail, "invalid export schema_version") ||
		strings.HasPrefix(detail, "unsupported export schema_version") ||
		strings.HasPrefix(detail, "duplicate export schema_version")
}

func fetchExportReviewsPage(ctx context.Context, ep daemon.DaemonEndpoint, opts exportReviewsOpts, cursor string, limit int) (exportReviewsRawPage, error) {
	params := url.Values{}
	params.Set("format", opts.format)
	params.Set("profile", opts.profile)
	if opts.schemaVersionSet {
		params.Set("schema_version", strconv.Itoa(opts.schemaVersion))
	}
	if opts.since != "" && cursor == "" {
		params.Set("since", opts.since)
	}
	if opts.until != "" {
		params.Set("until", opts.until)
	}
	if opts.closedOnly {
		params.Set("closed_only", "true")
	}
	if opts.updatedSince != "" {
		params.Set("updated_since", opts.updatedSince)
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

	resp, err := ep.APIClient(30*time.Second).ExportReviewsRaw(ctx, nil, roborevclient.WithQuery(params))
	if err != nil {
		return exportReviewsRawPage{}, fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyText := strings.TrimSpace(string(body))
		if resp.StatusCode == http.StatusConflict {
			return exportReviewsRawPage{}, fmt.Errorf("%w: daemon returned %s: %s",
				errExportCursorDatabaseReset, resp.Status, bodyText)
		}
		if opts.schemaVersionSet && resp.StatusCode == http.StatusBadRequest && isDaemonSchemaVersionError(bodyText) {
			return exportReviewsRawPage{}, invalidValue(fmt.Errorf("daemon returned %s: %s", resp.Status, bodyText))
		}
		return exportReviewsRawPage{}, fmt.Errorf("daemon returned %s: %s", resp.Status, bodyText)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return exportReviewsRawPage{}, fmt.Errorf("failed to read export response: %w", err)
	}

	// Preserve raw JSON values and unknown additive fields on the envelope,
	// reviews, subagents, and nested documents without strict struct decoding.
	var raw map[string]jsontext.Value
	if err := json.Unmarshal(body, &raw); err != nil {
		return exportReviewsRawPage{}, fmt.Errorf("failed to parse export response: %w", err)
	}
	if raw == nil {
		return exportReviewsRawPage{}, fmt.Errorf("failed to parse export response: expected JSON object")
	}

	page := exportReviewsRawPage{
		raw:     raw,
		reviews: []jsontext.Value{},
	}
	if v, ok := raw["schema_version"]; ok {
		if err := json.Unmarshal(v, &page.schemaVersion); err != nil {
			return exportReviewsRawPage{}, fmt.Errorf("failed to parse export response schema_version: %w", err)
		}
	}
	if v, ok := raw["database_id"]; ok {
		if err := json.Unmarshal(v, &page.databaseID); err != nil {
			return exportReviewsRawPage{}, fmt.Errorf("failed to parse export response database_id: %w", err)
		}
	}
	if v, ok := raw["profile"]; ok {
		if err := json.Unmarshal(v, &page.profile); err != nil {
			return exportReviewsRawPage{}, fmt.Errorf("failed to parse export response profile: %w", err)
		}
	}
	if v, ok := raw["truncated"]; ok {
		if err := json.Unmarshal(v, &page.truncated); err != nil {
			return exportReviewsRawPage{}, fmt.Errorf("failed to parse export response truncated: %w", err)
		}
	}
	if v, ok := raw["next_cursor"]; ok {
		if err := json.Unmarshal(v, &page.nextCursor); err != nil {
			return exportReviewsRawPage{}, fmt.Errorf("failed to parse export response next_cursor: %w", err)
		}
	}
	if v, ok := raw["reviews"]; ok {
		var reviews []jsontext.Value
		if err := json.Unmarshal(v, &reviews); err != nil {
			return exportReviewsRawPage{}, fmt.Errorf("failed to parse export response reviews: %w", err)
		}
		if reviews != nil {
			page.reviews = reviews
		}
	}
	return page, nil
}

var exportReviewsHeaderKeyOrder = []string{
	"schema_version",
	"tool",
	"tool_version",
	"generated_at",
	"database_id",
	"profile",
	"window",
	"truncated",
	"next_cursor",
	"reviews",
}

func writeExportReviewsDocument(w io.Writer, raw map[string]jsontext.Value) error {
	enc := jsontext.NewEncoder(w, jsontext.WithIndent("  "))
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	written := make(map[string]struct{}, len(exportReviewsHeaderKeyOrder))
	for _, key := range exportReviewsHeaderKeyOrder {
		val, ok := raw[key]
		if !ok {
			continue
		}
		written[key] = struct{}{}
		if err := enc.WriteToken(jsontext.String(key)); err != nil {
			return err
		}
		if err := enc.WriteValue(val); err != nil {
			return err
		}
	}
	var extraKeys []string
	for key := range raw {
		if _, ok := written[key]; !ok {
			extraKeys = append(extraKeys, key)
		}
	}
	slices.Sort(extraKeys)
	for _, key := range extraKeys {
		if err := enc.WriteToken(jsontext.String(key)); err != nil {
			return err
		}
		if err := enc.WriteValue(raw[key]); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}
