package storage

import (
	"context"
	"encoding/json"
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
	rows, err := tx.Query(ctx, `SELECT r.uuid, r.output, r.structured_output, j.job_type
 FROM reviews r JOIN review_jobs j ON j.uuid = r.job_uuid
 WHERE j.job_type IN ('review','range','dirty','synthesis','compact') FOR UPDATE OF r`)
	if err != nil {
		return err
	}
	type update struct {
		id     uuid.UUID
		raw    json.RawMessage
		reason string
	}
	var updates []update
	for rows.Next() {
		var u update
		var output, jobType string
		if err := rows.Scan(&u.id, &output, &u.raw, &jobType); err != nil {
			rows.Close()
			return err
		}
		if len(u.raw) == 0 {
			u.raw = json.RawMessage(output)
		}
		doc, decodeErr := structuredreview.Decode(u.raw)
		if decodeErr == nil && jobType == JobTypeSynthesis {
			decodeErr = doc.RequireSources(len(doc.SourceLabels))
		}
		if decodeErr != nil {
			u.reason = "No valid review JSON document; AI conversion required: " + decodeErr.Error()
		} else if output == "" {
			continue
		}
		updates = append(updates, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
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
			if _, err := tx.Exec(ctx, `UPDATE reviews SET output = '', structured_output = $2, updated_at = clock_timestamp()
    WHERE uuid = $1`, u.id, []byte(u.raw)); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
