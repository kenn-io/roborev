// Package kataresponse validates explicit finding responses before either CLI
// or daemon contacts Kata. Delivery belongs to katapublish.
package kataresponse

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"strings"
	"uuid"

	"go.kenn.io/roborev/internal/findingid"
	"go.kenn.io/roborev/internal/kata"
)

// Request is the caller's immutable intent. Its fingerprint deliberately
// excludes destination discovery: identical retries reuse the stored capture.
type Request struct {
	JobID         int64   `json:"job_id,omitempty" doc:"Optional original job ID; otherwise resolve the immutable finding or comment target"`
	OperationID   string  `json:"operation_id"`
	FindingID     string  `json:"finding_id" doc:"Complete finding selector or Kata comment reference"`
	Kind          string  `json:"kind"`
	Body          string  `json:"body"`
	Reason        *string `json:"reason,omitempty" doc:"Optional refute classification: false-positive, spurious or risk-accepted; omission is unspecified"`
	Replacement   string  `json:"replacement,omitempty"`
	TargetComment string  `json:"target_comment,omitempty"`
}

// ValidateOperationID requires a nonzero UUID in its complete hyphenated form.
// Hexadecimal case does not change the operation identity.
func ValidateOperationID(value string) error {
	id, err := uuid.Parse(value) //nolint:forbidigo // Explicit operation ID text boundary.
	if err != nil || id == uuid.Nil() {
		return fmt.Errorf("operation ID must be a complete nonzero UUID")
	}
	canonical := id.String() //nolint:forbidigo // External operation UUID spelling comparison.
	if canonical != strings.ToLower(value) {
		return fmt.Errorf("operation ID must be a complete nonzero UUID")
	}
	return nil
}

// ValidateIdempotencyKey retains Kata v0.19.0's opaque key semantics:
// internal/daemon/handlers_comments.go locks and looks up the exact header
// string. It neither requires a UUID nor folds case.
func ValidateIdempotencyKey(value string) error {
	if value == "" {
		return fmt.Errorf("idempotency key must not be empty")
	}
	return nil
}

// Validate enforces the typed reply contract without changing kind or payload.
func (r Request) Validate() error {
	if r.JobID < 0 {
		return fmt.Errorf("response job ID must not be negative")
	}
	if err := ValidateIdempotencyKey(r.OperationID); err != nil {
		return err
	}
	if _, err := findingid.Parse(r.FindingID); err != nil && !kata.ValidCommentReference(r.FindingID) {
		return fmt.Errorf("response target must be a complete finding selector or comment reference")
	}
	switch r.Kind {
	case "reply", "confirm", "refute", "supersede":
	default:
		return fmt.Errorf("select --reply, --confirm, --refute or --supersede")
	}
	if err := kata.ValidateRefuteReason(kata.CommentKind(r.Kind), r.Body, r.Reason); err != nil {
		return err
	}
	if err := kata.ValidateEvidence(kata.CommentKind(r.Kind), r.Body); err != nil {
		return err
	}
	if strings.TrimSpace(r.Body) == "" {
		return fmt.Errorf("response evidence must not be empty")
	}
	if r.Kind == "supersede" {
		if _, err := findingid.Parse(r.Replacement); err != nil {
			return fmt.Errorf("supersede requires --replacement with a complete finding selector")
		}
	} else if r.Replacement != "" {
		return fmt.Errorf("--replacement requires supersede")
	}
	return nil
}

// Fingerprint names the exact payload, including explicit continuation target.
func (r Request) Fingerprint() (string, error) {
	raw, err := json.Marshal(r, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
