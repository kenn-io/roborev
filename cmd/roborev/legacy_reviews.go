package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"uuid"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/structuredreview"
)

func legacyReviewsCmd() *cobra.Command {
	var dbPath, postgresURL string
	cmd := &cobra.Command{Use: "legacy-reviews", Short: "Export and resolve archived reviews that need AI conversion"}
	cmd.PersistentFlags().StringVar(&dbPath, "db", "", "Path to an offline reviews database (stop its daemon before importing)")
	cmd.PersistentFlags().StringVar(&postgresURL, "postgres-url", "", "PostgreSQL connection URL for archived mirror reviews")
	cmd.MarkFlagsMutuallyExclusive("db", "postgres-url")
	cmd.MarkFlagsOneRequired("db", "postgres-url")
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
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				Instructions    string          `json:"instructions"`
				Schema          json.RawMessage `json:"schema"`
				SynthesisSchema json.RawMessage `json:"synthesis_schema"`
				Records         any             `json:"records"`
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
