package storage

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"uuid"

	"go.kenn.io/roborev/internal/findingid"
	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// KataCompletion is passed to the daemon before acquiring the SQLite write
// lock. The builder can resolve a destination; insertion and snapshots remain
// atomic with the terminal transition. Retry workers never invoke the builder.
type KataCompletion struct {
	Job        *ReviewJob
	ReviewUUID string
	Snapshots  []FindingSnapshot
	Output     string
	Verdict    Verdict
	Error      string
	At         time.Time
}

type KataHandoffBuilder func(context.Context, KataCompletion) ([]katapublish.Request, error)

// SetKataHandoffBuilder installs the daemon's policy capture. It does not scan
// historical reviews. Tests and imports without a builder authorize no delivery.
func (db *DB) SetKataHandoffBuilder(builder KataHandoffBuilder) {
	db.kataBuilderMu.Lock()
	defer db.kataBuilderMu.Unlock()
	db.kataBuilder = builder
}

func migrateKataHandoffs(ctx context.Context, db *DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS kata_handoffs (
 operation_id TEXT PRIMARY KEY,
 job_id INTEGER NOT NULL REFERENCES review_jobs(id) ON DELETE CASCADE,
 review_uuid TEXT NOT NULL,
 fingerprint TEXT NOT NULL,
 request_json TEXT NOT NULL,
 state_json TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending',
 attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt INTEGER NOT NULL DEFAULT 0,
 claim TEXT NOT NULL DEFAULT '',
 claimed_until INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT ''
 ); CREATE INDEX IF NOT EXISTS idx_kata_handoffs_pending ON kata_handoffs(status,next_attempt);
 CREATE TABLE IF NOT EXISTS kata_destinations(endpoint TEXT NOT NULL,project TEXT NOT NULL,project_uid TEXT NOT NULL,PRIMARY KEY(endpoint,project));`)
	return err
}

// KataHandoff retains the exact authorized payload independently of mutable
// reviews, hook settings, broadcasts, and credentials. It is local-only, like
// finding snapshots; PostgreSQL sync never schedules another machine's writes.
type KataHandoff struct {
	OperationID string              `json:"operation_id"`
	JobID       int64               `json:"job_id"`
	ReviewUUID  string              `json:"review_uuid"`
	Fingerprint string              `json:"fingerprint"`
	Request     katapublish.Request `json:"-"`
	State       katapublish.State   `json:"-"`
	Status      string              `json:"state"`
	Attempts    int                 `json:"attempts"`
	NextAttempt int64               `json:"next_attempt"`
	LastError   string              `json:"last_error,omitempty"`
	Claim       string              `json:"-"`
}

var ErrKataClaimLost = errors.New("kata delivery claim lost")

// KataFailureRun stays stable across automatic retries and daemon restarts.
// ReenqueueJob advances EnqueuedAt monotonically for each manual rerun while
// retaining the job UUID, so a later failure cannot reuse an old operation key.
func KataFailureRun(job *ReviewJob) string {
	identity := fmt.Sprint(job.ID)
	if job.UUID != nil {
		identity = job.UUID.String()
	}
	return identity + ":" + job.EnqueuedAt.UTC().Format(time.RFC3339Nano)
}

// Destination resolution runs outside the write transaction. Revalidate the
// persisted run and attempt before attaching captured data to a terminal job.
func kataJobUnchanged(ctx context.Context, q snapshotQueries, job *ReviewJob) (bool, error) {
	var status JobStatus
	var enqueued, started string
	if err := q.QueryRowContext(ctx, `SELECT status,enqueued_at,COALESCE(started_at,'') FROM review_jobs WHERE id=?`, job.ID).Scan(&status, &enqueued, &started); err != nil {
		return false, err
	}
	return status == job.Status && parseSQLiteTime(enqueued).Equal(job.EnqueuedAt) && started == job.StartedAtRaw, nil
}

func (db *DB) prepareKataCompletion(ctx context.Context, job *ReviewJob, reviewUUID string, raw jsontext.Value, output string, verdict Verdict, failure string, now time.Time, prepared ...KataHandoffBuilder) ([]katapublish.Request, error) {
	db.kataBuilderMu.RLock()
	builder := db.kataBuilder
	db.kataBuilderMu.RUnlock()
	if len(prepared) > 0 && prepared[0] != nil {
		builder = prepared[0]
	}
	if builder == nil {
		return nil, nil
	}
	c := KataCompletion{Job: job, ReviewUUID: reviewUUID, Output: output, Verdict: verdict, Error: failure, At: now}
	if len(raw) > 0 {
		doc, err := structuredreview.Decode(raw)
		if err != nil {
			return nil, err
		}
		c.Output = doc.Markdown(job.MinSeverity)
		if doc.Legacy == nil && len(doc.Findings) > 0 {
			hash, err := findingid.HashDocument(doc)
			if err != nil {
				return nil, err
			}
			source := findingSource{doc: doc, panel: FindingProvenance{PanelRunUUID: job.PanelRunUUID, PanelRole: job.PanelRole, PanelName: job.PanelName, PanelMemberName: job.PanelMemberName, PanelMemberIndex: new(job.PanelMemberIndex)}}
			for i, finding := range doc.Findings {
				id, err := findingid.New(uuid.MustParse(reviewUUID), i, hash)
				if err != nil {
					return nil, err
				}
				snap := FindingSnapshot{Selector: id.String(), JobID: job.ID, ReviewUUID: uuid.MustParse(reviewUUID), DocumentHash: hash, Index: i, Finding: finding, Provenance: source.provenanceOf(finding)}
				if job.UUID != nil {
					snap.JobUUID = *job.UUID
				}
				c.Snapshots = append(c.Snapshots, snap)
			}
		}
	}
	requests, err := builder(ctx, c)
	if err == nil {
		for _, request := range requests {
			if _, err = katapublish.Fingerprint(request); err != nil {
				break
			}
		}
	}
	if err != nil {
		// A broken local hook cannot discard a finished review or repeatedly fail
		// its failure transition. Retain an inert operation with no remote payload;
		// an explicit publication can authorize it once configuration is repaired.
		identity := reviewUUID
		if identity == "" {
			identity = KataFailureRun(job)
		}
		requests = []katapublish.Request{{OperationID: "blocked:" + identity, ReviewUUID: reviewUUID, Policy: "blocked"}}
	}
	if reviewUUID == "" {
		for i := range requests {
			requests[i].FailureRun = KataFailureRun(job)
		}
	}
	return requests, nil
}

func persistKataCompletion(ctx context.Context, q snapshotQueries, job *ReviewJob, requests []katapublish.Request) error {
	if len(requests) == 0 {
		return nil
	}
	if requests[0].ReviewUUID != "" {
		src, err := loadFindingSource(ctx, q, job.ID)
		if err != nil {
			return err
		}
		if src != nil {
			if _, err = pinFindings(ctx, q, src); err != nil {
				return err
			}
		}
	}
	for _, req := range requests {
		if err := InsertKataHandoffTx(ctx, q, job.ID, req); err != nil {
			return err
		}
	}
	return nil
}

// InsertKataHandoffTx can be used by future response enqueue transactions. A
// reused operation ID must name the identical immutable request.
func InsertKataHandoffTx(ctx context.Context, q snapshotQueries, jobID int64, req katapublish.Request) error {
	fingerprint, err := katapublish.Fingerprint(req)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	initialState := `{}`
	if len(req.Responses) > 0 {
		encoded, err := json.Marshal(katapublish.State{IssueUID: req.IssueUID, IssueRef: req.Project + "#" + req.IssueUID, ProjectUID: req.ProjectUID})
		if err != nil {
			return err
		}
		initialState = string(encoded)
	}
	if _, err = q.ExecContext(ctx, `INSERT INTO kata_handoffs(operation_id,job_id,review_uuid,fingerprint,request_json,state_json) VALUES(?,?,?,?,?,?) ON CONFLICT(operation_id) DO NOTHING`, req.OperationID, jobID, req.ReviewUUID, fingerprint, string(raw), initialState); err != nil {
		return err
	}
	var stored string
	if err = q.QueryRowContext(ctx, `SELECT fingerprint FROM kata_handoffs WHERE operation_id=?`, req.OperationID).Scan(&stored); err != nil {
		return err
	}
	if stored != fingerprint {
		return fmt.Errorf("kata operation fingerprint mismatch")
	}
	return nil
}

// CaptureKataHandoffs is the explicit backfill boundary. Startup never calls it.
func (db *DB) CaptureKataHandoffs(ctx context.Context, jobID int64) error {
	job, err := db.GetJobByID(jobID)
	if err != nil {
		return err
	}
	if job.Status != JobStatusDone && job.Status != JobStatusFailed {
		return fmt.Errorf("job is not complete")
	}
	existing, err := db.KataHandoffs(ctx, jobID)
	if err != nil {
		return err
	}
	var reviewUUID, output string
	var structured sql.NullString
	var passed sql.NullBool
	if job.Status == JobStatusDone {
		if err = db.QueryRowContext(ctx, `SELECT uuid,structured_output,output,verdict_bool FROM reviews WHERE job_id=?`, jobID).Scan(&reviewUUID, &structured, &output, &passed); err != nil {
			return err
		}
	}
	for _, h := range existing {
		if len(h.Request.Responses) == 0 && h.ReviewUUID == reviewUUID && (reviewUUID != "" || h.Request.FailureRun == KataFailureRun(job)) && h.Request.Policy != "blocked" {
			return nil
		}
	}
	var documentHash string
	if strings.TrimSpace(structured.String) != "" {
		documentHash, err = findingid.DocumentHash([]byte(structured.String))
		if err != nil {
			return err
		}
	}
	verdict := VerdictUnknown
	if passed.Valid {
		verdict = VerdictFromPassed(passed.Bool)
	}
	requests, err := db.prepareKataCompletion(ctx, job, reviewUUID, []byte(structured.String), output, verdict, job.Error, time.Now())
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err = beginImmediate(ctx, conn); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if rollbackErr := rollbackConn(conn); rollbackErr != nil {
				log.Printf("Kata capture rollback: %v", rollbackErr)
			}
		}
	}()
	unchanged, err := kataJobUnchanged(ctx, conn, job)
	if err != nil {
		return err
	}
	if !unchanged {
		return ErrFindingChanged
	}
	if reviewUUID != "" {
		var current, currentOutput string
		var currentStructured sql.NullString
		if err = conn.QueryRowContext(ctx, `SELECT uuid,structured_output,output FROM reviews WHERE job_id=?`, jobID).Scan(&current, &currentStructured, &currentOutput); err != nil {
			return err
		}
		if current != reviewUUID {
			return ErrFindingChanged
		}
		// Sync can replace an unpinned document without replacing its UUID.
		// Validate the exact canonical document used to prepare the handoff
		// before pinning findings from the current row.
		if documentHash != "" {
			currentHash, hashErr := findingid.DocumentHash([]byte(currentStructured.String))
			if hashErr != nil || currentHash != documentHash {
				return ErrFindingChanged
			}
		} else if currentStructured.String != structured.String || currentOutput != output {
			return ErrFindingChanged
		}
	}
	// A concurrent explicit publication may have committed while destination
	// resolution ran outside the write lock. Current-review authorization wins.
	var captured int
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM kata_handoffs WHERE job_id=? AND review_uuid=? AND (?!='' OR json_extract(request_json,'$.failure_run')=?) AND json_extract(request_json,'$.Policy')!='blocked'`, jobID, reviewUUID, reviewUUID, KataFailureRun(job)).Scan(&captured); err != nil {
		return err
	}
	if captured > 0 {
		return nil
	}
	if err = persistKataCompletion(ctx, conn, job, requests); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	committed = err == nil
	return err
}

const kataHandoffSelect = `SELECT operation_id,job_id,review_uuid,fingerprint,request_json,state_json,status,attempts,next_attempt,last_error,claim FROM kata_handoffs`

func scanKataHandoff(row rowScanner) (KataHandoff, error) {
	var h KataHandoff
	var req, state string
	if err := row.Scan(&h.OperationID, &h.JobID, &h.ReviewUUID, &h.Fingerprint, &req, &state, &h.Status, &h.Attempts, &h.NextAttempt, &h.LastError, &h.Claim); err != nil {
		return h, err
	}
	if err := json.Unmarshal([]byte(req), &h.Request, json.RejectUnknownMembers(true)); err != nil {
		return h, err
	}
	err := json.Unmarshal([]byte(state), &h.State, json.RejectUnknownMembers(true))
	return h, err
}

func (db *DB) KataHandoffs(ctx context.Context, jobID int64) ([]KataHandoff, error) {
	rows, err := db.QueryContext(ctx, kataHandoffSelect+` WHERE job_id=? ORDER BY operation_id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KataHandoff{}
	for rows.Next() {
		h, err := scanKataHandoff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// A lease is renewed at each durable checkpoint. It lets a restarted daemon
// recover abandoned work without allowing a stale worker to record success.
const kataClaimLease = time.Minute

func (db *DB) ClaimKataHandoff(ctx context.Context, now time.Time) (*KataHandoff, error) {
	token := uuid.New().String()
	row := db.QueryRowContext(ctx, `UPDATE kata_handoffs SET claim=?,claimed_until=?,attempts=attempts+1 WHERE operation_id=(SELECT operation_id FROM kata_handoffs WHERE status='pending' AND next_attempt<=? AND claimed_until<=? ORDER BY next_attempt,operation_id LIMIT 1) RETURNING operation_id,job_id,review_uuid,fingerprint,request_json,state_json,status,attempts,next_attempt,last_error,claim`, token, now.Add(kataClaimLease).UnixNano(), now.UnixNano(), now.UnixNano())
	h, err := scanKataHandoff(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (db *DB) CheckpointKataHandoff(ctx context.Context, id, claim string, state katapublish.State) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE kata_handoffs SET state_json=?,claimed_until=? WHERE operation_id=? AND claim=? AND status='pending'`, string(raw), time.Now().Add(kataClaimLease).UnixNano(), id, claim)
	return kataClaimResult(result, err)
}

// FinishKataHandoff keeps ledger-pending work schedulable. The caller supplies
// a safe diagnostic; raw provider errors may echo confidential review text.
func (db *DB) FinishKataHandoff(ctx context.Context, h KataHandoff, deliveryErr error, now time.Time) error {
	status, diagnostic := "pending", ""
	if deliveryErr == nil && h.State.Done {
		status = "done"
	} else if deliveryErr != nil {
		diagnostic = deliveryErr.Error()
	} else {
		diagnostic = "issue ledger remains pending"
	}
	delay := min(30*time.Second*time.Duration(1<<min(h.Attempts-1, 7)), time.Hour)
	raw, err := json.Marshal(h.State)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE kata_handoffs SET state_json=?,status=?,last_error=?,next_attempt=?,claim='',claimed_until=0 WHERE operation_id=? AND claim=? AND status='pending'`, string(raw), status, diagnostic, now.Add(delay).UnixNano(), h.OperationID, h.Claim)
	return kataClaimResult(result, err)
}

func kataClaimResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrKataClaimLost
	}
	return nil
}

// CachedKataDestination is only a previously validated identity for this exact
// endpoint and frozen project selection. It grants no publication on its own.
func (db *DB) CachedKataDestination(ctx context.Context, endpoint, project string) (string, error) {
	var uid string
	err := db.QueryRowContext(ctx, `SELECT project_uid FROM kata_destinations WHERE endpoint=? AND project=?`, endpoint, project).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return uid, err
}

func (db *DB) RememberKataDestination(ctx context.Context, endpoint, project, uid string) error {
	if uid == "" {
		return fmt.Errorf("project UID is required")
	}
	_, err := db.ExecContext(ctx, `INSERT INTO kata_destinations(endpoint,project,project_uid) VALUES(?,?,?) ON CONFLICT(endpoint,project) DO UPDATE SET project_uid=excluded.project_uid`, endpoint, project, uid)
	return err
}
