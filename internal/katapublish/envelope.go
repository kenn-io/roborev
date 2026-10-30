// Package katapublish delivers frozen review publications through Kata's shared
// metadata ledger. Local checkpoints and guarded metadata writes are both
// required: registering pending work is never a delivery receipt.
package katapublish

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/kata"
)

const (
	EnvelopeKey    = "roborev.findings"
	ActionKey      = "roborev.action"
	FingerprintKey = "roborev.fingerprint"
)

var ErrPending = errors.New("kata publication remains pending")

// Request contains only immutable, policy-filtered data captured by the
// completion transaction. The service never reloads review text or hook config.
type Request struct {
	// CredentialFingerprint binds retries to the captured credential locally.
	// Only the hash of the complete request enters Kata metadata.
	CredentialFingerprint string
	// FailureRun identifies a local failed manual run, including its persisted
	// enqueue generation. Completed reviews use ReviewUUID instead. Omission
	// preserves existing completed-publication fingerprints.
	FailureRun string `json:"failure_run,omitempty"`
	// Response-only captures are omitted from publication encodings so existing
	// K2 payload fingerprints retain their exact meaning.
	PublicationOperationID string        `json:",omitempty"`
	PublicationFingerprint string        `json:",omitempty"`
	PublicationProjectUID  string        `json:",omitempty"`
	RootAttributionHash    string        `json:",omitempty"`
	PublicationEntries     []LedgerEntry `json:",omitempty"`
	// IssueUID identifies the existing issue for explicit response delivery.
	IssueUID    string
	OperationID string
	Endpoint    string
	Project     string
	ProjectUID  string
	Policy      string
	ReviewUUID  string
	Teammate    string
	Create      kata.CreateReq
	Entries     []Entry
	Responses   []Response
}

type Entry struct {
	Selector     string `json:"selector"`
	ReviewUUID   string `json:"review_uuid"`
	DocumentHash string `json:"document_hash"`
	Index        int    `json:"index"`
	Body         string `json:"body"`
}

// Response is one immutable typed finding response, registered before sending.
type Response struct {
	Root        string `json:"root,omitempty"`
	OperationID string `json:"operation_id"`
	Fingerprint string `json:"fingerprint"`
	Body        string `json:"body"`
	Target      string `json:"target"`
	Kind        string `json:"kind"`
	Teammate    string `json:"teammate"`
}

type Receipt struct {
	UID         string
	Attribution kata.Attribution
}

// State is checkpointed before every external send. FirstAttempt is retained
// after failures, because retrying after Kata discards its replay key is unsafe.
type State struct {
	IssueUID     string
	IssueRef     string
	ProjectUID   string
	Attribution  kata.Attribution
	Receipts     map[string]Receipt
	FirstAttempt map[string]time.Time
	Done         bool
}

type LedgerEntry struct {
	Selector        string `json:"selector"`
	ReviewUUID      string `json:"review_uuid"`
	DocumentHash    string `json:"document_hash"`
	Index           int    `json:"index"`
	BodyHash        string `json:"body_hash"`
	Pending         string `json:"pending"`
	ReceiptUID      string `json:"receipt_uid"`
	AttributionHash string `json:"attribution_hash"`
}

type LedgerResponse struct {
	Response
	ReceiptUID      string `json:"receipt_uid"`
	AttributionHash string `json:"attribution_hash"`
}

type Envelope struct {
	Version        int              `json:"version"`
	WriterEndpoint string           `json:"writer_endpoint"`
	ProjectUID     string           `json:"project_uid"`
	OperationID    string           `json:"operation_id"`
	Fingerprint    string           `json:"fingerprint"`
	Policy         string           `json:"policy"`
	ReviewUUID     string           `json:"review_uuid"`
	Entries        []LedgerEntry    `json:"entries"`
	Responses      []LedgerResponse `json:"responses"`
}

func hash(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func Fingerprint(req Request) (string, error) {
	data, err := json.Marshal(req, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return hash(string(data)), nil
}

func marker(req Request, operation string) string {
	return kata.CommentRecoveryMarker(hash(req.Endpoint + "\x00" + req.ProjectUID + "\x00" + req.OperationID + "\x00" + operation)[:24])
}

func action(req Request, projectUID string) string {
	return hash(req.Endpoint + "\x00" + projectUID + "\x00" + req.OperationID)
}

func newEnvelope(req Request, projectUID string) (Envelope, error) {
	fingerprint, err := Fingerprint(req)
	if err != nil {
		return Envelope{}, err
	}
	env := Envelope{Version: 1, WriterEndpoint: req.Endpoint, ProjectUID: projectUID, OperationID: req.OperationID, Fingerprint: fingerprint, Policy: req.Policy, ReviewUUID: req.ReviewUUID, Entries: []LedgerEntry{}, Responses: []LedgerResponse{}}
	for _, entry := range req.Entries {
		env.Entries = append(env.Entries, LedgerEntry{Selector: entry.Selector, ReviewUUID: entry.ReviewUUID, DocumentHash: entry.DocumentHash, Index: entry.Index, BodyHash: hash(entry.Body), Pending: entry.Body})
	}
	for _, response := range req.Responses {
		env.Responses = append(env.Responses, LedgerResponse{Response: response})
	}
	return env, nil
}

// DecodeEnvelope refuses newer versions, unknown fields, duplicate fields and
// missing mapping fields. Federation changes must not disappear in a rewrite.
func DecodeEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env, json.RejectUnknownMembers(true)); err != nil {
		return env, fmt.Errorf("%w: invalid ledger schema", ErrPending)
	}
	if env.Version != 1 || env.WriterEndpoint == "" || env.ProjectUID == "" || env.OperationID == "" || env.Fingerprint == "" || (env.ReviewUUID == "" && (len(env.Entries) > 0 || len(env.Responses) > 0)) || env.Entries == nil || env.Responses == nil {
		return env, fmt.Errorf("%w: incomplete ledger", ErrPending)
	}
	var raw map[string]jsontext.Value
	if err := json.Unmarshal(data, &raw); err != nil {
		return env, err
	}
	for _, key := range []string{"version", "writer_endpoint", "project_uid", "operation_id", "fingerprint", "policy", "review_uuid", "entries", "responses"} {
		if _, ok := raw[key]; !ok {
			return env, fmt.Errorf("%w: missing ledger field", ErrPending)
		}
	}
	var entries []map[string]jsontext.Value
	if err := json.Unmarshal(raw["entries"], &entries); err != nil {
		return env, err
	}
	entrySelectors := map[string]bool{}
	for i, e := range env.Entries {
		for _, key := range []string{"selector", "review_uuid", "document_hash", "index", "body_hash", "pending", "receipt_uid", "attribution_hash"} {
			if _, ok := entries[i][key]; !ok {
				return env, fmt.Errorf("%w: missing finding field", ErrPending)
			}
		}
		if entrySelectors[e.Selector] {
			return env, pending("duplicate finding selector")
		}
		entrySelectors[e.Selector] = true
		if !validReceiptHash(e.ReceiptUID, e.AttributionHash) {
			return env, pending("invalid finding receipt hash")
		}
		if e.Selector == "" || e.ReviewUUID == "" || e.DocumentHash == "" || e.Index < 0 || e.BodyHash == "" {
			return env, fmt.Errorf("%w: incomplete finding mapping", ErrPending)
		}
	}
	var responses []map[string]jsontext.Value
	if err := json.Unmarshal(raw["responses"], &responses); err != nil {
		return env, pending("invalid response operations")
	}
	seen := map[string]bool{}
	for i, response := range env.Responses {
		for _, key := range []string{"operation_id", "fingerprint", "body", "target", "kind", "teammate", "receipt_uid", "attribution_hash"} {
			if _, ok := responses[i][key]; !ok {
				return env, pending("missing response field")
			}
		}
		if response.OperationID == "" || seen[response.OperationID] || response.Fingerprint == "" || response.Body == "" || response.Target == "" || response.Kind == "" || response.Teammate == "" {
			return env, pending("incomplete response operation")
		}
		seen[response.OperationID] = true
		if !validReceiptHash(response.ReceiptUID, response.AttributionHash) {
			return env, pending("invalid response receipt hash")
		}
	}
	return env, nil
}

func (env Envelope) validateRequest(req Request, projectUID string) error {
	want, err := newEnvelope(req, projectUID)
	if err != nil {
		return err
	}
	if env.Version != want.Version || env.WriterEndpoint != want.WriterEndpoint || env.ProjectUID != want.ProjectUID || env.OperationID != want.OperationID || env.Fingerprint != want.Fingerprint || env.Policy != want.Policy || env.ReviewUUID != want.ReviewUUID || len(env.Entries) != len(want.Entries) || (len(want.Responses) > 0 && len(env.Responses) != len(want.Responses)) {
		return fmt.Errorf("%w: publication ledger changed", ErrPending)
	}
	seen := map[string]bool{}
	for i, e := range env.Entries {
		expected := want.Entries[i]
		if seen[e.Selector] || e.Selector != expected.Selector || e.ReviewUUID != expected.ReviewUUID || e.DocumentHash != expected.DocumentHash || e.Index != expected.Index || e.BodyHash != expected.BodyHash || (e.ReceiptUID == "" && e.Pending != expected.Pending) || (e.ReceiptUID != "" && e.Pending != "") {
			return fmt.Errorf("%w: finding mapping changed", ErrPending)
		}
		seen[e.Selector] = true
	}
	for i, e := range want.Responses {
		if !reflect.DeepEqual(e.Response, env.Responses[i].Response) {
			return fmt.Errorf("%w: response operation changed", ErrPending)
		}
	}
	return nil
}

func validateRequest(req Request) error {
	if len(req.Responses) > 0 || req.IssueUID != "" {
		return pending("response delivery is not enabled")
	}
	if req.OperationID == "" || req.Endpoint == "" || req.Project == "" || (req.ReviewUUID == "" && len(req.Entries) > 0) || req.Teammate == "" {
		return fmt.Errorf("%w: incomplete request", ErrPending)
	}
	if req.Policy != "comments" && req.Policy != "summary" && req.Policy != "legacy" {
		return fmt.Errorf("%w: invalid policy", ErrPending)
	}
	if req.Policy == "legacy" && len(req.Entries) > 0 {
		return fmt.Errorf("%w: legacy policy has findings", ErrPending)
	}
	seen := map[string]bool{}
	for _, e := range req.Entries {
		if e.Selector == "" || seen[e.Selector] || e.ReviewUUID != req.ReviewUUID || e.DocumentHash == "" || e.Index < 0 || strings.TrimSpace(e.Body) == "" {
			return fmt.Errorf("%w: invalid finding", ErrPending)
		}
		seen[e.Selector] = true
	}
	for _, key := range []string{ActionKey, FingerprintKey, EnvelopeKey} {
		if _, ok := req.Create.Metadata[key]; ok {
			return fmt.Errorf("%w: reserved metadata", ErrPending)
		}
	}
	return nil
}

// Attribution is retained in full in local receipts. Its fixed digest lets the
// shared ledger detect edited attribution without an unbounded growth term.
func attributionHash(attribution kata.Attribution) string {
	data, err := json.Marshal(attribution, json.Deterministic(true))
	if err != nil {
		return ""
	}
	return hash(string(data))
}

func validReceiptHash(uid, digest string) bool {
	if uid == "" {
		return digest == ""
	}
	return len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == ""
}
