package storage

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
	"uuid"

	"go.kenn.io/roborev/internal/findingid"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// A finding snapshot pins one finding of a structured review under its
// selector (see internal/findingid) the first time that identity is handed out.
// It records the job UUID, the review UUID, the hash of the canonical document,
// the finding's zero-based index, the finding itself, and the panel it came
// from. Nothing updates a snapshot afterwards.
//
// The table has no foreign key to reviews. A rerun deletes the review and
// writes another with a new UUID, and a selector issued before the rerun must
// keep naming the finding it named: other systems, such as a Kata comment, hold
// it. Removing the repository removes the job's snapshots with it.
//
// The table is local to this machine. Neither the sync worker nor PostgreSQL
// reads or writes it, because the selector is derived from fields that do sync
// (the review UUID, the position, and the document hash): any machine holding
// the same review derives the same selector. Only the pinned copy of a finding
// that has since been replaced stays here.

var (
	// ErrNoReview means the job has no stored review.
	ErrNoReview = errors.New("job has no review")
	// ErrFindingChanged means the review's document no longer hashes to the
	// value its findings were pinned under. The pinned snapshot is kept as it
	// was and no new identity is assigned.
	ErrFindingChanged = errors.New("the review document changed after its findings were pinned")
)

const findingSnapshotsSchema = `
CREATE TABLE IF NOT EXISTS finding_snapshots (
  selector TEXT PRIMARY KEY,
  job_id INTEGER NOT NULL REFERENCES review_jobs(id),
  job_uuid TEXT NOT NULL,
  review_uuid TEXT NOT NULL,
  document_hash TEXT NOT NULL,
  finding_index INTEGER NOT NULL,
  finding_json TEXT NOT NULL,
  provenance_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (review_uuid, finding_index)
);
CREATE INDEX IF NOT EXISTS idx_finding_snapshots_job ON finding_snapshots(job_id);`

// migrateFindingSnapshots adds the snapshot table. It is safe to run again.
func migrateFindingSnapshots(ctx context.Context, db *DB) error {
	if _, err := db.ExecContext(ctx, findingSnapshotsSchema); err != nil {
		return fmt.Errorf("create finding_snapshots: %w", err)
	}
	return nil
}

// FindingProvenance says which panel a finding came from. A standalone review
// has none.
type FindingProvenance struct {
	PanelRunUUID     *uuid.UUID `json:"panel_run_uuid,omitempty"`
	PanelRole        string     `json:"panel_role,omitempty"`
	PanelName        string     `json:"panel_name,omitempty"`
	PanelMemberName  string     `json:"panel_member_name,omitempty"`
	PanelMemberIndex *int       `json:"panel_member_index,omitempty"`
	// SourceLabels names the member reviews a synthesized finding cites.
	SourceLabels []string `json:"source_labels,omitempty"`
}

// FindingSnapshot is one pinned finding.
type FindingSnapshot struct {
	Selector     string
	JobID        int64
	JobUUID      uuid.UUID
	ReviewUUID   uuid.UUID
	DocumentHash string
	Index        int
	Finding      structuredreview.Finding
	Provenance   FindingProvenance
	CreatedAt    time.Time
}

// snapshotQueries is what the snapshot code needs from a connection or a
// transaction, so a caller already inside a transaction can pin findings in it.
type snapshotQueries interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// findingSource is the review a job currently holds, ready to be pinned.
type findingSource struct {
	jobID      int64
	jobUUID    uuid.UUID
	reviewUUID uuid.UUID
	hash       string
	doc        structuredreview.Document
	panel      FindingProvenance
}

// AssignFindingSnapshots returns the identity of every finding in the job's
// current review, pinning the ones that have none yet. A review without
// structured findings, such as a clean or historical prose review, has none and
// returns an empty list. ErrNoReview means the job has no review.
func (db *DB) AssignFindingSnapshots(ctx context.Context, jobID int64) ([]FindingSnapshot, error) {
	return db.assignFindingSnapshots(ctx, jobID, nil)
}

// assignFindingSnapshots separates the initial review read from the write
// transaction so tests can coordinate a concurrent repository deletion.
func (db *DB) assignFindingSnapshots(ctx context.Context, jobID int64, afterSourceRead func()) ([]FindingSnapshot, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	src, err := loadFindingSource(ctx, conn, jobID)
	if err != nil || src == nil {
		return nil, err
	}
	if afterSourceRead != nil {
		afterSourceRead()
	}
	pinned, err := pinnedFindings(ctx, conn, src)
	if err != nil || len(pinned) == len(src.doc.Findings) {
		return pinned, err
	}

	if err := beginImmediate(ctx, conn); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			if err := rollbackConn(conn); err != nil {
				log.Printf("finding snapshots: rollback failed: %v", err)
			}
		}
	}()
	// Re-read under the write lock. A repository deletion may have committed
	// after the initial read while this connection waited to begin its txn.
	src, err = loadFindingSource(ctx, conn, jobID)
	if err != nil || src == nil {
		return nil, err
	}
	pinned, err = pinFindings(ctx, conn, src)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return pinned, nil
}

// loadFindingSource reads the job's review. It returns nil, nil when the review
// has no findings to name.
func loadFindingSource(ctx context.Context, q snapshotQueries, jobID int64) (*findingSource, error) {
	src := &findingSource{jobID: jobID}
	var (
		structured, role, name, memberName sql.NullString
		runUUID                            sql.Null[uuid.UUID]
		memberIndex                        sql.NullInt64
	)
	err := q.QueryRowContext(ctx, `
		SELECT rv.uuid, rv.structured_output, j.uuid,
		       j.panel_run_uuid, j.panel_role, j.panel_name, j.panel_member_name, j.panel_member_index
		FROM reviews rv
		JOIN review_jobs j ON j.id = rv.job_id
		WHERE rv.job_id = ?`, jobID).Scan(
		&src.reviewUUID, &structured, &src.jobUUID, &runUUID, &role, &name, &memberName, &memberIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoReview
	}
	if err != nil {
		return nil, fmt.Errorf("read review of job %d: %w", jobID, err)
	}
	if !structured.Valid || strings.TrimSpace(structured.String) == "" {
		return nil, nil
	}
	doc, err := structuredreview.Decode(jsontext.Value(structured.String))
	if err != nil {
		return nil, fmt.Errorf("decode review of job %d: %w", jobID, err)
	}
	if doc.Legacy != nil || len(doc.Findings) == 0 {
		return nil, nil
	}
	if src.hash, err = findingid.HashDocument(doc); err != nil {
		return nil, err
	}
	src.doc = doc
	src.panel.PanelRole, src.panel.PanelName = role.String, name.String
	src.panel.PanelMemberName = memberName.String
	if runUUID.Valid {
		src.panel.PanelRunUUID = &runUUID.V
	}
	if src.panel.PanelRole == PanelRoleMember && memberIndex.Valid {
		src.panel.PanelMemberIndex = new(int(memberIndex.Int64))
	}
	return src, nil
}

// provenanceOf is the panel provenance of finding i, with the labels of the
// reviews a synthesized finding cites.
func (s *findingSource) provenanceOf(finding structuredreview.Finding) FindingProvenance {
	p := s.panel
	for _, n := range finding.Sources {
		if n < 1 || n > len(s.doc.SourceLabels) {
			continue
		}
		if label := s.doc.SourceLabels[n-1]; label != "" && !slices.Contains(p.SourceLabels, label) {
			p.SourceLabels = append(p.SourceLabels, label)
		}
	}
	return p
}

// pinnedFindings returns the snapshots already stored for the review, in
// order, and fails with ErrFindingChanged if one was pinned under another
// document hash.
func pinnedFindings(ctx context.Context, q snapshotQueries, src *findingSource) ([]FindingSnapshot, error) {
	rows, err := q.QueryContext(ctx, findingSnapshotSelect+`
		WHERE review_uuid = ? ORDER BY finding_index`, src.reviewUUID)
	if err != nil {
		return nil, fmt.Errorf("read finding snapshots: %w", err)
	}
	defer rows.Close()
	var out []FindingSnapshot
	for rows.Next() {
		snap, err := scanFindingSnapshot(rows)
		if err != nil {
			return nil, err
		}
		if snap.DocumentHash != src.hash {
			return nil, fmt.Errorf("%w: finding %d of review %s", ErrFindingChanged, snap.Index, snap.ReviewUUID)
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

// pinFindings stores a snapshot for every finding that has none, then returns
// all of them. An existing snapshot is never rewritten.
func pinFindings(ctx context.Context, q snapshotQueries, src *findingSource) ([]FindingSnapshot, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i, finding := range src.doc.Findings {
		id, err := findingid.New(src.reviewUUID, i, src.hash)
		if err != nil {
			return nil, err
		}
		findingJSON, err := json.Marshal(finding)
		if err != nil {
			return nil, fmt.Errorf("encode finding %d: %w", i, err)
		}
		provenanceJSON, err := json.Marshal(src.provenanceOf(finding))
		if err != nil {
			return nil, fmt.Errorf("encode finding %d provenance: %w", i, err)
		}
		if _, err := q.ExecContext(ctx, `
			INSERT OR IGNORE INTO finding_snapshots
			  (selector, job_id, job_uuid, review_uuid, document_hash, finding_index, finding_json, provenance_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id.String(), src.jobID, src.jobUUID, src.reviewUUID, src.hash, i,
			string(findingJSON), string(provenanceJSON), now); err != nil {
			return nil, fmt.Errorf("pin finding %d: %w", i, err)
		}
	}
	pinned, err := pinnedFindings(ctx, q, src)
	if err != nil {
		return nil, err
	}
	if len(pinned) != len(src.doc.Findings) {
		return nil, fmt.Errorf("%w: pinned %d of %d findings", ErrFindingChanged, len(pinned), len(src.doc.Findings))
	}
	return pinned, nil
}

// FindingSnapshot returns the finding pinned under selector, which must be
// given in full. The review it came from need not still exist. It returns
// findingid.ErrInvalid, before the database is asked, for text that is not a
// complete selector, and findingid.ErrNotFound when no finding was pinned
// under a well-formed one. A selector for the right review and position under
// a document hash the review never had is the second case.
func (db *DB) FindingSnapshot(ctx context.Context, selector string) (FindingSnapshot, error) {
	id, err := findingid.Parse(selector)
	if err != nil {
		return FindingSnapshot{}, err
	}
	snap, err := scanFindingSnapshot(db.QueryRowContext(ctx, findingSnapshotSelect+` WHERE selector = ?`, id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return FindingSnapshot{}, fmt.Errorf("%w: %s", findingid.ErrNotFound, selector)
	}
	return snap, err
}

const findingSnapshotSelect = `
	SELECT selector, job_id, job_uuid, review_uuid, document_hash, finding_index,
	       finding_json, provenance_json, created_at
	FROM finding_snapshots`

type rowScanner interface{ Scan(dest ...any) error }

func scanFindingSnapshot(row rowScanner) (FindingSnapshot, error) {
	var (
		snap                        FindingSnapshot
		findingJSON, provenanceJSON string
		createdAt                   string
	)
	if err := row.Scan(&snap.Selector, &snap.JobID, &snap.JobUUID, &snap.ReviewUUID, &snap.DocumentHash,
		&snap.Index, &findingJSON, &provenanceJSON, &createdAt); err != nil {
		return FindingSnapshot{}, err
	}
	var err error
	if snap.Finding, err = decodeStoredFinding(findingJSON); err != nil {
		return FindingSnapshot{}, fmt.Errorf("finding %s: %w", snap.Selector, err)
	}
	if err := json.Unmarshal([]byte(provenanceJSON), &snap.Provenance); err != nil {
		return FindingSnapshot{}, fmt.Errorf("finding %s provenance: %w", snap.Selector, err)
	}
	if snap.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return FindingSnapshot{}, fmt.Errorf("finding %s has an invalid creation time: %w", snap.Selector, err)
	}
	return snap, nil
}

// decodeStoredFinding reads back what json.Marshal wrote for a Finding, which
// encodes an empty location as null.
func decodeStoredFinding(raw string) (structuredreview.Finding, error) {
	var wire struct {
		Severity string  `json:"severity"`
		Problem  string  `json:"problem"`
		Fix      string  `json:"fix"`
		Location *string `json:"location"`
		Sources  []int   `json:"sources"`
	}
	if err := json.Unmarshal([]byte(raw), &wire, json.RejectUnknownMembers(true)); err != nil {
		return structuredreview.Finding{}, fmt.Errorf("decode pinned finding: %w", err)
	}
	f := structuredreview.Finding{Severity: wire.Severity, Problem: wire.Problem, Fix: wire.Fix, Sources: wire.Sources}
	if wire.Location != nil {
		f.Location = *wire.Location
	}
	return f, nil
}
