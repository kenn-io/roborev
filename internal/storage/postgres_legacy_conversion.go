package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"

	"go.kenn.io/roborev/internal/structuredreview"
)

// PostgresLegacyReview is an archived conversion input from the shared mirror.
type PostgresLegacyReview struct {
	ID               uuid.UUID            `json:"id"`
	JobUUID          uuid.UUID            `json:"job_uuid"`
	JobType          string               `json:"job_type"`
	Output           string               `json:"markdown"`
	StructuredOutput string               `json:"previous_json"`
	Reason           string               `json:"migration_error"`
	Sources          []LegacyReviewSource `json:"sources,omitempty"`
}

func (p *PgPool) UnresolvedLegacyReviews(ctx context.Context) ([]PostgresLegacyReview, error) {
	rows, err := p.pool.Query(ctx, `SELECT l.uuid, j.uuid, j.job_type,
 COALESCE(l.record->>'output', ''), COALESCE(l.record->>'structured_output', ''), l.migration_error
 FROM legacy_reviews l JOIN review_jobs j ON j.uuid = (l.record->>'job_uuid')::uuid
 WHERE l.resolved_at IS NULL ORDER BY l.uuid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []PostgresLegacyReview{}
	for rows.Next() {
		var r PostgresLegacyReview
		if err := rows.Scan(&r.ID, &r.JobUUID, &r.JobType, &r.Output, &r.StructuredOutput, &r.Reason); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range records {
		if records[i].JobType == JobTypeSynthesis {
			records[i].Sources, err = postgresLegacySources(ctx, p.pool, records[i].JobUUID)
			if err != nil {
				return nil, err
			}
		}
	}
	return records, nil
}

func (p *PgPool) ResolveLegacyReview(ctx context.Context, id uuid.UUID, raw json.RawMessage) error {
	doc, err := structuredreview.Decode(raw)
	if err != nil {
		return err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var record json.RawMessage
	var jobUUID uuid.UUID
	var jobType, threshold string
	err = tx.QueryRow(ctx, `SELECT l.record, j.uuid, j.job_type, COALESCE(j.min_severity, '')
 FROM legacy_reviews l JOIN review_jobs j ON j.uuid = (l.record->>'job_uuid')::uuid
 WHERE l.uuid = $1 AND l.resolved_at IS NULL FOR UPDATE OF l`, id).Scan(&record, &jobUUID, &jobType, &threshold)
	if err != nil {
		return err
	}
	if jobType == JobTypeSynthesis {
		sources, err := postgresLegacySources(ctx, tx, jobUUID)
		if err != nil {
			return err
		}
		doc.SourceLabels = nil
		for _, source := range sources {
			doc.SourceLabels = append(doc.SourceLabels, source.Agent)
		}
		if err := doc.RequireSources(len(doc.SourceLabels)); err != nil {
			return fmt.Errorf("synthesis conversion: %w", err)
		}
		raw, err = json.Marshal(doc)
		if err != nil {
			return err
		}
	}
	var verdict *bool
	if !doc.UnableToReview() {
		verdict = new(doc.Passed(threshold))
	}
	_, err = tx.Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, closed, verdict_bool,
 structured_output, reviewed_file_count, excluded_file_count, updated_by_machine_id, created_at, updated_at)
 SELECT r.uuid, r.job_uuid, r.agent, r.prompt, '', r.closed, $3, $2,
 r.reviewed_file_count, r.excluded_file_count, $4, r.created_at, clock_timestamp()
 FROM jsonb_populate_record(NULL::reviews, $1::jsonb) r`, record, raw, verdict, uuid.New())
	if err != nil {
		return fmt.Errorf("restore converted review: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE legacy_reviews SET resolved_at = clock_timestamp() WHERE uuid = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type pgLegacyQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func postgresLegacySources(ctx context.Context, q pgLegacyQuerier, jobUUID uuid.UUID) ([]LegacyReviewSource, error) {
	rows, err := q.Query(ctx, `SELECT j.agent, j.review_type,
 COALESCE(r.structured_output::text, l.record->>'structured_output', ''),
 COALESCE(NULLIF(r.output, ''), l.record->>'output', '')
 FROM review_jobs j LEFT JOIN reviews r ON r.job_uuid = j.uuid
 LEFT JOIN legacy_reviews l ON (l.record->>'job_uuid')::uuid = j.uuid AND l.resolved_at IS NULL AND r.job_uuid IS NULL
 WHERE j.panel_run_uuid = (SELECT panel_run_uuid FROM review_jobs WHERE uuid = $1)
 AND j.panel_role = 'member' AND j.status = 'done' ORDER BY j.panel_member_index, j.uuid`, jobUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := []LegacyReviewSource{}
	for rows.Next() {
		var agent, reviewType, raw, markdown string
		if err := rows.Scan(&agent, &reviewType, &raw, &markdown); err != nil {
			return nil, err
		}
		if source, ok := legacySource(agent, reviewType, json.RawMessage(raw), markdown); ok {
			source.Number = len(sources) + 1
			sources = append(sources, source)
		}
	}
	return sources, rows.Err()
}
