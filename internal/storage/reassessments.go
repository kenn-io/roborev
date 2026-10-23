package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
	"uuid"

	"go.kenn.io/roborev/internal/findingid"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/katapublish"
	rebuttalresult "go.kenn.io/roborev/internal/rebuttal/result"
)

// ReassessmentContext is captured with the queued job. Its snapshot and
// destination survive a parent rerun and never follow later hook/binding edits.
type ReassessmentContext struct {
	OriginalReviewUUID   uuid.UUID
	OriginalDocumentHash string
	Finding              FindingSnapshot
	Selector             string
	Excerpt              string
	Requester            string
	Explanation          string
	Replacement          string
	Policy               string
	Delivery             *katapublish.Request
}

type Reassessment struct {
	Context    ReassessmentContext
	Assessment *rebuttalresult.Assessment
}

// ReassessmentReviewHash identifies the review content selected by a request.
// Structured reviews use the same canonical hash as their finding selectors;
// historical prose reviews retain the exact text used for excerpt selection.
func ReassessmentReviewHash(review *Review) (string, error) {
	var raw []byte
	var err error
	if review.StructuredOutput != nil {
		raw, err = json.Marshal(review.StructuredOutput)
		if err != nil {
			return "", err
		}
	}
	return reassessmentContentHash(raw, review.Output)
}

func reassessmentContentHash(structured []byte, output string) (string, error) {
	if len(structured) != 0 {
		return findingid.DocumentHash(structured)
	}
	digest := sha256.Sum256([]byte(output))
	return hex.EncodeToString(digest[:]), nil
}

func migrateReassessments(ctx context.Context, db *DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS reassessments(
 job_id INTEGER PRIMARY KEY REFERENCES review_jobs(id) ON DELETE CASCADE,
 reassessment_uuid TEXT NOT NULL UNIQUE,
 context_json TEXT NOT NULL,
 assessment_json TEXT
 )`)
	return err
}

func (db *DB) Reassessment(ctx context.Context, jobID int64) (Reassessment, error) {
	return readReassessment(ctx, db, jobID)
}

func readReassessment(ctx context.Context, q snapshotQueries, jobID int64) (Reassessment, error) {
	var result Reassessment
	var capture string
	var assessment sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT context_json,assessment_json FROM reassessments WHERE job_id=?`, jobID).Scan(&capture, &assessment); err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(capture), &result.Context); err != nil {
		return result, err
	}
	if assessment.Valid {
		result.Assessment = new(rebuttalresult.Assessment)
		if err := json.Unmarshal([]byte(assessment.String), result.Assessment); err != nil {
			return result, err
		}
	}
	return result, nil
}

func insertReassessment(ctx context.Context, q snapshotQueries, job *ReviewJob, capture *ReassessmentContext, machine uuid.UUID, now time.Time) error {
	if capture == nil {
		return nil
	}
	if job.Source != JobSourceRebuttal || job.ParentJobID == nil || job.UUID == nil {
		return fmt.Errorf("reassessment requires a rebuttal job with a parent")
	}
	var current uuid.UUID
	var closed bool
	var structured sql.NullString
	var output string
	if err := q.QueryRowContext(ctx, `SELECT uuid,closed,structured_output,output FROM reviews WHERE job_id=?`, *job.ParentJobID).Scan(&current, &closed, &structured, &output); err != nil {
		return ErrReviewChanged
	}
	if closed || current != capture.OriginalReviewUUID {
		return ErrReviewChanged
	}
	if capture.Selector != "" && (capture.Finding.ReviewUUID != current || capture.Finding.Selector != capture.Selector || capture.Finding.JobID != *job.ParentJobID) {
		return ErrFindingChanged
	}
	hash, err := reassessmentContentHash([]byte(structured.String), output)
	if err != nil {
		return ErrFindingChanged
	}
	if (capture.OriginalDocumentHash != "" && capture.OriginalDocumentHash != hash) || (capture.Selector != "" && capture.Finding.DocumentHash != hash) {
		return ErrFindingChanged
	}
	// Copy the request so callers keep ownership of their captured value.
	frozen := *capture
	frozen.OriginalDocumentHash = hash
	raw, err := json.Marshal(&frozen)
	if err != nil {
		return err
	}
	if _, err = q.ExecContext(ctx, `INSERT INTO reassessments(job_id,reassessment_uuid,context_json) VALUES(?,?,?)`, job.ID, *job.UUID, string(raw)); err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO responses(job_id,responder,response,source,uuid,source_machine_id,created_at) VALUES(?,?,?,?,?,?,?)`, *job.ParentJobID, capture.Requester, capture.Explanation, ResponseSourceLocal, uuid.New(), machine, now.Format(time.RFC3339))
	return err
}

// completeReassessment runs inside the result transaction. The parent annotation
// is version guarded; the durable assessment and delivery still belong to the
// original snapshot when a newer review has replaced its parent.
func completeReassessment(ctx context.Context, q snapshotQueries, jobID int64, agent, output string, machine uuid.UUID, now string) error {
	result, err := readReassessment(ctx, q, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	assessment, err := rebuttalresult.Parse(output)
	if err != nil {
		return fmt.Errorf("invalid reassessment result")
	}
	if result.Assessment != nil {
		return nil
	}
	raw, err := json.Marshal(assessment)
	if err != nil {
		return err
	}
	if _, err = q.ExecContext(ctx, `UPDATE reassessments SET assessment_json=? WHERE job_id=? AND assessment_json IS NULL`, string(raw), jobID); err != nil {
		return err
	}
	var parentID int64
	var reassessmentUUID string
	if err = q.QueryRowContext(ctx, `SELECT j.parent_job_id,r.reassessment_uuid FROM review_jobs j JOIN reassessments r ON r.job_id=j.id WHERE j.id=?`, jobID).Scan(&parentID, &reassessmentUUID); err != nil {
		return err
	}
	capture := result.Context
	excerpt := capture.Excerpt
	if capture.Selector != "" {
		excerpt = fmt.Sprintf("%s\nProblem: %s\nFix: %s", capture.Selector, capture.Finding.Finding.Problem, capture.Finding.Finding.Fix)
	}
	body := fmt.Sprintf("Rebuttal job %d (%s): %s\n\nFinding:\n%s\n\n%s", jobID, agent, assessment.Outcome, excerpt, assessment.Explanation)
	var current uuid.UUID
	var structured sql.NullString
	var outputText string
	err = q.QueryRowContext(ctx, `SELECT uuid,structured_output,output FROM reviews WHERE job_id=?`, parentID).Scan(&current, &structured, &outputText)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && current == capture.OriginalReviewUUID {
		hash, hashErr := reassessmentContentHash([]byte(structured.String), outputText)
		if hashErr == nil && hash == capture.OriginalDocumentHash {
			if _, err = q.ExecContext(ctx, `INSERT INTO responses(job_id,responder,response,source,uuid,source_machine_id,created_at) VALUES(?,?,?,?,?,?,?)`, parentID, agent, body, ResponseSourceLocal, uuid.New(), machine, now); err != nil {
				return err
			}
		}
	}
	if capture.Delivery != nil {
		if len(capture.Delivery.Responses) != 1 {
			return fmt.Errorf("reassessment delivery requires exactly one response")
		}
		request := *capture.Delivery
		request.OperationID = reassessmentUUID
		response := request.Responses[0]
		response.OperationID = reassessmentUUID
		switch assessment.Outcome {
		case "confirmed":
			response.Kind = "confirm"
		case "refuted":
			response.Kind = "refute"
		case "superseded":
			response.Kind = "reply"
			if capture.Replacement != "" {
				response.Kind = "supersede"
			}
		}
		response.Body = assessment.Explanation
		if capture.Replacement != "" && response.Kind == "supersede" {
			response.Body += "\n\nReplacement: " + capture.Replacement
		}
		evidenceErr := kata.ValidateEvidence(kata.CommentKind(response.Kind), assessment.Explanation)
		if request.Policy == "summary" {
			response.Body = fmt.Sprintf("Reassessment %s for finding %s. Details withheld by the captured summary policy.", assessment.Outcome, capture.Selector)
			if capture.Replacement != "" && response.Kind == "supersede" {
				response.Body += " Replacement: " + capture.Replacement
			}
			if evidenceErr != nil {
				response.Body = "Insufficient evidence"
			}
		}
		response.Fingerprint = ""
		request.Responses = []katapublish.Response{response}
		response.Fingerprint, err = katapublish.Fingerprint(request)
		if err != nil {
			return err
		}
		request.Responses[0] = response
		if err = InsertKataHandoffTx(ctx, q, jobID, request); err != nil {
			return err
		}
	}
	return nil
}
