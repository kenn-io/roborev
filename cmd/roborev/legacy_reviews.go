package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"strconv"
	"strings"
	"uuid"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/structuredreview"
)

func legacyReviewsCmd() *cobra.Command {
	var dbPath, postgresURL string
	cmd := &cobra.Command{Use: "legacy-reviews", Short: "Convert, export, and resolve archived Markdown reviews"}
	cmd.PersistentFlags().StringVar(&dbPath, "db", "", "Path to an offline reviews database (stop its daemon before convert or import; convert --dry-run and export only read)")
	cmd.PersistentFlags().StringVar(&postgresURL, "postgres-url", "", "PostgreSQL connection URL for archived mirror reviews")
	cmd.MarkFlagsMutuallyExclusive("db", "postgres-url")
	cmd.MarkFlagsOneRequired("db", "postgres-url")
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
review stays archived when a finding lacks a severity, problem, or fix, when
text falls outside the recognized structure, when the sources of a combined
panel review cannot be recovered, or when the converted findings would change
the recorded verdict. Use export and import for those.

Each conversion goes through the same validation as import, and the original
stays archived. Running convert again is safe: it only sees reviews that are
still unresolved.

--dry-run changes nothing and only opens the database for reading. It reports
how many reviews would convert and counts the rest by refusal reason. Without
--dry-run, stop the daemon that uses the --db database first.`),
		RunE: func(cmd *cobra.Command, args []string) error {
			var report storage.LegacyConversionReport
			if postgresURL != "" {
				pool, err := storage.NewPgPool(cmd.Context(), postgresURL, storage.DefaultPgPoolConfig())
				if err != nil {
					return err
				}
				defer pool.Close()
				report, err = pool.ConvertLegacyReviews(cmd.Context(), dryRun)
				if err != nil {
					return err
				}
			} else {
				open := storage.Open
				if dryRun {
					open = storage.OpenReadOnly
				}
				db, err := open(dbPath)
				if err != nil {
					return err
				}
				defer db.Close()
				report, err = db.ConvertLegacyReviews(dryRun)
				if err != nil {
					return err
				}
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
			var records any
			if postgresURL != "" {
				pool, err := storage.NewPgPool(cmd.Context(), postgresURL, storage.DefaultPgPoolConfig())
				if err != nil {
					return err
				}
				defer pool.Close()
				records, err = pool.UnresolvedLegacyReviews(cmd.Context())
				if err != nil {
					return err
				}
			} else {
				db, err := storage.OpenReadOnly(dbPath)
				if err != nil {
					return err
				}
				defer db.Close()
				records, err = db.UnresolvedLegacyReviews()
				if err != nil {
					return err
				}
			}
			return json.MarshalWrite(cmd.OutOrStdout(), struct {
				Instructions    string         `json:"instructions"`
				Schema          jsontext.Value `json:"schema"`
				SynthesisSchema jsontext.Value `json:"synthesis_schema"`
				Records         any            `json:"records"`
			}{
				"Convert each archived review into the supplied JSON model. Preserve every finding and its severity, problem, fix, location, and synthesis source numbers when present. Do not invent missing information or re-review the code. Leave ambiguous records unresolved and report why. Return one JSON file per resolved record for import with roborev legacy-reviews and the same --db or --postgres-url option, followed by import <id> < converted.json.",
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
			if postgresURL != "" {
				id, err := uuid.Parse(args[0]) //nolint:forbidigo // Legacy archive ID CLI text boundary.
				if err != nil {
					return fmt.Errorf("invalid legacy review UUID: %w", err)
				}
				pool, err := storage.NewPgPool(cmd.Context(), postgresURL, storage.DefaultPgPoolConfig())
				if err != nil {
					return err
				}
				defer pool.Close()
				if err := pool.ResolveLegacyReview(cmd.Context(), id, raw); err != nil {
					return err
				}
			} else {
				id, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil {
					return fmt.Errorf("invalid legacy review ID: %w", err)
				}
				db, err := storage.Open(dbPath)
				if err != nil {
					return err
				}
				defer db.Close()
				if err := db.ResolveLegacyReview(id, raw); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Converted review restored. The original remains archived.")
			return err
		},
	})
	return cmd
}

func writeLegacyConversionReport(w io.Writer, report storage.LegacyConversionReport) error {
	converted, left := "Converted and restored", "Left archived for export and import"
	if report.DryRun {
		converted, left = "Would convert", "Would stay archived"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Archived reviews needing conversion: %d\n", report.Unresolved)
	fmt.Fprintf(&out, "%s: %d\n", converted, report.Converted)
	fmt.Fprintf(&out, "%s: %d\n", left, report.Unresolved-report.Converted)
	for _, reason := range report.RefusalReasons() {
		fmt.Fprintf(&out, "  %s: %d\n", reason, report.Refused[reason])
	}
	_, err := io.WriteString(w, out.String())
	return err
}
