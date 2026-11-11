package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"uuid"

	"go.kenn.io/roborev/pkg/structuredreview"
)

// KataPublicationSeed is the offline authorization selected for a review.
// Alias fields are present for a committed workspace binding; a local override
// is selected by project name alone. Credentials never belong here.
type KataPublicationSeed struct {
	Endpoint      string `json:"endpoint"`
	Project       string `json:"project"`
	AliasKind     string `json:"alias_kind,omitempty"`
	AliasIdentity string `json:"alias_identity,omitempty"`
	Policy        string `json:"policy"`
}

// KataPublication contains local recovery facts, not a copy of the payload.
type KataPublication struct {
	KataPublicationSeed
	Version            int    `json:"version"`
	DocumentHash       string `json:"document_hash"`
	State              string `json:"state"`
	ProjectUID         string `json:"project_uid,omitempty"`
	RequestFingerprint string `json:"request_fingerprint,omitempty"`
}

// KataPublicationRecord identifies the current review and its local marker.
// Marker is nil when no publication was authorized on this machine.
type KataPublicationRecord struct {
	JobID        int64
	ReviewUUID   uuid.UUID
	DocumentHash string
	SourceOutput string
	Marker       *KataPublication
	document     string
	markerJSON   sql.NullString
}

// KataDocumentHash hashes the canonical structured review, independently of
// JSON whitespace and field order. It is shared by capture and replay guards.
func KataDocumentHash(raw []byte) (string, error) {
	if _, err := structuredreview.Decode(raw); err != nil {
		return "", err
	}
	canonical := jsontext.Value(append([]byte(nil), raw...))
	if err := canonical.Canonicalize(); err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func newKataPublication(seed KataPublicationSeed, raw []byte) (string, error) {
	if seed.Endpoint == "" || (seed.Project == "" && seed.AliasKind == "") || (seed.Policy != "comments" && seed.Policy != "summary") || (seed.AliasKind == "") != (seed.AliasIdentity == "") {
		return "", errors.New("invalid Kata publication authorization")
	}
	hash, err := KataDocumentHash(raw)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(KataPublication{KataPublicationSeed: seed, Version: 1, DocumentHash: hash, State: "pending"})
	return string(data), err
}

func scanKataPublication(scanner interface{ Scan(...any) error }) (*KataPublicationRecord, error) {
	var record KataPublicationRecord
	if err := scanner.Scan(&record.JobID, &record.ReviewUUID, &record.document, &record.markerJSON, &record.SourceOutput); err != nil {
		return nil, err
	}
	var err error
	record.DocumentHash, err = KataDocumentHash([]byte(record.document))
	if err != nil {
		// Preserve the identity so a read can still return the original legacy
		// review as unavailable without treating it as a publication match.
		return &record, fmt.Errorf("hash review document: %w", err)
	}
	if record.markerJSON.Valid {
		var marker KataPublication
		if err := json.Unmarshal([]byte(record.markerJSON.String), &marker); err != nil {
			return nil, fmt.Errorf("decode Kata publication: %w", err)
		}
		record.Marker = &marker
	}
	return &record, nil
}

// GetKataPublication returns the current review for a job, even without a marker.
func (db *DB) GetKataPublication(ctx context.Context, jobID int64) (*KataPublicationRecord, error) {
	return scanKataPublication(db.QueryRowContext(ctx, `SELECT job_id, uuid, COALESCE(structured_output, ''), kata_publication, output FROM reviews WHERE job_id = ?`, jobID))
}

// GetKataPublicationByUUID returns the current review bearing this UUID.
func (db *DB) GetKataPublicationByUUID(ctx context.Context, id uuid.UUID) (*KataPublicationRecord, error) {
	return scanKataPublication(db.QueryRowContext(ctx, `SELECT job_id, uuid, COALESCE(structured_output, ''), kata_publication, output FROM reviews WHERE uuid = ?`, id))
}

// PendingKataPublications lists local authorizations to consider at startup.
func (db *DB) PendingKataPublications(ctx context.Context) ([]KataPublicationRecord, error) {
	rows, err := db.QueryContext(ctx, `SELECT job_id, uuid, COALESCE(structured_output, ''), kata_publication, output FROM reviews WHERE kata_publication IS NOT NULL AND json_extract(kata_publication, '$.state') = 'pending' AND julianday(created_at) > julianday('now', '-7 days', '+5 seconds') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []KataPublicationRecord
	for rows.Next() {
		record, err := scanKataPublication(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, *record)
	}
	return records, rows.Err()
}

// AuthorizeKataPublication saves an explicit authorization only if the
// unmarked, failing review still has the document read by the caller.
func (db *DB) AuthorizeKataPublication(ctx context.Context, record *KataPublicationRecord, seed KataPublicationSeed) error {
	if record == nil || record.markerJSON.Valid {
		return ErrReviewChanged
	}
	hash, err := KataDocumentHash([]byte(record.document))
	if err != nil || hash != record.DocumentHash {
		return ErrReviewChanged
	}
	marker, err := newKataPublication(seed, []byte(record.document))
	if err != nil {
		return err
	}
	return db.writeKataPublication(ctx, record, marker, true)
}

// UpdateKataPublication pins destination/request identity or completes a
// previously read marker. Immutable authorization and existing pins cannot be
// changed. The compare-and-swap also prevents concurrent updates being lost.
func (db *DB) UpdateKataPublication(ctx context.Context, record *KataPublicationRecord, next KataPublication) error {
	if record == nil || !record.markerJSON.Valid {
		return ErrReviewChanged
	}
	var old KataPublication
	if err := json.Unmarshal([]byte(record.markerJSON.String), &old); err != nil {
		return fmt.Errorf("decode Kata publication: %w", err)
	}
	if old.Version != 1 || next.Version != old.Version || next.KataPublicationSeed != old.KataPublicationSeed ||
		next.DocumentHash != old.DocumentHash || next.DocumentHash != record.DocumentHash ||
		(old.ProjectUID != "" && next.ProjectUID != old.ProjectUID) ||
		(old.RequestFingerprint != "" && next.RequestFingerprint != old.RequestFingerprint) ||
		(old.State != "pending" && next.State != old.State) || (next.State != "pending" && next.State != "complete") ||
		(next.ProjectUID == "") != (next.RequestFingerprint == "") || (next.State == "complete" && next.ProjectUID == "") {
		return errors.New("invalid Kata publication update")
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return db.writeKataPublication(ctx, record, string(data), false)
}

func (db *DB) writeKataPublication(ctx context.Context, record *KataPublicationRecord, marker string, unmarked bool) error {
	if record.DocumentHash == "" {
		return ErrReviewChanged
	}
	query := `UPDATE reviews SET kata_publication = ? WHERE job_id = ? AND uuid = ? AND structured_output = ? AND kata_publication IS ?`
	if unmarked {
		query += ` AND verdict_bool = 0`
	}
	var previous any
	if record.markerJSON.Valid {
		previous = record.markerJSON.String
	}
	result, err := db.ExecContext(ctx, query, marker, record.JobID, record.ReviewUUID, record.document, previous)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrReviewChanged
	}
	return nil
}

// RunKataPublication guards one callback (normally one POST) against rerun,
// deletion, and pulled review replacement. The caller must bound ctx and the
// callback must honor it. No SQL transaction spans the callback. The callback
// must not recursively acquire the attempt lock or mutate jobs.
func (db *DB) RunKataPublication(ctx context.Context, id uuid.UUID, hash string, callback func(context.Context) error) error {
	if err := db.attempts.mu.RLockContext(ctx); err != nil {
		return err
	}
	defer db.attempts.mu.RUnlock()
	record, err := db.GetKataPublicationByUUID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReviewChanged
	}
	if err != nil {
		return err
	}
	if hash == "" || record.DocumentHash != hash {
		return ErrReviewChanged
	}
	return callback(ctx)
}
