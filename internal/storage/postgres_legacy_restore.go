package storage

import (
	"context"
	"encoding/json/v2"
	"log"
	"uuid"

	"go.kenn.io/roborev/pkg/structuredreview"
)

func (p *PgPool) restoreLegacyReviews(ctx context.Context) error {
	restored, unstructured := 0, 0
	var after *uuid.UUID
	for {
		records, err := p.unresolvedLegacyReviews(ctx, after, legacyReviewBatchSize, true)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			break
		}
		for _, record := range records {
			after = new(record.ID)
			var active bool
			if err := p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reviews WHERE uuid = $1 OR job_uuid = $2)`, record.ID, record.JobUUID).Scan(&active); err != nil {
				return err
			}
			if active {
				continue
			}
			raw, refusal := convertLegacyRecord(legacyMarkdown{Markdown: record.Output, JobType: record.JobType, MinSeverity: record.minSeverity, StoredVerdict: record.storedVerdict, SourceLabels: legacySourceLabels(record.Sources)}, record.StructuredOutput)
			if refusal == nil {
				if err := p.ResolveLegacyReview(ctx, record.ID, raw); err != nil {
					return err
				}
			} else {
				markdown := record.Output
				if markdown == "" {
					markdown = record.StructuredOutput
				}
				raw, err = json.Marshal(structuredreview.Document{SchemaVersion: structuredreview.LegacySchemaVersion, Legacy: &structuredreview.LegacyDocument{Markdown: markdown, RecordedVerdict: record.storedVerdict}})
				if err != nil {
					return err
				}
				_, err = p.pool.Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, closed, verdict_bool,
 structured_output, reviewed_file_count, excluded_file_count, updated_by_machine_id, created_at, updated_at)
 SELECT r.uuid, r.job_uuid, r.agent, r.prompt, '', r.closed, r.verdict_bool, $2,
 r.reviewed_file_count, r.excluded_file_count, r.updated_by_machine_id, r.created_at, clock_timestamp()
 FROM legacy_reviews l CROSS JOIN LATERAL jsonb_populate_record(NULL::reviews, l.record) r
 WHERE l.uuid = $1 AND NOT EXISTS(SELECT 1 FROM reviews WHERE uuid = r.uuid OR job_uuid = r.job_uuid)
 ON CONFLICT DO NOTHING`, record.ID, []byte(raw))
				if err != nil {
					return err
				}
				unstructured++
			}
			restored++
		}
		log.Printf("Historical mirror restoration: processed %d reviews", restored)
	}
	if restored > 0 {
		log.Printf("Restored %d historical mirror reviews (%d unstructured).", restored, unstructured)
	}
	return nil
}
