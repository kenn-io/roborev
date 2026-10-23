package storage

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"

	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/internal/kataresponse"
)

// ErrResponseChanged rejects reuse of an operation ID for another intent.
var ErrResponseChanged = errors.New("response operation ID was already used with a different payload")

// KataResponseStatus exposes only safe delivery diagnostics and receipt IDs.
// It never exposes stored request content or credentials.
type KataResponseStatus struct {
	JobID           int64  `json:"-"`
	OperationID     string `json:"operation_id"`
	State           string `json:"state"`
	Kind            string `json:"kind"`
	Target          string `json:"target"`
	Receipt         string `json:"receipt,omitempty"`
	ExistingComment string `json:"existing_comment,omitempty"`
	Message         string `json:"message,omitempty"`
}

func migrateKataResponses(ctx context.Context, db *DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS kata_responses(
 operation_id TEXT PRIMARY KEY REFERENCES kata_handoffs(operation_id) ON DELETE CASCADE,
 job_id INTEGER NOT NULL REFERENCES review_jobs(id) ON DELETE CASCADE,
 fingerprint TEXT NOT NULL,
 intent_json TEXT NOT NULL
 )`)
	return err
}

// KataResponseIntent reads the complete local caller evidence. It is separate
// from the policy-filtered handoff and is never part of public delivery status.
func (db *DB) KataResponseIntent(ctx context.Context, operation string) (kataresponse.Request, error) {
	var intent kataresponse.Request
	var raw string
	if err := db.QueryRowContext(ctx, `SELECT intent_json FROM kata_responses WHERE operation_id=?`, operation).Scan(&raw); err != nil {
		return intent, err
	}
	err := json.Unmarshal([]byte(raw), &intent)
	return intent, err
}

// ResumeKataResponse checks the caller's fingerprint before any destination
// lookup. Thus a lost acknowledgement can be recovered while Kata is offline.
func (db *DB) ResumeKataResponse(ctx context.Context, intent kataresponse.Request) (*KataResponseStatus, error) {
	fingerprint, err := intent.Fingerprint()
	if err != nil {
		return nil, err
	}
	var stored string
	err = db.QueryRowContext(ctx, `SELECT fingerprint FROM kata_responses WHERE operation_id=?`, intent.OperationID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if stored != fingerprint {
		return nil, ErrResponseChanged
	}
	result, err := db.KataResponseStatus(ctx, intent.OperationID)
	return &result, err
}

func (db *DB) KataResponseStatus(ctx context.Context, operation string) (KataResponseStatus, error) {
	return readKataResponseStatus(ctx, db, operation)
}

func readKataResponseStatus(ctx context.Context, q snapshotQueries, operation string) (KataResponseStatus, error) {
	h, err := scanKataHandoff(q.QueryRowContext(ctx, kataHandoffSelect+` WHERE operation_id=?`, operation))
	if err != nil {
		return KataResponseStatus{}, err
	}
	return kataResponseStatus(h)
}

func kataResponseStatus(h KataHandoff) (KataResponseStatus, error) {
	operation := h.OperationID
	result := KataResponseStatus{OperationID: operation}
	if len(h.Request.Responses) != 1 {
		return result, sql.ErrNoRows
	}
	response := h.Request.Responses[0]
	result.JobID = h.JobID
	result.State = h.Status
	result.Kind = response.Kind
	result.Target = response.Target
	result.Message = h.LastError
	result.Receipt = h.State.Receipts[operation].UID
	result.ExistingComment = h.State.Receipts["duplicate:"+operation].UID
	return result, nil
}

// KataResponseStatuses lists only safe status fields for an authorized job.
// An optional full issue UID selects that frozen destination, without consulting
// the current connection or bindings. The daemon owns source authorization.
func (db *DB) KataResponseStatuses(ctx context.Context, jobID int64, issueUID string) ([]KataResponseStatus, error) {
	if jobID <= 0 {
		return nil, fmt.Errorf("response statuses require a positive job ID")
	}
	handoffs, err := db.KataHandoffs(ctx, jobID)
	if err != nil {
		return nil, err
	}
	out := []KataResponseStatus{}
	for _, handoff := range handoffs {
		if len(handoff.Request.Responses) != 1 || issueUID != "" && handoff.Request.IssueUID != issueUID {
			continue
		}
		status, err := kataResponseStatus(handoff)
		if err != nil {
			return nil, err
		}
		out = append(out, status)
	}
	return out, nil
}

// KataResponseRoots reads completed local finding mappings for explicit comment
// reference resolution. It never resolves a current project binding. Callers
// must authorize each source job before reading its remote issue.
func (db *DB) KataResponseRoots(ctx context.Context) ([]KataHandoff, error) {
	rows, err := db.QueryContext(ctx, kataHandoffSelect+` ORDER BY operation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KataHandoff{}
	for rows.Next() {
		handoff, err := scanKataHandoff(rows)
		if err != nil {
			return nil, err
		}
		if handoff.State.Done && len(handoff.Request.Responses) == 0 && len(handoff.Request.Entries) > 0 {
			out = append(out, handoff)
		}
	}
	return out, rows.Err()
}

// EnqueueKataResponse commits the caller's fingerprint with K2's sole durable
// delivery queue. Concurrent identical requests converge to one operation.
func (db *DB) EnqueueKataResponse(ctx context.Context, intent kataresponse.Request, request katapublish.Request) (KataResponseStatus, error) {
	if err := intent.Validate(); err != nil {
		return KataResponseStatus{}, err
	}
	fingerprint, err := intent.Fingerprint()
	if err != nil {
		return KataResponseStatus{}, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return KataResponseStatus{}, err
	}
	defer conn.Close()
	if err = beginImmediate(ctx, conn); err != nil {
		return KataResponseStatus{}, err
	}
	committed := false
	defer func() {
		if !committed {
			if err := rollbackConn(conn); err != nil {
				log.Print("Kata response rollback failed")
			}
		}
	}()
	var stored string
	err = conn.QueryRowContext(ctx, `SELECT fingerprint FROM kata_responses WHERE operation_id=?`, intent.OperationID).Scan(&stored)
	switch {
	case err == nil:
		if stored != fingerprint {
			return KataResponseStatus{}, ErrResponseChanged
		}
	case errors.Is(err, sql.ErrNoRows):
		if request.OperationID != intent.OperationID || len(request.Responses) != 1 || request.Responses[0].Fingerprint != fingerprint {
			return KataResponseStatus{}, fmt.Errorf("invalid captured response")
		}
		raw, err := json.Marshal(intent)
		if err != nil {
			return KataResponseStatus{}, err
		}
		jobID := intent.JobID
		if jobID == 0 {
			if len(request.Entries) != 1 {
				return KataResponseStatus{}, fmt.Errorf("captured finding unavailable")
			}
			if err = conn.QueryRowContext(ctx, `SELECT job_id FROM finding_snapshots WHERE selector=?`, request.Entries[0].Selector).Scan(&jobID); err != nil {
				return KataResponseStatus{}, fmt.Errorf("captured finding unavailable")
			}
		}
		if err = InsertKataHandoffTx(ctx, conn, jobID, request); err != nil {
			return KataResponseStatus{}, err
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO kata_responses(operation_id,job_id,fingerprint,intent_json) VALUES(?,?,?,?)`, intent.OperationID, jobID, fingerprint, string(raw)); err != nil {
			return KataResponseStatus{}, err
		}
	default:
		return KataResponseStatus{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return KataResponseStatus{}, err
	}
	committed = true
	return readKataResponseStatus(ctx, conn, intent.OperationID)
}
