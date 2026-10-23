package katapublish

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/kata"
)

// replayWindow is Kata v0.19.0's seven-day receipt lifetime
// (internal/daemon/handlers_issues.go:1469). After that interval only complete
// reads may establish delivery; a missing comment must never be reposted.
const replayWindow = 7 * 24 * time.Hour

type API interface {
	Endpoint() string
	ResolveProject(context.Context, string) (kata.Binding, error)
	ListProject(context.Context, string, kata.ListOpts) ([]kata.Issue, error)
	Show(context.Context, string) (kata.Issue, error)
	ShowComplete(context.Context, string) (kata.Issue, error)
	Create(context.Context, kata.CreateReq) (kata.CreateResult, error)
	Comment(context.Context, kata.CommentReq) (kata.CommentResult, error)
	MutateMetadata(context.Context, string, string, any, any) (kata.Issue, error)
}

func pending(reason string) error { return fmt.Errorf("%w: %s", ErrPending, reason) }

// deliveryError preserves useful classification without leaking a server's
// echoed payload into local diagnostics, logs or summary-policy metadata.
func deliveryError(err error) error {
	kinds := []error{kata.ErrDuplicateReply, kata.ErrIdempotencyMismatch, kata.ErrIssueClosed, kata.ErrRepliesUnsupported, kata.ErrEvidenceTooShort, kata.ErrAuthenticationRequired, kata.ErrConflict, kata.ErrIncompatible, kata.ErrInvalidResponse, kata.ErrNotFound, kata.ErrProjectNotFound, kata.ErrRequestRejected, kata.ErrUnreachable, kata.ErrRedirect}
	var classified error
	for _, kind := range kinds {
		if errors.Is(err, kind) {
			classified = kind
			break
		}
	}
	if apiErr, ok := kata.AsAPIError(err); ok {
		code := ""
		switch apiErr.Code {
		case "claim_denied", "federation_read_only", "metadata_guard_failed", "guard_failed", "read_only", "duplicate_reply", "idempotency_mismatch", "issue_closed", "comment_not_found":
			code = apiErr.Code
		}
		class := classified
		if class == nil {
			class = kata.ErrRequestRejected
		}
		safe := &kata.APIError{Status: apiErr.Status, Code: code, Class: class}
		return fmt.Errorf("%w: %w", ErrPending, safe)
	}
	if classified != nil {
		return fmt.Errorf("%w: %w", ErrPending, classified)
	}
	return pending("Kata request failed")
}

func decodeValue(value any) (Envelope, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return Envelope{}, pending("invalid ledger value")
	}
	return DecodeEnvelope(data)
}

// PublicationLedger validates a read-back issue against the original frozen
// publication. It never changes the issue, receipts or local handoff.
func PublicationLedger(issue kata.Issue, req Request, state State) (Envelope, error) {
	return readEnvelope(issue, req, state)
}

func readEnvelope(issue kata.Issue, req Request, state State) (Envelope, error) {
	fingerprint, err := Fingerprint(req)
	if err != nil {
		return Envelope{}, pending("invalid request encoding")
	}
	if issue.UID == "" || issue.UID != state.IssueUID || issue.ProjectUID != state.ProjectUID {
		return Envelope{}, pending("issue identity changed")
	}
	if issue.Metadata[ActionKey] != action(req, state.ProjectUID) || issue.Metadata[FingerprintKey] != fingerprint {
		return Envelope{}, pending("publication marker or fingerprint changed")
	}
	env, err := decodeValue(issue.Metadata[EnvelopeKey])
	if err != nil {
		return env, err
	}
	return env, env.validateRequest(req, state.ProjectUID)
}

func cloneState(state State) State {
	state.Receipts = maps.Clone(state.Receipts)
	if state.Receipts == nil {
		state.Receipts = map[string]Receipt{}
	}
	state.FirstAttempt = maps.Clone(state.FirstAttempt)
	if state.FirstAttempt == nil {
		state.FirstAttempt = map[string]time.Time{}
	}
	state.Done = false
	return state
}

// Deliver makes one resumable attempt. checkpoint must durably save State;
// failure stops before the next external write. This method never claims an
// issue and never changes a missing or mismatched ledger to make it fit.
func Deliver(ctx context.Context, api API, req Request, state State, checkpoint func(State) error) (State, error) {
	if len(req.Responses) > 0 {
		return deliverResponse(ctx, api, req, state, checkpoint)
	}
	state = cloneState(state)
	if err := validateRequest(req); err != nil {
		return state, err
	}
	if checkpoint == nil {
		return state, pending("durable checkpoint required")
	}
	if api.Endpoint() != req.Endpoint {
		return state, pending("destination endpoint changed")
	}
	resolved, err := api.ResolveProject(ctx, req.Project)
	if err != nil {
		return state, deliveryError(err)
	}
	if resolved.ProjectUID == "" || (req.ProjectUID != "" && req.ProjectUID != resolved.ProjectUID) || (state.ProjectUID != "" && state.ProjectUID != resolved.ProjectUID) {
		return state, pending("destination project changed")
	}
	state.ProjectUID = resolved.ProjectUID
	env, err := newEnvelope(req, state.ProjectUID)
	if err != nil {
		return state, pending("invalid request encoding")
	}
	// Both the pending envelope and a guarded update carrying its old and new
	// values must fit Kata's request. Nothing has been created at this point.
	if err = preflight(req, env); err != nil {
		return state, err
	}
	if err = checkpoint(state); err != nil {
		return state, pending("local checkpoint failed")
	}
	fingerprint, _ := Fingerprint(req)
	listed, err := api.ListProject(ctx, req.Project, kata.ListOpts{Status: "all", Unlimited: true})
	if err != nil {
		return state, deliveryError(err)
	}
	var found []kata.Issue
	for _, candidate := range listed {
		// Current API lists metadata. Older adapters may omit it, so obtain the
		// authoritative issue before deciding it lacks our action marker.
		if candidate.Metadata == nil {
			ref := candidate.QualifiedID
			if ref == "" {
				ref = req.Project + "#" + candidate.ShortID
			}
			candidate, err = api.Show(ctx, ref)
			if err != nil {
				return state, deliveryError(err)
			}
		}
		if candidate.Metadata[ActionKey] == action(req, state.ProjectUID) {
			found = append(found, candidate)
		}
	}
	if len(found) > 1 {
		return state, pending("ambiguous publication marker")
	}
	if len(found) == 1 {
		issue := found[0]
		if issue.Metadata[FingerprintKey] != fingerprint {
			return state, pending("publication fingerprint changed")
		}
		if state.IssueUID != "" && state.IssueUID != issue.UID {
			return state, pending("publication issue changed")
		}
		state.IssueUID = issue.UID
		state.Attribution = issue.Attribution
		state.IssueRef = issue.QualifiedID
		if state.IssueRef == "" {
			state.IssueRef = req.Project + "#" + issue.ShortID
		}
	} else {
		if state.IssueUID != "" {
			return state, pending("publication issue missing")
		}
		if expired(state.FirstAttempt["issue"]) {
			return state, pending("issue receipt window expired; no marker found")
		}
		if state.FirstAttempt["issue"].IsZero() {
			state.FirstAttempt["issue"] = time.Now()
		}
		if err = checkpoint(state); err != nil {
			return state, pending("local checkpoint failed")
		}
		create := req.Create
		create.Project = req.Project
		create.IdempotencyKey = action(req, state.ProjectUID)
		create.Metadata = maps.Clone(create.Metadata)
		if create.Metadata == nil {
			create.Metadata = map[string]any{}
		}
		create.Metadata[ActionKey] = action(req, state.ProjectUID)
		create.Metadata[FingerprintKey] = fingerprint
		create.Metadata[EnvelopeKey] = env
		receipt, createErr := api.Create(ctx, create)
		if createErr != nil {
			return state, deliveryError(createErr)
		}
		if receipt.IssueUID == "" || receipt.ProjectUID != state.ProjectUID {
			return state, pending("invalid issue receipt")
		}
		state.IssueUID = receipt.IssueUID
		state.IssueRef = req.Project + "#" + receipt.ShortID
		state.Attribution = receipt.Attribution
	}
	if err = checkpoint(state); err != nil {
		return state, pending("local checkpoint failed")
	}
	issue, err := api.ShowComplete(ctx, state.IssueRef)
	if err != nil {
		return state, deliveryError(err)
	}
	env, err = readEnvelope(issue, req, state)
	if err != nil {
		return state, err
	}
	if issue.CommentsTruncated {
		return state, pending("complete comment read unavailable")
	}
	if env.WriterEndpoint != api.Endpoint() {
		return state, pending("ledger writer endpoint unavailable")
	}
	for i, entry := range req.Entries {
		operation := entry.Selector
		body := entry.Body + "\n\n" + marker(req, operation)
		receipt, ok, findErr := findComment(issue, body, "", "", req.Teammate, env.Entries[i].ReceiptUID)
		if findErr != nil {
			return state, findErr
		}
		if !ok {
			// Closed issues can still prove delivery, but new findings wait until
			// the issue is reopened. Plain root comments do not enforce this.
			if issue.Status != "open" {
				return state, pending("issue is not open; finding delivery deferred")
			}
			if env.Entries[i].ReceiptUID != "" || state.Receipts[operation].UID != "" {
				return state, pending("recorded finding receipt missing")
			}
			if expired(state.FirstAttempt[operation]) {
				return state, pending("comment receipt window expired; no intact marker found")
			}
			if state.FirstAttempt[operation].IsZero() {
				state.FirstAttempt[operation] = time.Now()
			}
			if err = checkpoint(state); err != nil {
				return state, pending("local checkpoint failed")
			}
			posted, postErr := api.Comment(ctx, kata.CommentReq{Project: req.Project, Issue: state.IssueUID, Body: body, Teammate: req.Teammate, IdempotencyKey: hash(state.IssueUID + "\x00" + req.OperationID + "\x00" + operation + "\x00" + body)})
			if postErr != nil {
				return state, deliveryError(postErr)
			}
			if posted.UID == "" || posted.Kind != "" || posted.ReplyTo != "" || posted.Attribution.Teammate != req.Teammate {
				return state, pending("invalid comment receipt")
			}
			issue, err = api.ShowComplete(ctx, state.IssueRef)
			if err != nil {
				return state, deliveryError(err)
			}
			if issue.CommentsTruncated {
				return state, pending("complete comment read unavailable")
			}
			current, readErr := readEnvelope(issue, req, state)
			if readErr != nil {
				return state, readErr
			}
			// A different writer may have updated any entry while the comment was
			// in flight. Continue only from this latest validated envelope.
			env = current
			receipt, ok, err = findComment(issue, body, "", "", req.Teammate, posted.UID)
			if err != nil {
				return state, err
			}
			if !ok {
				return state, pending("comment receipt unavailable")
			}
		}
		if env.Entries[i].ReceiptUID != "" && env.Entries[i].AttributionHash != attributionHash(receipt.Attribution) {
			return state, pending("ledger receipt attribution changed")
		}
		if previous := state.Receipts[operation]; previous.UID != "" && (previous.UID != receipt.UID || previous.Attribution != receipt.Attribution) {
			return state, pending("local finding receipt changed")
		}
		state.Receipts[operation] = receipt
		if err = checkpoint(state); err != nil {
			return state, pending("local checkpoint failed")
		}
		if env.Entries[i].ReceiptUID != receipt.UID {
			next := env
			next.Entries = append([]LedgerEntry(nil), env.Entries...)
			next.Entries[i].ReceiptUID = receipt.UID
			next.Entries[i].Pending = ""
			next.Entries[i].AttributionHash = attributionHash(receipt.Attribution)
			if err = guardEnvelope(ctx, api, req, state, env, next); err != nil {
				return state, err
			}
			env = next
		}
	}
	// Response operations are part of the strict schema, but K2 does not send
	// them. A future caller cannot accidentally mark unimplemented work done.
	if len(req.Responses) > 0 {
		return state, pending("response delivery is not enabled")
	}
	state.Done = true
	if err = checkpoint(state); err != nil {
		state.Done = false
		return state, pending("local checkpoint failed")
	}
	return state, nil
}

func expired(first time.Time) bool {
	return !first.IsZero() && !time.Now().Before(first.Add(replayWindow))
}

func findComment(issue kata.Issue, body, target, kind, teammate, expectedUID string) (Receipt, bool, error) {
	markerLine := body[strings.LastIndex(body, "\n")+1:]
	var matches []kata.Comment
	for _, comment := range issue.Comments {
		// A quoted marker in a reply or another teammate's comment does not
		// identify this publication. Always validate a recorded receipt UID,
		// including when its kind or attribution has changed.
		knownReceipt := expectedUID != "" && comment.UID == expectedUID
		candidate := string(comment.Kind) == kind && comment.ReplyTo == target && comment.Teammate == teammate
		if knownReceipt || candidate && strings.Contains(comment.Body, markerLine) {
			matches = append(matches, comment)
		}
	}
	if len(matches) > 1 {
		return Receipt{}, false, pending("ambiguous comment marker")
	}
	if len(matches) == 0 {
		return Receipt{}, false, nil
	}
	comment := matches[0]
	if comment.UID == "" || comment.IssueUID != issue.UID || comment.Body != body || string(comment.Kind) != kind || comment.ReplyTo != target || comment.Teammate != teammate || comment.Author == "" || comment.EditedAt != nil || (expectedUID != "" && comment.UID != expectedUID) {
		return Receipt{}, false, pending("comment receipt does not match frozen request")
	}
	return Receipt{UID: comment.UID, Attribution: comment.Attribution}, true, nil
}

func guardEnvelope(ctx context.Context, api API, req Request, state State, previous, next Envelope) error {
	if err := validateGuard(previous, next); err != nil {
		return err
	}
	updated, err := api.MutateMetadata(ctx, state.IssueRef, EnvelopeKey, previous, next)
	if err != nil {
		return deliveryError(err)
	}
	got, err := readEnvelope(updated, req, state)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(got, json.Deterministic(true))
	b, _ := json.Marshal(next, json.Deterministic(true))
	if string(a) != string(b) {
		return pending("guarded ledger receipt changed")
	}
	return nil
}

func preflight(req Request, env Envelope) error {
	if err := preflightLifecycle(env); err != nil {
		return err
	}
	metadata := maps.Clone(req.Create.Metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[ActionKey] = action(req, env.ProjectUID)
	metadata[FingerprintKey] = env.Fingerprint
	metadata[EnvelopeKey] = env
	create := req.Create
	create.Project = req.Project
	create.Metadata = metadata
	if err := kata.ValidateCreateRequest(create); err != nil {
		return pending("publication exceeds Kata request size")
	}
	return nil
}

func validateGuard(previous, next Envelope) error {
	// Kata's helper counts the exact guard encoding, including the old
	// value carried as a JSON string. Its provider boundary owns the limit.
	if err := kata.ValidateMetadataMutation(EnvelopeKey, previous, next); err != nil {
		return pending("publication ledger exceeds Kata request size")
	}
	return nil
}

// Lookup recovers the currently visible issue and comment receipts without
// writing either Kata or local state. A comment whose receipt is still pending
// in the ledger is reported, but cannot make the publication complete.
func Lookup(ctx context.Context, api API, req Request, state State) (State, error) {
	state = cloneState(state)
	if err := validateRequest(req); err != nil {
		return state, err
	}
	if api.Endpoint() != req.Endpoint {
		return state, pending("destination endpoint changed")
	}
	project, err := api.ResolveProject(ctx, req.Project)
	if err != nil {
		return state, deliveryError(err)
	}
	if project.ProjectUID == "" || (req.ProjectUID != "" && req.ProjectUID != project.ProjectUID) || (state.ProjectUID != "" && state.ProjectUID != project.ProjectUID) {
		return state, pending("destination project changed")
	}
	state.ProjectUID = project.ProjectUID
	issues, err := api.ListProject(ctx, req.Project, kata.ListOpts{Status: "all", Unlimited: true})
	if err != nil {
		return state, deliveryError(err)
	}
	var matches []kata.Issue
	for _, issue := range issues {
		if issue.Metadata == nil {
			ref := issue.QualifiedID
			if ref == "" {
				ref = req.Project + "#" + issue.ShortID
			}
			issue, err = api.Show(ctx, ref)
			if err != nil {
				return state, deliveryError(err)
			}
		}
		if issue.Metadata[ActionKey] == action(req, state.ProjectUID) {
			matches = append(matches, issue)
		}
	}
	if len(matches) == 0 {
		return state, pending("publication issue missing")
	}
	if len(matches) != 1 {
		return state, pending("ambiguous publication marker")
	}
	issue := matches[0]
	if state.IssueUID != "" && state.IssueUID != issue.UID {
		return state, pending("publication issue changed")
	}
	state.IssueUID = issue.UID
	state.IssueRef = issue.QualifiedID
	if state.IssueRef == "" {
		state.IssueRef = req.Project + "#" + issue.ShortID
	}
	state.Attribution = issue.Attribution
	issue, err = api.ShowComplete(ctx, state.IssueRef)
	if err != nil {
		return state, deliveryError(err)
	}
	if issue.CommentsTruncated {
		return state, pending("complete comment read unavailable")
	}
	env, err := readEnvelope(issue, req, state)
	if err != nil {
		return state, err
	}
	done := true
	for i, entry := range req.Entries {
		body := entry.Body + "\n\n" + marker(req, entry.Selector)
		receipt, found, findErr := findComment(issue, body, "", "", req.Teammate, env.Entries[i].ReceiptUID)
		if findErr != nil {
			return state, findErr
		}
		if !found {
			if env.Entries[i].ReceiptUID != "" || state.Receipts[entry.Selector].UID != "" {
				return state, pending("recorded finding receipt missing")
			}
			done = false
			continue
		}
		if env.Entries[i].ReceiptUID != "" && env.Entries[i].AttributionHash != attributionHash(receipt.Attribution) {
			return state, pending("ledger receipt attribution changed")
		}
		if previous := state.Receipts[entry.Selector]; previous.UID != "" && (previous.UID != receipt.UID || previous.Attribution != receipt.Attribution) {
			return state, pending("local finding receipt changed")
		}
		state.Receipts[entry.Selector] = receipt
		if env.Entries[i].ReceiptUID != receipt.UID {
			done = false
		}
	}
	state.Done = done
	if !done {
		return state, pending("publication ledger has pending receipts")
	}
	return state, nil
}

// Every entry can independently be pending or receipted. Pick its largest
// representation separately for the guard's quoted old value and the new JSON
// object. This bounds all intermediate states, including mixed short/long
// finding bodies, before the first external write.
func preflightLifecycle(env Envelope) error {
	previous, next := env, env
	previous.Entries = append([]LedgerEntry(nil), env.Entries...)
	next.Entries = append([]LedgerEntry(nil), env.Entries...)
	for i, entry := range env.Entries {
		done := entry
		done.Pending = ""
		done.ReceiptUID = strings.Repeat("Z", kata.MaxUIDBytes)
		done.AttributionHash = strings.Repeat("f", 64)
		pendingJSON, err := json.Marshal(entry, json.Deterministic(true))
		if err != nil {
			return pending("invalid ledger encoding")
		}
		doneJSON, err := json.Marshal(done, json.Deterministic(true))
		if err != nil {
			return pending("invalid ledger encoding")
		}
		oldPending, _ := json.Marshal(string(pendingJSON))
		oldDone, _ := json.Marshal(string(doneJSON))
		if len(oldDone) > len(oldPending) {
			previous.Entries[i] = done
		}
		if len(doneJSON) > len(pendingJSON) {
			next.Entries[i] = done
		}
	}
	// Response bodies remain in both pending and receipted slots. The receipt
	// UID and attribution digest are their only growth, so reserve both before
	// registering an operation or sending its comment.
	previous.Responses = append([]LedgerResponse(nil), env.Responses...)
	next.Responses = append([]LedgerResponse(nil), env.Responses...)
	for i, response := range env.Responses {
		response.ReceiptUID = strings.Repeat("Z", kata.MaxUIDBytes)
		response.AttributionHash = strings.Repeat("f", 64)
		previous.Responses[i] = response
		next.Responses[i] = response
	}

	return validateGuard(previous, next)
}
