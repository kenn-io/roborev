package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/pkg/client/generated"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func legacyReviewsCmd() *cobra.Command {
	var dbPath, postgresURL string
	cmd := &cobra.Command{Use: "legacy-reviews", Short: "Convert, export, and resolve historical Markdown reviews"}
	cmd.PersistentFlags().StringVar(&dbPath, "db", "", "Verify the database path owned by the selected daemon; use --server to select another daemon")
	cmd.PersistentFlags().StringVar(&postgresURL, "postgres-url", "", "PostgreSQL connection URL for archived mirror reviews")
	cmd.MarkFlagsMutuallyExclusive("db", "postgres-url")
	var dryRun bool
	convert := &cobra.Command{
		Use: "convert", Args: cobra.NoArgs,
		Short: "Restore archived reviews that roborev can read back exactly, without an AI agent",
		Long: strings.TrimSpace(`
Convert archived Markdown reviews to JSON review documents and restore them.

convert reads only formats that roborev itself wrote: the Markdown roborev
renders from a review document, the "## Review Findings" list with Severity,
Location, Problem, and Fix bullets, reviews that state "No issues found.", and
the SEVERITY_THRESHOLD_MET marker. It copies stated fields and never guesses. A
review stays unstructured when a finding lacks a severity, problem, or fix, when
text falls outside the recognized structure, when the sources of a combined
panel review cannot be recovered, or when the converted findings would change
the recorded verdict. Use export and import for those.

Each conversion goes through the same validation as import, and the original
stays archived. Running convert again is safe: it only sees reviews that are
still unresolved. The daemon may already have restored recognized archived reviews at startup;
the report describes conversions still remaining.

--dry-run changes nothing. It reports how many reviews would convert and
counts the rest by refusal reason. The selected daemon owns the database
throughout conversion.`),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureDaemon(); err != nil {
				return err
			}
			report, err := getDaemonEndpoint().APIClient(0).ConvertLegacyReviews(cmd.Context(), &generated.ConvertLegacyReviewsRequestOptions{
				Body: &generated.ConvertLegacyReviewsBody{DB: &dbPath, PostgresURL: &postgresURL, DryRun: dryRun},
			})
			if err != nil {
				return daemonRequestError("convert legacy reviews", err)
			}

			return writeLegacyConversionReport(cmd.OutOrStdout(), report)
		},
	}
	convert.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would convert and why the rest would not, without changing anything")
	cmd.AddCommand(convert)
	cmd.AddCommand(&cobra.Command{
		Use: "export", Args: cobra.NoArgs,
		Short: "Export unresolved records and the JSON schema for an AI agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureDaemon(); err != nil {
				return err
			}
			result, err := getDaemonEndpoint().APIClient(0).ExportLegacyReviews(cmd.Context(), &generated.ExportLegacyReviewsRequestOptions{
				Body: &generated.LegacyMaintenanceTarget{DB: &dbPath, PostgresURL: &postgresURL},
			})
			if err != nil {
				return daemonRequestError("export legacy reviews", err)
			}
			var records any = result.SqliteRecords
			if postgresURL != "" {
				records = result.PostgresRecords
			}

			return json.MarshalWrite(cmd.OutOrStdout(), struct {
				Instructions    string         `json:"instructions"`
				Schema          jsontext.Value `json:"schema"`
				SynthesisSchema jsontext.Value `json:"synthesis_schema"`
				Records         any            `json:"records"`
			}{
				"Convert each historical review into the supplied JSON model. Never replace an already structured review. Preserve every finding and its severity, problem, fix, location, and synthesis source numbers when present. Do not invent missing information or re-review the code. Leave ambiguous records unresolved and report why. Return one JSON file per resolved record for import with roborev legacy-reviews and the same --server and --postgres-url options, followed by import <id> < converted.json. The id is the archive ID. When review_id is present, you may instead POST {review_id, document} to the running daemon at /api/review/migrate.",
				structuredreview.Schema, structuredreview.SourcedSchema, records,
			})
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "import <id>", Args: cobra.ExactArgs(1),
		Short: "Validate a converted JSON document from stdin and restore its review",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return err
			}
			if _, err := structuredreview.Decode(raw); err != nil {
				return err
			}
			request := generated.ImportLegacyReviewBody{DB: &dbPath, PostgresURL: &postgresURL, Document: raw}
			if postgresURL != "" {
				id, err := uuid.Parse(args[0]) //nolint:forbidigo // Legacy archive ID CLI text boundary.
				if err != nil {
					return fmt.Errorf("invalid legacy review UUID: %w", err)
				}
				request.UUID = &id
			} else {
				id, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil {
					return fmt.Errorf("invalid legacy review ID: %w", err)
				}
				request.ID = &id
			}
			if err := ensureDaemon(); err != nil {
				return err
			}
			_, err = getDaemonEndpoint().APIClient(0).ImportLegacyReview(cmd.Context(), &generated.ImportLegacyReviewRequestOptions{Body: &request})
			if err != nil {
				return daemonRequestError("import legacy review", err)
			}

			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Converted review restored. The original remains archived.")
			return err
		},
	})
	for _, subcmd := range cmd.Commands() {
		subcmd.PreRunE = func(_ *cobra.Command, _ []string) error {
			if dbPath == "" {
				return nil
			}
			path, err := filepath.Abs(dbPath)
			if err != nil {
				return fmt.Errorf("resolve database path: %w", err)
			}
			dbPath = path
			return nil
		}
	}
	return cmd
}

func writeLegacyConversionReport(w io.Writer, report *generated.ConvertLegacyReviewsResponse) error {
	converted, left := "Converted and restored", "Left unstructured for export and import"
	if report.DryRun {
		converted, left = "Would convert", "Would remain unstructured"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Historical reviews needing conversion: %d\n", report.Unresolved)
	fmt.Fprintf(&out, "%s: %d\n", converted, report.Converted)
	fmt.Fprintf(&out, "%s: %d\n", left, report.Unresolved-report.Converted)
	for _, reason := range slices.Sorted(maps.Keys(report.Refused)) {
		fmt.Fprintf(&out, "  %s: %d\n", reason, report.Refused[reason])
	}
	_, err := io.WriteString(w, out.String())
	return err
}
