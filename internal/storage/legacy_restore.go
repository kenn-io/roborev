package storage

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"uuid"

	"go.kenn.io/roborev/pkg/structuredreview"
)

// ErrReviewNotLegacy prevents conversion from replacing an existing structured review.
var ErrReviewNotLegacy = errors.New("review already has a structured document")

// ErrInvalidReviewDocument identifies rejected conversion input.
var ErrInvalidReviewDocument = errors.New("invalid review document")

// restoreLegacyReviews is a forward migration after the original archive migration.
// Unresolved archive records remain optional conversion inputs. Repeated opens
// never replace active reviews.
func (db *DB) restoreLegacyReviews() error {
	records, err := db.UnresolvedLegacyReviews()
	if err != nil {
		return err
	}
	restored, unstructured := 0, 0
	for _, record := range records {
		if record.JobType == "" {
			continue
		}
		var active bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM reviews WHERE job_id = ? OR uuid = ?)`, record.JobID, record.uuid).Scan(&active); err != nil {
			return err
		}
		if active {
			continue
		}
		raw, refusal := convertLegacyRecord(legacyMarkdown{Markdown: record.Output, JobType: record.JobType, MinSeverity: record.minSeverity, StoredVerdict: record.storedVerdict, SourceLabels: legacySourceLabels(record.Sources)}, record.StructuredOutput)
		if refusal == nil {
			if err := db.ResolveLegacyReview(record.ID, raw); err != nil {
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
			machineID, err := db.GetMachineID()
			if err != nil {
				return err
			}
			// Review IDs may have been reused after archival. Keep them when
			// available; the archived numeric job ID stays unchanged.
			_, err = db.Exec(`INSERT INTO reviews (id, job_id, agent, prompt, output, created_at, closed,
 reviewed_file_count, excluded_file_count, verdict_bool, structured_output, uuid, updated_by_machine_id, updated_at)
 SELECT CASE WHEN EXISTS(SELECT 1 FROM reviews WHERE id = l.id) THEN NULL ELSE l.id END,
 job_id, agent, prompt, '', created_at, closed, reviewed_file_count, excluded_file_count,
 verdict_bool, ?, uuid, ?, datetime('now') FROM legacy_reviews l WHERE archive_id = ?
 AND NOT EXISTS(SELECT 1 FROM reviews WHERE job_id = l.job_id OR uuid = l.uuid)`, string(raw), machineID, record.ID)
			if err != nil {
				return err
			}
			unstructured++
		}
		restored++
	}
	if restored > 0 {
		log.Printf("Restored %d historical reviews (%d unstructured). Use legacy-reviews export/import for optional agent-driven conversion.", restored, unstructured)
	}
	return nil
}

// MigrateReview accepts agent-provided findings only for an existing legacy review.
func (db *DB) MigrateReview(id int64, raw jsontext.Value) error {
	machineID, err := db.GetMachineID()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := migrateReview(tx, id, raw, machineID); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateReview(tx *sql.Tx, id int64, raw jsontext.Value, machineID uuid.UUID) error {
	var current, threshold, jobType string
	var jobID int64
	if err := tx.QueryRow(`SELECT r.structured_output, r.job_id, j.job_type, COALESCE(j.min_severity, '')
 FROM reviews r JOIN review_jobs j ON j.id = r.job_id WHERE r.id = ?`, id).Scan(&current, &jobID, &jobType, &threshold); err != nil {
		return err
	}
	previous, err := structuredreview.Decode(jsontext.Value(current))
	if err != nil {
		return err
	}
	if previous.Legacy == nil {
		return ErrReviewNotLegacy
	}
	doc, err := structuredreview.Decode(raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidReviewDocument, err)
	}
	if doc.Legacy != nil {
		return fmt.Errorf("%w: import requires a structured review document", ErrInvalidReviewDocument)
	}
	if jobType == JobTypeSynthesis {
		sources, err := legacySynthesisSources(tx, jobID)
		if err != nil {
			return err
		}
		doc.SourceLabels = legacySourceLabels(sources)
		if err := doc.RequireSources(len(doc.SourceLabels)); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidReviewDocument, err)
		}
	}
	raw, err = json.Marshal(doc)
	if err != nil {
		return err
	}
	var verdict any
	if !doc.UnableToReview() {
		verdict = verdictToBool(VerdictFromPassed(doc.Passed(threshold)))
	}
	if _, err := tx.Exec(`UPDATE reviews SET structured_output = ?, output = '', verdict_bool = ?, updated_at = datetime('now'), updated_by_machine_id = ?, synced_at = NULL WHERE id = ?`, string(raw), verdict, machineID, id); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE legacy_reviews SET resolved_at = datetime('now') WHERE uuid = (SELECT uuid FROM reviews WHERE id = ?)`, id)
	return err
}
