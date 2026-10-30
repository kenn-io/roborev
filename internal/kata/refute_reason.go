package kata

import (
	"fmt"
	"strings"
)

const refuteMarkerOpening = "<!-- roborev-refute:"

// CommentBody is the current decoded view of an untrusted native comment.
// Body on Comment/CommentResult retains the original bytes, including invalid
// markers. EvidenceValid qualifies the evidence, not the author's assertion.
type CommentBody struct {
	Reason        string `json:"reason"`
	ReasonState   string `json:"reason_state"`
	Evidence      string `json:"evidence"`
	EvidenceValid bool   `json:"evidence_valid"`
}

func validRefuteReason(reason string) bool {
	switch reason {
	case "false-positive", "spurious", "risk-accepted":
		return true
	}
	return false
}

// ValidateRefuteReason checks caller evidence before encoding. A nil reason
// preserves legacy intent; a present empty string is invalid. The reserved
// opening cannot be supplied by a caller, even when reason is omitted.
func ValidateRefuteReason(kind CommentKind, evidence string, reason *string) error {
	if strings.HasPrefix(evidence, refuteMarkerOpening) {
		return fmt.Errorf("%w: response evidence starts with a reserved refute marker", ErrRequestRejected)
	}
	if reason != nil && (kind != CommentRefute || !validRefuteReason(*reason)) {
		return fmt.Errorf("%w: --reason requires refute and false-positive, spurious or risk-accepted", ErrRequestRejected)
	}
	return nil
}

// EncodeRefuteReason freezes a marker after the response's disclosure policy
// has transformed its evidence. Omitted reasons leave the bytes unchanged.
func EncodeRefuteReason(kind CommentKind, evidence string, reason *string) (string, error) {
	if err := ValidateRefuteReason(kind, evidence, reason); err != nil {
		return "", err
	}
	if reason == nil {
		return evidence, nil
	}
	return refuteMarkerOpening + "v1 reason=" + *reason + " -->\n" + evidence, nil
}

// DecodeCommentBody recognizes only the exact byte-zero marker on a refute.
// Invalid markers expose no decoded evidence; the caller retains the raw body.
// Native and edited comments qualify again without any local operation state.
func DecodeCommentBody(kind CommentKind, body string) CommentBody {
	view := CommentBody{Reason: "unspecified", ReasonState: "absent", Evidence: body}
	if strings.HasPrefix(body, refuteMarkerOpening) {
		header, evidence, newline := strings.Cut(body, "\n")
		reason := strings.TrimPrefix(header, refuteMarkerOpening+"v1 reason=")
		reason = strings.TrimSuffix(reason, " -->")
		if kind != CommentRefute || !newline || !validRefuteReason(reason) || header != refuteMarkerOpening+"v1 reason="+reason+" -->" || strings.HasPrefix(evidence, refuteMarkerOpening) {
			return CommentBody{Reason: "unspecified", ReasonState: "invalid"}
		}
		view.Reason, view.ReasonState, view.Evidence = reason, "explicit", evidence
	}
	view.Evidence = evidenceWithoutRecoveryMetadata(view.Evidence)
	switch kind {
	case CommentConfirm, CommentRefute:
		view.EvidenceValid = EvidenceRunes(view.Evidence) >= MinEvidenceRunes
	case "", CommentReply, CommentSupersede:
		view.EvidenceValid = strings.TrimSpace(view.Evidence) != ""
	}
	return view
}
