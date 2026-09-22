package storage

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"

	"go.kenn.io/roborev/pkg/structuredreview"
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

	// Stored facts an automatic conversion must agree with.
	minSeverity   string
	storedVerdict *bool
}

func (p *PgPool) UnresolvedLegacyReviews(ctx context.Context) ([]PostgresLegacyReview, error) {
	return p.unresolvedLegacyReviews(ctx, nil, 0, false)
}

func (p *PgPool) unresolvedLegacyReviews(ctx context.Context, after *uuid.UUID, limit int, missingOnly bool) ([]PostgresLegacyReview, error) {
	rows, err := p.pool.Query(ctx, `SELECT l.uuid, j.uuid, j.job_type,
 COALESCE(l.record->>'output', ''), COALESCE(l.record->>'structured_output', ''), l.migration_error,
 COALESCE(j.min_severity, ''), (l.record->>'verdict_bool')::boolean
 FROM legacy_reviews l JOIN review_jobs j ON j.uuid = (l.record->>'job_uuid')::uuid
 LEFT JOIN reviews r ON r.uuid = l.uuid
 WHERE (l.resolved_at IS NULL OR r.structured_output->'legacy' IS NOT NULL)
 AND ($1::uuid IS NULL OR l.uuid > $1)
 AND (NOT $2 OR NOT EXISTS (SELECT 1 FROM reviews active WHERE active.uuid = l.uuid OR active.job_uuid = j.uuid))
 ORDER BY l.uuid LIMIT NULLIF($3, 0)`, after, missingOnly, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []PostgresLegacyReview{}
	for rows.Next() {
		var r PostgresLegacyReview
		if err := rows.Scan(&r.ID, &r.JobUUID, &r.JobType, &r.Output, &r.StructuredOutput, &r.Reason,
			&r.minSeverity, &r.storedVerdict); err != nil {
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

func (p *PgPool) ResolveLegacyReview(ctx context.Context, id uuid.UUID, raw jsontext.Value) error {
	doc, err := structuredreview.Decode(raw)
	if err != nil {
		return err
	}
	if doc.Legacy != nil {
		return fmt.Errorf("import requires a structured review document")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var record jsontext.Value
	var jobUUID uuid.UUID
	var jobType, threshold string
	err = tx.QueryRow(ctx, `SELECT l.record, j.uuid, j.job_type, COALESCE(j.min_severity, '')
 FROM legacy_reviews l JOIN review_jobs j ON j.uuid = (l.record->>'job_uuid')::uuid
 WHERE l.uuid = $1 AND (l.resolved_at IS NULL OR EXISTS(SELECT 1 FROM reviews r WHERE r.uuid = l.uuid)) FOR UPDATE OF l`, id).Scan(&record, &jobUUID, &jobType, &threshold)
	if err != nil {
		return err
	}
	var current jsontext.Value
	var activeID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT uuid, structured_output FROM reviews WHERE uuid = $1 OR job_uuid = $2 FOR UPDATE`, id, jobUUID).Scan(&activeID, &current)
	active := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if active {
		previous, err := structuredreview.Decode(current)
		if err != nil {
			return err
		}
		if activeID != id || previous.Legacy == nil {
			return ErrReviewNotLegacy
		}
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
	if active {
		_, err = tx.Exec(ctx, `UPDATE reviews SET structured_output = $2, output = '', verdict_bool = $3, updated_at = clock_timestamp(), updated_by_machine_id = $4 WHERE uuid = $1`, id, raw, verdict, uuid.New())
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, closed, verdict_bool,
 structured_output, reviewed_file_count, excluded_file_count, updated_by_machine_id, created_at, updated_at)
 SELECT r.uuid, r.job_uuid, r.agent, r.prompt, '', r.closed, $3, $2,
 r.reviewed_file_count, r.excluded_file_count, $4, r.created_at, clock_timestamp()
 FROM jsonb_populate_record(NULL::reviews, $1::jsonb) r`, record, raw, verdict, uuid.New())
	}
	if err != nil {
		return fmt.Errorf("restore converted review: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE legacy_reviews SET resolved_at = clock_timestamp() WHERE uuid = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ConvertLegacyReviews is the PostgreSQL mirror's form of
// DB.ConvertLegacyReviews. It imports each conversion through
// ResolveLegacyReview.
func (p *PgPool) ConvertLegacyReviews(ctx context.Context, dryRun bool) (LegacyConversionReport, error) {
	report := newLegacyConversionReport(dryRun)
	records, err := p.UnresolvedLegacyReviews(ctx)
	if err != nil {
		return report, err
	}
	report.Unresolved = len(records)
	for _, record := range records {
		var active bool
		if err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reviews WHERE (uuid = $1 OR job_uuid = $2) AND (uuid != $1 OR structured_output->'legacy' IS NULL))`,
			record.ID, record.JobUUID).Scan(&active); err != nil {
			return report, err
		}
		if active {
			report.Refused[LegacyRefusalActiveReviewExists]++
			continue
		}
		raw, refusal := convertLegacyRecord(legacyMarkdown{
			Markdown: record.Output, JobType: record.JobType, MinSeverity: record.minSeverity,
			StoredVerdict: record.storedVerdict, SourceLabels: legacySourceLabels(record.Sources),
		}, record.StructuredOutput)
		if refusal != nil {
			report.Refused[refusal.Reason]++
			continue
		}
		if !dryRun {
			if err := p.ResolveLegacyReview(ctx, record.ID, raw); err != nil {
				return report, err
			}
		}
		report.Converted++
	}
	return report, nil
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
		if source, ok := legacySource(agent, reviewType, jsontext.Value(raw), markdown); ok {
			source.Number = len(sources) + 1
			sources = append(sources, source)
		}
	}
	return sources, rows.Err()
}
