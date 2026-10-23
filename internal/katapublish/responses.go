package katapublish

import (
	"context"
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kataresponse"
)

// ContinuationTarget resolves a full UID or handle only within a complete
// finding thread. Missing ancestors and cycles are unresolved, never retargeted.
func ContinuationTarget(issue kata.Issue, root, ref string) (string, error) {
	if issue.CommentsTruncated {
		return "", pending("complete comment read unavailable")
	}
	comments := map[string]kata.Comment{}
	target := ""
	for _, comment := range issue.Comments {
		if _, ok := comments[comment.UID]; ok {
			return "", pending("duplicate comment identity")
		}
		comments[comment.UID] = comment
		if comment.UID == ref || (comment.Handle != "" && comment.Handle == ref) {
			if target != "" {
				return "", pending("ambiguous continuation")
			}
			target = comment.UID
		}
	}
	if target == "" {
		return "", pending("continuation comment unavailable")
	}
	current := target
	seen := map[string]bool{}
	for current != "" && !seen[current] {
		seen[current] = true
		comment, ok := comments[current]
		if !ok || comment.IssueUID != issue.UID {
			return "", pending("continuation lineage unavailable")
		}
		if current == root {
			if comment.ReplyTo != "" || comment.Kind != "" {
				return "", pending("finding root changed")
			}
			return target, nil
		}
		current = comment.ReplyTo
	}
	return "", pending("continuation is outside the finding thread")
}

func responseEnvelope(issue kata.Issue, req Request, state State) (Envelope, error) {
	if issue.UID != req.IssueUID || issue.UID != state.IssueUID || issue.ProjectUID != req.ProjectUID {
		return Envelope{}, pending("response destination changed")
	}
	parent := req
	parent.OperationID = req.PublicationOperationID
	parent.ProjectUID = req.PublicationProjectUID
	if issue.Metadata[ActionKey] != action(parent, req.ProjectUID) || issue.Metadata[FingerprintKey] != req.PublicationFingerprint {
		return Envelope{}, pending("publication identity changed")
	}
	env, err := decodeValue(issue.Metadata[EnvelopeKey])
	if err != nil {
		return env, err
	}
	if env.WriterEndpoint != req.Endpoint || env.ProjectUID != req.ProjectUID || env.OperationID != req.PublicationOperationID || env.Fingerprint != req.PublicationFingerprint || env.ReviewUUID != req.ReviewUUID || env.Policy != req.Policy {
		return env, pending("finding ledger changed")
	}
	if len(req.PublicationEntries) == 0 || !reflect.DeepEqual(env.Entries, req.PublicationEntries) {
		return env, pending("publication finding manifest changed")
	}
	response := req.Responses[0]
	selected := req.Entries[0]
	var root *LedgerEntry
	for _, entry := range env.Entries {
		if entry.Selector == selected.Selector {
			copy := entry
			root = &copy
		}
	}
	if root == nil || root.ReviewUUID != selected.ReviewUUID || root.DocumentHash != selected.DocumentHash || root.Index != selected.Index || root.BodyHash != hash(selected.Body) || root.ReceiptUID != response.Root || root.Pending != "" {
		return env, pending("finding root mapping changed")
	}
	if issue.CommentsTruncated {
		return env, pending("complete comment read unavailable")
	}
	if root.AttributionHash != req.RootAttributionHash {
		return env, pending("finding root attribution changed")
	}
	rootTeammate := ""
	for _, comment := range issue.Comments {
		if comment.UID == response.Root {
			rootTeammate = comment.Teammate
		}
	}
	receipt, ok, err := findComment(issue, selected.Body+"\n\n"+marker(parent, selected.Selector), "", "", rootTeammate, response.Root)
	if err != nil {
		return env, err
	}
	if !ok || attributionHash(receipt.Attribution) != req.RootAttributionHash {
		return env, pending("finding root receipt unavailable")
	}
	if _, err = ContinuationTarget(issue, response.Root, response.Target); err != nil {
		return env, err
	}
	for _, slot := range env.Responses {
		if slot.OperationID == response.OperationID && !reflect.DeepEqual(slot.Response, response) {
			return env, pending("response fingerprint or payload changed")
		}
	}
	return env, nil
}

func guardResponse(ctx context.Context, api API, req Request, state State, previous, next Envelope) error {
	if err := validateGuard(previous, next); err != nil {
		return err
	}
	_, err := api.MutateMetadata(ctx, state.IssueRef, EnvelopeKey, previous, next)
	if err != nil {
		return deliveryError(err)
	}
	issue, err := api.ShowComplete(ctx, state.IssueRef)
	if err != nil {
		return deliveryError(err)
	}
	got, err := responseEnvelope(issue, req, state)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(got, json.Deterministic(true))
	b, _ := json.Marshal(next, json.Deterministic(true))
	if string(a) != string(b) {
		return pending("guarded response ledger changed")
	}
	return nil
}

// deliverResponse appends and completes one response in the existing publication
// envelope. It never creates an issue and never replaces publication identity.
func deliverResponse(ctx context.Context, api API, req Request, state State, checkpoint func(State) error) (State, error) {
	state = cloneState(state)
	if len(req.Responses) != 1 || len(req.Entries) != 1 || req.IssueUID == "" || req.ProjectUID == "" || req.PublicationOperationID == "" || req.PublicationFingerprint == "" || req.RootAttributionHash == "" || (req.Policy != "summary" && req.Policy != "comments") {
		return state, pending("incomplete frozen response mapping")
	}
	response := req.Responses[0]
	if err := kataresponse.ValidateIdempotencyKey(req.OperationID); err != nil {
		return state, pending("invalid response operation ID")
	}
	if response.OperationID != req.OperationID || response.Root == "" || response.Target == "" || response.Fingerprint == "" || response.Teammate == "" || strings.TrimSpace(response.Body) == "" {
		return state, pending("incomplete response operation")
	}
	switch response.Kind {
	case "reply", "confirm", "refute", "supersede":
	default:
		return state, pending("invalid response kind")
	}
	if err := kata.ValidateEvidence(kata.CommentKind(response.Kind), response.Body); err != nil {
		return state, deliveryError(err)
	}
	if checkpoint == nil {
		return state, pending("durable checkpoint required")
	}
	if api.Endpoint() != req.Endpoint {
		return state, pending("destination endpoint changed")
	}
	binding, err := api.ResolveProject(ctx, req.Project)
	if err != nil {
		return state, deliveryError(err)
	}
	if binding.ProjectUID != req.ProjectUID || (state.ProjectUID != "" && state.ProjectUID != req.ProjectUID) || (state.IssueUID != "" && state.IssueUID != req.IssueUID) {
		return state, pending("destination project or issue changed")
	}
	state.ProjectUID = req.ProjectUID
	state.IssueUID = req.IssueUID
	state.IssueRef = req.Project + "#" + req.IssueUID
	if err = checkpoint(state); err != nil {
		return state, pending("local checkpoint failed")
	}
	issue, err := api.ShowComplete(ctx, state.IssueRef)
	if err != nil {
		return state, deliveryError(err)
	}
	env, err := responseEnvelope(issue, req, state)
	if err != nil {
		return state, err
	}
	slot := -1
	for i, entry := range env.Responses {
		if entry.OperationID == req.OperationID {
			slot = i
		}
	}
	if slot < 0 {
		if !state.FirstAttempt[req.OperationID].IsZero() || state.Receipts[req.OperationID].UID != "" || state.Receipts["duplicate:"+req.OperationID].UID != "" {
			return state, pending("registered response operation missing")
		}
		next := env
		next.Responses = append(append([]LedgerResponse{}, env.Responses...), LedgerResponse{Response: response})
		if err = preflightLifecycle(next); err != nil {
			return state, err
		}
		if err = guardResponse(ctx, api, req, state, env, next); err != nil {
			return state, err
		}
		env = next
		slot = len(env.Responses) - 1
	}
	if err = preflightLifecycle(env); err != nil {
		return state, err
	}
	body := response.Body + "\n\n" + marker(req, req.OperationID)
	receipt, ok, err := findComment(issue, body, response.Target, response.Kind, response.Teammate, env.Responses[slot].ReceiptUID)
	if err != nil {
		return state, err
	}
	if !ok {
		if env.Responses[slot].ReceiptUID != "" || state.Receipts[req.OperationID].UID != "" {
			return state, pending("recorded response receipt missing")
		}
		if expired(state.FirstAttempt[req.OperationID]) {
			return state, pending("response receipt window expired; no intact marker found")
		}
		if state.FirstAttempt[req.OperationID].IsZero() {
			state.FirstAttempt[req.OperationID] = time.Now()
		}
		if err = checkpoint(state); err != nil {
			return state, pending("local checkpoint failed")
		}
		posted, postErr := api.Comment(ctx, kata.CommentReq{Project: req.Project, Issue: req.IssueUID, Body: body, Kind: kata.CommentKind(response.Kind), ReplyTo: response.Target, Teammate: response.Teammate, IdempotencyKey: hash(req.IssueUID + "\x00" + req.OperationID + "\x00" + body)})
		if postErr != nil {
			var duplicate *kata.CommentError
			if errors.As(postErr, &duplicate) && errors.Is(postErr, kata.ErrDuplicateReply) {
				ref := duplicate.ExistingUID
				if ref == "" {
					ref = duplicate.ExistingHandle
				}
				if safeCommentRef(ref) {
					state.Receipts["duplicate:"+req.OperationID] = Receipt{UID: ref}
				}
			}
			return state, deliveryError(postErr)
		}
		if posted.UID == "" || string(posted.Kind) != response.Kind || posted.ReplyTo != response.Target || posted.Attribution.Teammate != response.Teammate {
			return state, pending("invalid response receipt")
		}
		issue, err = api.ShowComplete(ctx, state.IssueRef)
		if err != nil {
			return state, deliveryError(err)
		}
		env, err = responseEnvelope(issue, req, state)
		if err != nil {
			return state, err
		}
		slot = -1
		for i, entry := range env.Responses {
			if entry.OperationID == req.OperationID {
				slot = i
			}
		}
		if slot < 0 {
			return state, pending("registered response operation missing")
		}
		receipt, ok, err = findComment(issue, body, response.Target, response.Kind, response.Teammate, posted.UID)
		if err != nil {
			return state, err
		}
		if !ok {
			return state, pending("response receipt unavailable")
		}
	}
	if env.Responses[slot].ReceiptUID != "" && env.Responses[slot].AttributionHash != attributionHash(receipt.Attribution) {
		return state, pending("response attribution changed")
	}
	if previous := state.Receipts[req.OperationID]; previous.UID != "" && previous != receipt {
		return state, pending("local response receipt changed")
	}
	state.Receipts[req.OperationID] = receipt
	delete(state.Receipts, "duplicate:"+req.OperationID)
	if err = checkpoint(state); err != nil {
		return state, pending("local checkpoint failed")
	}
	if env.Responses[slot].ReceiptUID != receipt.UID {
		next := env
		next.Responses = append([]LedgerResponse{}, env.Responses...)
		next.Responses[slot].ReceiptUID = receipt.UID
		next.Responses[slot].AttributionHash = attributionHash(receipt.Attribution)
		if err = guardResponse(ctx, api, req, state, env, next); err != nil {
			return state, err
		}
	}
	state.Done = true
	if err = checkpoint(state); err != nil {
		state.Done = false
		return state, pending("local checkpoint failed")
	}
	return state, nil
}

// Provider text is never a diagnostic. Existing comment references contain only
// reference characters, so a malicious error cannot smuggle a body into status.
func safeCommentRef(ref string) bool {
	return (len(ref) == 26 || strings.HasPrefix(ref, "c:")) && kata.ValidCommentReference(ref)
}

// AttributionFingerprint pins the actual root writer without copying arbitrary
// provider actor names into the response envelope.
func AttributionFingerprint(attribution kata.Attribution) string { return attributionHash(attribution) }

// PublicationManifest freezes every original root mapping from the completed
// local publication. A response cannot silently accept edits to another root.
func PublicationManifest(req Request, state State) ([]LedgerEntry, error) {
	env, err := newEnvelope(req, state.ProjectUID)
	if err != nil {
		return nil, err
	}
	for i := range env.Entries {
		receipt := state.Receipts[env.Entries[i].Selector]
		if receipt.UID == "" {
			return nil, pending("publication root receipt unavailable")
		}
		env.Entries[i].Pending = ""
		env.Entries[i].ReceiptUID = receipt.UID
		env.Entries[i].AttributionHash = attributionHash(receipt.Attribution)
	}
	return env.Entries, nil
}
