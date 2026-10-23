package kata

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MinEvidenceRunes is how many Unicode runes a confirm or refute needs after
// whitespace is normalized. Kata refuses fewer
// (internal/db/comment_reply_write.go:49 at v0.19.0).
const MinEvidenceRunes = 40

// Errors for comment operations. Each is distinct, so a caller can leave a
// delivery unresolved for the right reason. A Kata rejection carries its
// Class too: errors.Is(err, ErrConflict) holds for a duplicate reply, an
// idempotency mismatch, and a closed issue.
var (
	// ErrRepliesUnsupported means the server's API schema predates typed
	// replies, which it would store as plain comments.
	ErrRepliesUnsupported = errors.New("kata: server does not store typed replies (needs API schema " + RepliesAPISchemaVersion + " or later)")
	// ErrDuplicateReply means the actor and teammate already posted a reply of
	// this kind to the comment. The existing comment is on the CommentError.
	ErrDuplicateReply = errors.New("kata: a reply of this kind already exists for this actor and teammate")
	// ErrIdempotencyMismatch means the idempotency key was already used for a
	// different request. Nothing is posted and the key is not reusable.
	ErrIdempotencyMismatch = errors.New("kata: the idempotency key matched a different request")
	// ErrIssueClosed means the issue is closed, and Kata stores typed replies
	// only on open issues.
	ErrIssueClosed = errors.New("kata: the issue is closed, so a typed reply cannot be added")
	// ErrCommentNotFound means a comment a request names, such as the one a
	// reply answers or a thread's root, does not exist.
	ErrCommentNotFound = errors.New("kata: comment not found")
	// ErrEvidenceTooShort means a confirm or refute has fewer than
	// MinEvidenceRunes runes after whitespace is normalized. The kind is never
	// changed to get past it.
	ErrEvidenceTooShort = errors.New("kata: a confirm or refute needs more evidence")
)

// CommentError is a comment request that was rejected, classified by Kata's
// error code or, for evidence, before anything was sent.
type CommentError struct {
	// Kind is one of the comment errors above.
	Kind error
	// ExistingUID and ExistingHandle name the comment a duplicate reply would
	// have repeated. They are set only for ErrDuplicateReply.
	ExistingUID    string
	ExistingHandle string

	detail string
	cause  error
}

// Error says what was wrong. A local check supplies its own detail; a Kata
// rejection has none, so its text is Kata's own, with the HTTP status and code.
func (e *CommentError) Error() string {
	switch {
	case e.detail != "":
		return e.Kind.Error() + ": " + e.detail
	case e.cause != nil:
		return e.cause.Error()
	}
	return e.Kind.Error()
}

// Unwrap lets errors.Is match both the comment error and Kata's own class.
func (e *CommentError) Unwrap() []error {
	out := []error{e.Kind}
	if e.cause != nil {
		out = append(out, e.cause)
	}
	return out
}

// classifyCommentError names a Kata rejection of a comment request. Errors that
// are not one of those pass through unchanged.
func classifyCommentError(err error) error {
	apiErr, ok := AsAPIError(err)
	if !ok {
		return err
	}
	switch apiErr.Code {
	case "duplicate_reply":
		return &CommentError{Kind: ErrDuplicateReply, ExistingUID: apiErr.ExistingUID, ExistingHandle: apiErr.ExistingHandle, cause: err}
	case "idempotency_mismatch":
		return &CommentError{Kind: ErrIdempotencyMismatch, cause: err}
	case "issue_closed":
		return &CommentError{Kind: ErrIssueClosed, cause: err}
	case "comment_not_found":
		return &CommentError{Kind: ErrCommentNotFound, cause: err}
	}
	return err
}

// CommentKind is a typed reply kind.
type CommentKind string

// Typed reply kinds. An empty kind is a plain comment.
const (
	CommentReply     CommentKind = "reply"
	CommentConfirm   CommentKind = "confirm"
	CommentRefute    CommentKind = "refute"
	CommentSupersede CommentKind = "supersede"
)

func (k CommentKind) valid() bool {
	switch k {
	case CommentReply, CommentConfirm, CommentRefute, CommentSupersede:
		return true
	}
	return false
}

// EvidenceRunes counts the Unicode runes of body after whitespace is
// normalized: runs of space become one space and the ends are trimmed. This is
// the count Kata applies to a confirm or refute.
func EvidenceRunes(body string) int {
	return utf8.RuneCountInString(strings.Join(strings.Fields(body), " "))
}

// ValidateEvidence checks the evidence rule for a reply of kind: a confirm or
// refute needs MinEvidenceRunes runes of evidence, and the other kinds need
// none. It never changes the kind. The error is ErrEvidenceTooShort and
// ErrRequestRejected.
func ValidateEvidence(kind CommentKind, body string) error {
	if kind != CommentConfirm && kind != CommentRefute {
		return nil
	}
	if n := EvidenceRunes(body); n < MinEvidenceRunes {
		return &CommentError{
			Kind:   ErrEvidenceTooShort,
			detail: fmt.Sprintf("a %s needs at least %d characters after whitespace is normalized, and this has %d", kind, MinEvidenceRunes, n),
			cause:  ErrRequestRejected,
		}
	}
	return nil
}

// CommentReq is one comment or typed reply. Kind and ReplyTo (a comment UID)
// go together. Teammate defaults to the configured teammate.
type CommentReq struct {
	Project        string // optional; resolved like Create when empty
	Issue          string // short ID or UID within the project
	Body           string
	Kind           CommentKind
	ReplyTo        string
	Teammate       string
	IdempotencyKey string
}

// CommentResult reports what Kata stored. Changed is false when Kata replayed
// an earlier request with the same key and body, which is a success.
type CommentResult struct {
	UID         string
	Body        string
	EditedAt    *time.Time
	Changed     bool
	Kind        CommentKind
	ReplyTo     string
	Attribution Attribution
}

// Comment is one comment of an issue as Kata reports it.
type Comment struct {
	UID string
	// Handle is Kata's short reference for the comment, such as c:ab12cd.
	Handle string
	// IssueUID is the issue the comment was posted on. A reply can sit on a
	// different issue of the same project than the comment it answers.
	IssueUID  string
	Body      string
	CreatedAt time.Time
	// EditedAt is nil for a comment that was never edited.
	EditedAt *time.Time
	// ReplyTo is the UID of the comment this one answers, and Kind how. Both
	// are empty for a plain comment.
	ReplyTo string
	Kind    CommentKind
	Attribution
}

// ThreadReq selects the comments beneath one comment. Issue is any ref Show
// accepts. Root is the UID of the comment whose replies to read.
type ThreadReq struct {
	Issue string
	Root  string
}

// Thread is a comment and every reply beneath it, oldest first. Kata returns at
// most 50 comments of a thread, keeping the root; Truncated says it left some
// out, so a thread that is truncated is never a complete record.
type Thread struct {
	Comments  []Comment
	Truncated bool
}
