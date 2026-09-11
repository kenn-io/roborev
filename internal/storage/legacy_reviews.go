package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"uuid"

	"go.kenn.io/roborev/internal/structuredreview"
)

// LegacyReviewMigrationNotice is shown when archived reviews need conversion.
const LegacyReviewMigrationNotice = "Some reviews could not be converted to JSON. Their original records and migration errors are preserved in legacy_reviews and are excluded from normal review reads. Ask an AI agent to convert the unresolved records using the review JSON schema, then import the validated results with roborev legacy-reviews --db <database> import <id>. Run roborev legacy-reviews --db <database> export to prepare the migration input."

var ErrLegacyReviewMigration = errors.New("review requires legacy JSON migration")

func requiresReviewDocument(jobType string) bool {
	switch jobType {
	case "", JobTypeReview, JobTypeRange, JobTypeDirty, JobTypeSynthesis, JobTypeCompact:
		return true
	default:
		return false
	}
}

const legacyReviewColumns = `id, job_id, agent, prompt, output, created_at, closed,
 reviewed_file_count, excluded_file_count, verdict_bool, structured_output,
 uuid, updated_by_machine_id, updated_at, synced_at`

// migrateLegacyReviews retires prose records without inventing findings. The
// original row is retained even when its existing JSON makes conversion exact.
func (db *DB) migrateLegacyReviews() error {
	machineID, err := db.GetMachineID()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS legacy_reviews (
 archive_id INTEGER PRIMARY KEY,
 id INTEGER, job_id INTEGER NOT NULL, agent TEXT NOT NULL, prompt TEXT NOT NULL,
 output TEXT NOT NULL, created_at TEXT NOT NULL, closed INTEGER NOT NULL,
 reviewed_file_count INTEGER, excluded_file_count INTEGER, verdict_bool INTEGER,
 structured_output TEXT, uuid TEXT UNIQUE, updated_by_machine_id TEXT,
 updated_at TEXT, synced_at TEXT, migration_error TEXT NOT NULL, resolved_at TEXT
 )`); err != nil {
		return err
	}

	rows, err := tx.Query(`SELECT rv.id, rv.output, rv.structured_output, COALESCE(j.min_severity, ''), j.job_type, rv.verdict_bool
 FROM reviews rv JOIN review_jobs j ON j.id = rv.job_id
 WHERE j.job_type IN ('review','range','dirty','synthesis','compact')`)
	if err != nil {
		return err
	}
	type pending struct {
		id      int64
		raw     json.RawMessage
		verdict any
		reason  string
	}
	var updates []pending
	for rows.Next() {
		var id int64
		var output, threshold, jobType string
		var raw sql.NullString
		var oldVerdict sql.NullInt64
		if err := rows.Scan(&id, &output, &raw, &threshold, &jobType, &oldVerdict); err != nil {
			rows.Close()
			return err
		}
		candidate := json.RawMessage(raw.String)
		if len(candidate) == 0 {
			candidate = json.RawMessage(output)
		}
		doc, decodeErr := structuredreview.Decode(candidate)
		if decodeErr == nil && jobType == JobTypeSynthesis {
			decodeErr = doc.RequireSources(len(doc.SourceLabels))
		}
		if decodeErr == nil && output == "" {
			continue
		}
		item := pending{id: id, raw: candidate}
		if decodeErr != nil {
			item.reason = "No valid review JSON document; AI conversion required: " + decodeErr.Error()
		} else if doc.UnableToReview() && jobType == JobTypeSynthesis && oldVerdict.Valid {
			// All-failed panel syntheses retain their blocking outcome.
			item.verdict = oldVerdict.Int64
		} else if !doc.UnableToReview() {
			item.verdict = verdictToBool(VerdictFromPassed(doc.Passed(threshold)))
		}
		updates = append(updates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range updates {
		if _, err := tx.Exec(`INSERT INTO legacy_reviews (`+legacyReviewColumns+`, migration_error, resolved_at)
   SELECT `+legacyReviewColumns+`, ?, CASE WHEN ? = '' THEN datetime('now') ELSE NULL END
   FROM reviews WHERE id = ? ON CONFLICT(uuid) DO NOTHING`, item.reason, item.reason, item.id); err != nil {
			return err
		}
		if item.reason != "" {
			if _, err := tx.Exec(`DELETE FROM reviews WHERE id = ?`, item.id); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(`UPDATE reviews SET output = '', structured_output = ?, verdict_bool = ?, synced_at = NULL,
    updated_at = datetime('now'), updated_by_machine_id = ? WHERE id = ?`, string(item.raw), item.verdict, machineID, item.id); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM legacy_reviews WHERE resolved_at IS NULL`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		log.Print(LegacyReviewMigrationNotice)
	}
	return nil
}

// ResolveLegacyReview imports an explicitly converted document. It preserves
// the archived record for audit and never accepts a Markdown replacement.
func (db *DB) ResolveLegacyReview(id int64, raw json.RawMessage) error {
	machineID, err := db.GetMachineID()
	if err != nil {
		return err
	}
	doc, err := structuredreview.Decode(raw)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var threshold, jobType string
	var panelRun sql.Null[uuid.UUID]
	var jobID int64
	if err := tx.QueryRow(`SELECT l.job_id, COALESCE(j.min_severity, ''), j.job_type, j.panel_run_uuid
 FROM legacy_reviews l JOIN review_jobs j ON j.id = l.job_id
 WHERE l.archive_id = ? AND l.resolved_at IS NULL`, id).Scan(&jobID, &threshold, &jobType, &panelRun); err != nil {
		return err
	}
	if jobType == JobTypeSynthesis {
		rows, err := tx.Query(`SELECT agent FROM review_jobs WHERE panel_run_uuid = ? AND panel_role = 'member' ORDER BY panel_member_index, id`, panelRun.V)
		if err != nil {
			return err
		}
		doc.SourceLabels = nil
		for rows.Next() {
			var label string
			if err := rows.Scan(&label); err != nil {
				rows.Close()
				return err
			}
			doc.SourceLabels = append(doc.SourceLabels, label)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if err := doc.RequireSources(len(doc.SourceLabels)); err != nil {
			return fmt.Errorf("synthesis conversion: %w", err)
		}
		raw, err = json.Marshal(doc)
		if err != nil {
			return err
		}
	}

	var verdict any
	if !doc.UnableToReview() {
		verdict = verdictToBool(VerdictFromPassed(doc.Passed(threshold)))
	}
	_, err = tx.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, created_at, closed,
 reviewed_file_count, excluded_file_count, verdict_bool, structured_output,
 uuid, updated_by_machine_id, updated_at, synced_at)
 SELECT job_id, agent, prompt, '', created_at, closed, reviewed_file_count, excluded_file_count,
 ?, ?, uuid, ?, datetime('now'), NULL FROM legacy_reviews WHERE archive_id = ?`, verdict, string(raw), machineID, id)
	if err != nil {
		return fmt.Errorf("restore converted review: %w", err)
	}
	if _, err := tx.Exec(`UPDATE legacy_reviews SET resolved_at = datetime('now') WHERE archive_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// LegacyReview is an unresolved conversion input, available only through the
// explicit migration command rather than ordinary review APIs.
type LegacyReview struct {
	ID               int64                `json:"id"`
	JobID            int64                `json:"job_id"`
	JobType          string               `json:"job_type"`
	Output           string               `json:"markdown"`
	StructuredOutput string               `json:"previous_json"`
	Reason           string               `json:"migration_error"`
	Sources          []LegacyReviewSource `json:"sources,omitempty"`
}

func (db *DB) UnresolvedLegacyReviews() ([]LegacyReview, error) {
	rows, err := db.Query(`SELECT l.archive_id, l.job_id, COALESCE(j.job_type, ''), l.output,
 COALESCE(l.structured_output, ''), l.migration_error FROM legacy_reviews l
 LEFT JOIN review_jobs j ON j.id = l.job_id WHERE l.resolved_at IS NULL ORDER BY l.archive_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []LegacyReview{}
	for rows.Next() {
		var r LegacyReview
		if err := rows.Scan(&r.ID, &r.JobID, &r.JobType, &r.Output, &r.StructuredOutput, &r.Reason); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range result {
		if result[i].JobType != JobTypeSynthesis {
			continue
		}
		sources, err := db.Query(`SELECT j.agent, r.structured_output, COALESCE(l.output, '') FROM review_jobs j
 LEFT JOIN reviews r ON r.job_id = j.id LEFT JOIN legacy_reviews l ON l.job_id = j.id
 WHERE j.panel_run_uuid = (SELECT panel_run_uuid FROM review_jobs WHERE id = ?) AND j.panel_role = 'member'
 ORDER BY j.panel_member_index, j.id`, result[i].JobID)
		if err != nil {
			return nil, err
		}
		for sources.Next() {
			var source LegacyReviewSource
			var raw sql.NullString
			if err := sources.Scan(&source.Agent, &raw, &source.Markdown); err != nil {
				sources.Close()
				return nil, err
			}
			source.Number = len(result[i].Sources) + 1
			if raw.Valid {
				source.Document = json.RawMessage(raw.String)
			}
			result[i].Sources = append(result[i].Sources, source)
		}
		sources.Close()
		if err := sources.Err(); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// LegacyReviewSource fixes the review-number mapping used by a conversion.
type LegacyReviewSource struct {
	Number   int             `json:"number"`
	Agent    string          `json:"agent"`
	Document json.RawMessage `json:"document,omitempty"`
	Markdown string          `json:"markdown,omitempty"`
}
