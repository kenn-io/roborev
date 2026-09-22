package storage

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"strings"
	"uuid"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// LegacyReviewMigrationNotice is shown when archived reviews need conversion.
const LegacyReviewMigrationNotice = "Some reviews could not be converted to JSON. Their original records and migration errors are preserved in legacy_reviews. Stop the daemon and run roborev legacy-reviews --db <database> convert to restore the reviews roborev wrote in a format it can read back exactly. For the rest, ask an AI agent to convert the unresolved records using the review JSON schema, then import the validated results with roborev legacy-reviews --db <database> import <id>. Run roborev legacy-reviews --db <database> export to prepare the migration input."

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

// legacyReviewBatchSize is the requested 100-review work unit for upgrades.
// It bounds retained review bodies and transaction size, not document size.
const legacyReviewBatchSize = 100

// migrateLegacyReviews retires prose records without inventing findings. A
// Markdown review that roborev wrote in a format it can read back exactly is
// converted in place. The original row is retained even when conversion is
// exact.
func (db *DB) migrateLegacyReviews() error {
	machineID, err := db.GetMachineID()
	if err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS legacy_reviews (
 archive_id INTEGER PRIMARY KEY,
 id INTEGER, job_id INTEGER NOT NULL, agent TEXT NOT NULL, prompt TEXT NOT NULL,
 output TEXT NOT NULL, created_at TEXT NOT NULL, closed INTEGER NOT NULL,
 reviewed_file_count INTEGER, excluded_file_count INTEGER, verdict_bool INTEGER,
 structured_output TEXT, uuid TEXT UNIQUE, updated_by_machine_id TEXT,
 updated_at TEXT, synced_at TEXT, migration_error TEXT NOT NULL, resolved_at TEXT
 )`); err != nil {
		return err
	}
	// Panel-source lookup joins unresolved archives by job ID. Without this
	// forward index, each lookup scans the growing archive table between batches.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_legacy_reviews_unresolved_job ON legacy_reviews(job_id) WHERE resolved_at IS NULL`); err != nil {
		return err
	}
	var after int64
	total := 0
	for {
		next, scanned, err := db.migrateLegacyReviewsBatch(after, machineID)
		if err != nil {
			return fmt.Errorf("archive legacy reviews after review %d: %w", after, err)
		}
		if scanned == 0 {
			return nil
		}
		after = next
		total += scanned
		log.Printf("Legacy review migration: scanned %d reviews (through review %d)", total, after)
	}
}

func (db *DB) migrateLegacyReviewsBatch(after int64, machineID uuid.UUID) (int64, int, error) {
	scanned := 0
	tx, err := db.Begin()
	if err != nil {
		return after, scanned, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(`SELECT rv.id, rv.job_id, rv.output, rv.structured_output, COALESCE(j.min_severity, ''), j.job_type, rv.verdict_bool
 FROM reviews rv JOIN review_jobs j ON j.id = rv.job_id
 WHERE j.job_type IN ('review','range','dirty','synthesis','compact')
 AND rv.id > ? ORDER BY rv.id LIMIT ?`, after, legacyReviewBatchSize)
	if err != nil {
		return after, scanned, err
	}
	type pending struct {
		id      int64
		raw     jsontext.Value
		verdict any
		reason  string
		// markdown is set when the row has no usable JSON. Conversion waits
		// until the row cursor is closed because it may query panel members.
		markdown *legacyMarkdown
		jobID    int64
	}
	var updates []pending
	for rows.Next() {
		var id, jobID int64
		var output, threshold, jobType string
		var raw sql.NullString
		var oldVerdict sql.NullInt64
		if err := rows.Scan(&id, &jobID, &output, &raw, &threshold, &jobType, &oldVerdict); err != nil {
			rows.Close()
			return after, scanned, err
		}
		after = id
		scanned++
		candidate := jsontext.Value(raw.String)
		if len(candidate) == 0 {
			candidate = jsontext.Value(output)
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
			item.jobID = jobID
			item.markdown = &legacyMarkdown{Markdown: output, JobType: jobType, MinSeverity: threshold}
			if oldVerdict.Valid {
				item.markdown.StoredVerdict = new(oldVerdict.Int64 != 0)
			}
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
		return after, scanned, err
	}
	if err := rows.Close(); err != nil {
		return after, scanned, err
	}
	for i := range updates {
		item := &updates[i]
		if item.markdown == nil {
			continue
		}
		if item.markdown.JobType == JobTypeSynthesis {
			sources, err := legacySynthesisSources(tx, item.jobID)
			if err != nil {
				return after, scanned, err
			}
			item.markdown.SourceLabels = legacySourceLabels(sources)
		}
		raw, refusal := convertLegacyMarkdown(*item.markdown)
		if refusal != nil {
			item.reason += "; automatic conversion refused (" + refusal.Reason + "): " + refusal.Detail
			continue
		}
		item.raw, item.reason = raw, ""
		item.verdict = nil
		if doc, err := structuredreview.Decode(raw); err == nil && !doc.UnableToReview() {
			item.verdict = verdictToBool(VerdictFromPassed(doc.Passed(item.markdown.MinSeverity)))
		}
	}
	for _, item := range updates {
		if _, err := tx.Exec(`INSERT INTO legacy_reviews (`+legacyReviewColumns+`, migration_error, resolved_at)
   SELECT `+legacyReviewColumns+`, ?, CASE WHEN ? = '' THEN datetime('now') ELSE NULL END
   FROM reviews WHERE id = ? ON CONFLICT(uuid) DO NOTHING`, item.reason, item.reason, item.id); err != nil {
			return after, scanned, err
		}
		if item.reason != "" {
			if _, err := tx.Exec(`DELETE FROM reviews WHERE id = ?`, item.id); err != nil {
				return after, scanned, err
			}
		} else {
			if _, err := tx.Exec(`UPDATE reviews SET output = '', structured_output = ?, verdict_bool = ?, synced_at = NULL,
    updated_at = datetime('now'), updated_by_machine_id = ? WHERE id = ?`, string(item.raw), item.verdict, machineID, item.id); err != nil {
				return after, scanned, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return after, scanned, err
	}

	return after, scanned, nil
}

// ResolveLegacyReview imports an explicitly converted document. It preserves
// the archived record for audit and never accepts a Markdown replacement.
func (db *DB) ResolveLegacyReview(id int64, raw jsontext.Value) error {
	machineID, err := db.GetMachineID()
	if err != nil {
		return err
	}
	doc, err := structuredreview.Decode(raw)
	if err != nil {
		return err
	}
	if doc.Legacy != nil {
		return fmt.Errorf("import requires a structured review document")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var threshold, jobType string
	var jobID int64
	if err := tx.QueryRow(`SELECT l.job_id, COALESCE(j.min_severity, ''), j.job_type
 FROM legacy_reviews l JOIN review_jobs j ON j.id = l.job_id
 WHERE l.archive_id = ? AND (l.resolved_at IS NULL OR EXISTS(SELECT 1 FROM reviews r WHERE r.uuid = l.uuid))`, id).Scan(&jobID, &threshold, &jobType); err != nil {
		return err
	}
	var activeID int64
	var same bool
	err = tx.QueryRow(`SELECT r.id, r.uuid = l.uuid FROM reviews r JOIN legacy_reviews l ON l.job_id = r.job_id WHERE l.archive_id = ?`, id).Scan(&activeID, &same)
	if err == nil {
		if !same {
			return ErrReviewNotLegacy
		}
		if err := migrateReview(tx, activeID, raw, machineID); err != nil {
			return err
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if jobType == JobTypeSynthesis {
		sources, err := legacySynthesisSources(tx, jobID)
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

	var verdict any
	if !doc.UnableToReview() {
		verdict = verdictToBool(VerdictFromPassed(doc.Passed(threshold)))
	}
	_, err = tx.Exec(`INSERT INTO reviews (id, job_id, agent, prompt, output, created_at, closed,
 reviewed_file_count, excluded_file_count, verdict_bool, structured_output,
 uuid, updated_by_machine_id, updated_at, synced_at)
 SELECT CASE WHEN EXISTS(SELECT 1 FROM reviews WHERE id = l.id) THEN NULL ELSE l.id END, job_id, agent, prompt, '', created_at, closed, reviewed_file_count, excluded_file_count,
 ?, ?, uuid, ?, datetime('now'), NULL FROM legacy_reviews l WHERE archive_id = ?`, verdict, string(raw), machineID, id)
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
	ReviewID         *int64               `json:"review_id,omitempty"`
	JobID            int64                `json:"job_id"`
	JobType          string               `json:"job_type"`
	Output           string               `json:"markdown"`
	StructuredOutput string               `json:"previous_json"`
	Reason           string               `json:"migration_error"`
	Sources          []LegacyReviewSource `json:"sources,omitempty"`

	// Stored facts an automatic conversion must agree with. The AI conversion
	// input does not need them.
	uuid          sql.Null[uuid.UUID]
	minSeverity   string
	storedVerdict *bool
}

func (db *DB) UnresolvedLegacyReviews() ([]LegacyReview, error) {
	return db.unresolvedLegacyReviews(0, -1, false)
}

// Restoration pages only missing active reviews. Explicit export still includes
// active legacy documents and returns the complete conversion input.
func (db *DB) unresolvedLegacyReviews(after int64, limit int, missingOnly bool) ([]LegacyReview, error) {
	rows, err := db.Query(`SELECT l.archive_id, l.job_id, COALESCE(j.job_type, ''), l.output,
 COALESCE(l.structured_output, ''), l.migration_error, l.uuid, COALESCE(j.min_severity, ''), l.verdict_bool, r.id FROM legacy_reviews l
 LEFT JOIN review_jobs j ON j.id = l.job_id
 LEFT JOIN reviews r ON r.uuid = l.uuid
 WHERE (l.resolved_at IS NULL OR json_extract(r.structured_output, '$.legacy') IS NOT NULL)
 AND l.archive_id > ?
 AND (NOT ? OR (j.id IS NOT NULL AND NOT EXISTS (
 SELECT 1 FROM reviews active WHERE active.job_id = l.job_id OR active.uuid = l.uuid)))
 ORDER BY l.archive_id LIMIT ?`, after, missingOnly, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []LegacyReview{}
	for rows.Next() {
		var r LegacyReview
		var verdict sql.NullInt64
		if err := rows.Scan(&r.ID, &r.JobID, &r.JobType, &r.Output, &r.StructuredOutput, &r.Reason,
			&r.uuid, &r.minSeverity, &verdict, &r.ReviewID); err != nil {
			return nil, err
		}
		if verdict.Valid {
			r.storedVerdict = new(verdict.Int64 != 0)
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
		result[i].Sources, err = legacySynthesisSources(db, result[i].JobID)
		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

// LegacyReviewSource fixes the review-number mapping used by a conversion.
type LegacyReviewSource struct {
	Number   int            `json:"number"`
	Agent    string         `json:"agent"`
	Document jsontext.Value `json:"document,omitempty"`
	Markdown string         `json:"markdown,omitempty"`
}

// legacySynthesisSources retains the successful, substantive input order used
// by synthesis. Archived prose is read only as explicit conversion input.
func legacySynthesisSources(q querier, jobID int64) ([]LegacyReviewSource, error) {
	rows, err := q.Query(`SELECT j.agent, j.review_type,
 COALESCE(r.structured_output, l.structured_output, ''), COALESCE(NULLIF(r.output, ''), l.output, '')
 FROM review_jobs j LEFT JOIN reviews r ON r.job_id = j.id
 LEFT JOIN legacy_reviews l ON l.job_id = j.id AND l.resolved_at IS NULL AND r.job_id IS NULL
 WHERE j.panel_run_uuid = (SELECT panel_run_uuid FROM review_jobs WHERE id = ?)
 AND j.panel_role = 'member' AND j.status = 'done' ORDER BY j.panel_member_index, j.id`, jobID)
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

func legacySource(agent, reviewType string, raw jsontext.Value, markdown string) (LegacyReviewSource, bool) {
	doc, err := structuredreview.Decode(raw)
	if err == nil {
		if doc.UnableToReview() || (doc.Legacy != nil && ClassifyOutput(doc.Legacy.Markdown) != OutputReviewed) {
			return LegacyReviewSource{}, false
		}
	} else {
		if ClassifyOutput(markdown) != OutputReviewed {
			return LegacyReviewSource{}, false
		}
		raw = nil
	}
	label := strings.TrimSpace(agent)
	if rt := strings.TrimSpace(reviewType); rt != "" && !config.IsDefaultReviewType(rt) {
		label += " (" + rt + ")"
	}
	return LegacyReviewSource{Agent: label, Document: raw, Markdown: markdown}, true
}
