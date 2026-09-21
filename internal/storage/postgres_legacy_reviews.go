package storage

import (
	"context"
	"encoding/json/jsontext"
	"uuid"

	"go.kenn.io/roborev/internal/structuredreview"
)

func (p *PgPool) migrateLegacyReviews(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS legacy_reviews (
 uuid UUID PRIMARY KEY, record JSONB NOT NULL, migration_error TEXT NOT NULL, resolved_at TIMESTAMPTZ)`); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT r.uuid, r.output, r.structured_output, j.job_type, j.uuid,
 COALESCE(j.min_severity, ''), r.verdict_bool
 FROM reviews r JOIN review_jobs j ON j.uuid = r.job_uuid
 WHERE j.job_type IN ('review','range','dirty','synthesis','compact') FOR UPDATE OF r`)
	if err != nil {
		return err
	}
	type update struct {
		id     uuid.UUID
		raw    jsontext.Value
		reason string
		// markdown is set when the row has no usable JSON. Conversion waits
		// until the row cursor is closed because it may query panel members.
		markdown *legacyMarkdown
		jobUUID  uuid.UUID
		verdict  *bool
	}
	var updates []update
	for rows.Next() {
		var u update
		var output, jobType, threshold string
		var storedVerdict *bool
		if err := rows.Scan(&u.id, &output, &u.raw, &jobType, &u.jobUUID, &threshold, &storedVerdict); err != nil {
			rows.Close()
			return err
		}
		if len(u.raw) == 0 {
			u.raw = jsontext.Value(output)
		}
		doc, decodeErr := structuredreview.Decode(u.raw)
		if decodeErr == nil && jobType == JobTypeSynthesis {
			decodeErr = doc.RequireSources(len(doc.SourceLabels))
		}
		if decodeErr != nil {
			u.reason = "No valid review JSON document; AI conversion required: " + decodeErr.Error()
			u.markdown = &legacyMarkdown{Markdown: output, JobType: jobType, MinSeverity: threshold, StoredVerdict: storedVerdict}
		} else if output == "" {
			continue
		}
		updates = append(updates, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range updates {
		u := &updates[i]
		if u.markdown == nil {
			continue
		}
		if u.markdown.JobType == JobTypeSynthesis {
			sources, err := postgresLegacySources(ctx, tx, u.jobUUID)
			if err != nil {
				return err
			}
			u.markdown.SourceLabels = legacySourceLabels(sources)
		}
		raw, refusal := convertLegacyMarkdown(*u.markdown)
		if refusal != nil {
			u.reason += "; automatic conversion refused (" + refusal.Reason + "): " + refusal.Detail
			continue
		}
		u.raw, u.reason = raw, ""
		if doc, err := structuredreview.Decode(raw); err == nil && !doc.UnableToReview() {
			u.verdict = new(doc.Passed(u.markdown.MinSeverity))
		}
	}
	for _, u := range updates {
		if _, err := tx.Exec(ctx, `INSERT INTO legacy_reviews (uuid, record, migration_error, resolved_at)
   SELECT r.uuid, to_jsonb(r), $2, CASE WHEN $2 = '' THEN clock_timestamp() ELSE NULL END
   FROM reviews r WHERE r.uuid = $1 ON CONFLICT(uuid) DO NOTHING`, u.id, u.reason); err != nil {
			return err
		}
		if u.reason != "" {
			if _, err := tx.Exec(ctx, `DELETE FROM reviews WHERE uuid = $1`, u.id); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `UPDATE reviews SET output = '', structured_output = $2, updated_at = clock_timestamp(),
    verdict_bool = CASE WHEN $3 THEN $4::boolean ELSE verdict_bool END
    WHERE uuid = $1`, u.id, []byte(u.raw), u.markdown != nil, u.verdict); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
