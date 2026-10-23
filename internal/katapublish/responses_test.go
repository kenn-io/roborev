package katapublish

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
)

type responseAPI struct {
	*fakeAPI
	loseResponse     bool
	loseGuard        bool
	loseGuardAt      int
	appendConcurrent bool
	duplicate        bool
}

func TestResponseCaptureFieldsPreservePublicationEncoding(t *testing.T) {
	// This is K2's published request wire shape, before response-only captures.
	type publicationRequest struct {
		CredentialFingerprint string
		IssueUID              string
		OperationID           string
		Endpoint              string
		Project               string
		ProjectUID            string
		Policy                string
		ReviewUUID            string
		Teammate              string
		Create                kata.CreateReq
		Entries               []Entry
		Responses             []Response
	}
	req := fixtureRequest()
	old := publicationRequest{req.CredentialFingerprint, req.IssueUID, req.OperationID, req.Endpoint, req.Project, req.ProjectUID, req.Policy, req.ReviewUUID, req.Teammate, req.Create, req.Entries, req.Responses}
	before, err := json.Marshal(old, json.Deterministic(true))
	require.NoError(t, err)
	after, err := json.Marshal(req, json.Deterministic(true))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

// The real metadata mutation response does not include comments. Receipt
// validation must perform a complete read after a successful guarded write.
func (f *responseAPI) MutateMetadata(ctx context.Context, ref, key string, previous, value any) (kata.Issue, error) {
	issue, err := f.fakeAPI.MutateMetadata(ctx, ref, key, previous, value)
	issue.Comments = nil
	if err == nil && f.appendConcurrent {
		f.appendConcurrent = false
		env, decodeErr := decodeValue(f.issue.Metadata[key])
		if decodeErr != nil {
			return kata.Issue{}, decodeErr
		}
		env.Responses = append(env.Responses, LedgerResponse{OperationID: "concurrent-operation", Fingerprint: "other", Root: f.issue.Comments[0].UID, Target: f.issue.Comments[0].UID, Kind: "reply", Teammate: "other", Body: "Other writer's progress."})
		f.issue.Metadata[key] = env
	}
	if err == nil && (f.loseGuard || f.loseGuardAt == f.guards) {
		f.loseGuard = false
		return kata.Issue{}, errors.New("guard acknowledgement lost")
	}
	return issue, err
}

func (f *responseAPI) Comment(ctx context.Context, req kata.CommentReq) (kata.CommentResult, error) {
	if req.Kind == "" {
		result, err := f.fakeAPI.Comment(ctx, req)
		if err == nil && f.posts > 1 {
			result.UID = fmt.Sprintf("P%025d", f.posts)
			f.issue.Comments[len(f.issue.Comments)-1].UID = result.UID
		}
		return result, err
	}
	f.posts++
	if f.duplicate {
		return kata.CommentResult{}, &kata.CommentError{Kind: kata.ErrDuplicateReply, ExistingUID: "01j00000000000000000000001"}
	}
	if f.postErr != nil {
		return kata.CommentResult{}, f.postErr
	}
	comment := kata.Comment{UID: fmt.Sprintf("R%025d", f.posts), IssueUID: f.issue.UID, Body: req.Body, Kind: req.Kind, ReplyTo: req.ReplyTo, Author: "service", Teammate: req.Teammate}
	f.issue.Comments = append(f.issue.Comments, comment)
	if f.loseResponse {
		return kata.CommentResult{}, errors.New("response lost")
	}
	return kata.CommentResult{UID: comment.UID, Kind: comment.Kind, ReplyTo: comment.ReplyTo, Attribution: comment.Attribution}, nil
}

func responseFixture(t *testing.T) (*responseAPI, Request, State) {
	t.Helper()
	return responseFixtureFrom(t, fixtureRequest())
}

func responseFixtureFrom(t *testing.T, publication Request) (*responseAPI, Request, State) {
	t.Helper()
	api := &responseAPI{fakeAPI: &fakeAPI{}}
	state, err := Deliver(t.Context(), api, publication, State{}, func(State) error { api.checkpointed = true; return nil })
	require.NoError(t, err)
	fingerprint, err := Fingerprint(publication)
	require.NoError(t, err)
	req := publication
	req.PublicationEntries, err = PublicationManifest(publication, state)
	require.NoError(t, err)
	req.PublicationOperationID = publication.OperationID
	req.PublicationFingerprint = fingerprint
	req.PublicationProjectUID = publication.ProjectUID
	req.OperationID = uuid.New().String()
	req.IssueUID = state.IssueUID
	req.ProjectUID = state.ProjectUID //nolint:forbidigo // Synthetic daemon/provider operation and review UUID text boundaries.
	req.Teammate = "reviewer"
	req.RootAttributionHash = attributionHash(state.Receipts[req.Entries[0].Selector].Attribution)
	req.Responses = []Response{{OperationID: req.OperationID, Fingerprint: "intent-fingerprint", Root: state.Receipts[req.Entries[0].Selector].UID, Target: state.Receipts[req.Entries[0].Selector].UID, Body: "The caller validates this value before reaching the path.", Kind: "refute", Teammate: req.Teammate}}
	return api, req, State{}
}

func TestResponseDeliveryPreservesPublicationAndRecoversLostReceipt(t *testing.T) {
	api, req, state := responseFixture(t)
	before, err := decodeValue(api.issue.Metadata[EnvelopeKey])
	require.NoError(t, err)
	api.loseResponse = true
	state, err = Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.ErrorIs(t, err, ErrPending)
	assert.False(t, state.Done)
	api.loseResponse = false
	state, err = Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.NoError(t, err)
	assert.True(t, state.Done)
	assert.Equal(t, 1, api.creates)
	assert.Equal(t, 2, api.posts, "one root and one response despite a lost receipt")
	after, err := decodeValue(api.issue.Metadata[EnvelopeKey])
	require.NoError(t, err)
	assert.Equal(t, before.OperationID, after.OperationID)
	assert.Equal(t, before.Fingerprint, after.Fingerprint)
	assert.Equal(t, before.Entries, after.Entries)
	require.Len(t, after.Responses, 1)
	assert.NotEmpty(t, after.Responses[0].ReceiptUID)
	_, err = Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.NoError(t, err)
	assert.Equal(t, 2, api.posts)
}

func TestResponseDuplicateReplyRemainsPending(t *testing.T) {
	api, req, state := responseFixture(t)
	api.duplicate = true
	state, err := Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.ErrorIs(t, err, kata.ErrDuplicateReply)
	assert.False(t, state.Done)
	assert.Equal(t, "01j00000000000000000000001", state.Receipts["duplicate:"+req.OperationID].UID)
	assert.Empty(t, state.Receipts[req.OperationID].UID)
}

func TestResponseMissingPreviouslySentSlotRemainsPending(t *testing.T) {
	api, req, state := responseFixture(t)
	api.loseResponse = true
	state, err := Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.ErrorIs(t, err, ErrPending)
	env, err := decodeValue(api.issue.Metadata[EnvelopeKey])
	require.NoError(t, err)
	require.Len(t, env.Responses, 1)
	env.Responses = []LedgerResponse{}
	api.issue.Metadata[EnvelopeKey] = env
	api.loseResponse = false
	guards, posts := api.guards, api.posts
	state, err = Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.ErrorIs(t, err, ErrPending)
	assert.False(t, state.Done)
	assert.Equal(t, guards, api.guards)
	assert.Equal(t, posts, api.posts)
}

func TestResponseRejectsChangedUnrelatedPublicationRoot(t *testing.T) {
	publication := fixtureRequest()
	other := publication.Entries[0]
	other.Selector = "another-selector"
	other.Index++
	other.Body = "Another original finding"
	publication.Entries = append(publication.Entries, other)
	api, req, state := responseFixtureFrom(t, publication)
	req.Entries = req.Entries[:1]
	env, err := decodeValue(api.issue.Metadata[EnvelopeKey])
	require.NoError(t, err)
	require.Len(t, env.Entries, 2)
	env.Entries = env.Entries[:1]
	api.issue.Metadata[EnvelopeKey] = env
	posts, guards := api.posts, api.guards
	state, err = Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.ErrorIs(t, err, ErrPending)
	assert.False(t, state.Done)
	assert.Equal(t, posts, api.posts)
	assert.Equal(t, guards, api.guards)
}

func TestResponseRefusesShortEvidenceBeforeRemoteMutation(t *testing.T) {
	for _, kind := range []string{"confirm", "refute"} {
		t.Run(kind, func(t *testing.T) {
			api, req, state := responseFixture(t)
			req.Responses[0].Kind = kind
			req.Responses[0].Body = strings.Repeat("界", 39)
			guards, posts := api.guards, api.posts
			state, err := Deliver(t.Context(), api, req, state, func(State) error { return nil })
			require.ErrorIs(t, err, kata.ErrEvidenceTooShort)
			assert.False(t, state.Done)
			assert.Equal(t, kind, req.Responses[0].Kind)
			assert.Equal(t, guards, api.guards)
			assert.Equal(t, posts, api.posts)
		})
	}
}

func TestResponseErrorsNeverEchoProviderContent(t *testing.T) {
	api, req, state := responseFixture(t)
	api.postErr = errors.New("confidential-sentinel")
	state, err := Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "confidential-sentinel")
	assert.False(t, state.Done)
}

func TestContinuationRequiresRootLineage(t *testing.T) {
	issue := kata.Issue{UID: "issue", Comments: []kata.Comment{
		{UID: "root", IssueUID: "issue"},
		{UID: "reply", Handle: "c:abc123", IssueUID: "issue", ReplyTo: "root", Kind: kata.CommentReply},
		{UID: "other", IssueUID: "issue"},
		{UID: "cycle", IssueUID: "issue", ReplyTo: "cycle", Kind: kata.CommentReply},
	}}
	got, err := ContinuationTarget(issue, "root", "c:abc123")
	require.NoError(t, err)
	assert.Equal(t, "reply", got)
	for _, ref := range []string{"other", "cycle", "missing"} {
		_, err := ContinuationTarget(issue, "root", ref)
		require.Error(t, err)
	}
	issue.CommentsTruncated = true
	_, err = ContinuationTarget(issue, "root", "reply")
	require.Error(t, err)
}

// Appending any unrelated comment must never change the selected continuation
// into an accepted descendant. Arbitrary target strings cannot bypass lineage.
func FuzzContinuationLineage(f *testing.F) {
	f.Add("other", "root")
	f.Add("", "missing")
	f.Add("cycle", "cycle")
	f.Fuzz(func(t *testing.T, uid, parent string) {
		if uid == "root" || uid == "" {
			return
		}
		issue := kata.Issue{UID: "issue", Comments: []kata.Comment{{UID: "root", IssueUID: "issue"}, {UID: uid, IssueUID: "issue", ReplyTo: parent, Kind: kata.CommentReply}}}
		got, err := ContinuationTarget(issue, "root", uid)
		if parent == "root" {
			require.NoError(t, err)
			assert.Equal(t, uid, got)
		} else {
			require.Error(t, err)
		}
	})
}

func TestResponseKindsKeepFrozenRoot(t *testing.T) {
	for _, kind := range []string{"reply", "confirm", "refute", "supersede"} {
		t.Run(kind, func(t *testing.T) {
			api, req, state := responseFixture(t)
			req.Responses[0].Kind = kind
			if kind == "supersede" {
				req.Responses[0].Body += " Replacement: f:replacement"
			}
			state, err := Deliver(t.Context(), api, req, state, func(State) error { return nil })
			require.NoError(t, err)
			assert.True(t, state.Done)
			assert.Equal(t, req.Responses[0].Root, api.issue.Comments[1].ReplyTo)
			assert.Equal(t, kata.CommentKind(kind), api.issue.Comments[1].Kind)
		})
	}
}

func TestResponseExplicitContinuation(t *testing.T) {
	api, req, state := responseFixture(t)
	state, err := Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.NoError(t, err)
	target := state.Receipts[req.OperationID].UID
	req.OperationID = uuid.New().String()
	req.Responses[0].OperationID = req.OperationID
	req.Responses[0].Fingerprint = "continuation"
	req.Responses[0].Target = target //nolint:forbidigo // Synthetic daemon/provider operation and review UUID text boundaries.
	state, err = Deliver(t.Context(), api, req, State{}, func(State) error { return nil })
	require.NoError(t, err)
	assert.True(t, state.Done)
	assert.Equal(t, target, api.issue.Comments[2].ReplyTo)
}

func TestResponseRecoversLostRegistrationAcknowledgement(t *testing.T) {
	api, req, state := responseFixture(t)
	api.loseGuard = true
	state, err := Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.ErrorIs(t, err, ErrPending)
	assert.Equal(t, 1, api.posts)
	state, err = Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.NoError(t, err)
	assert.True(t, state.Done)
	assert.Equal(t, 2, api.posts)
	assert.Equal(t, 1, api.creates)
}

func TestResponsePreservesInitiallyUnresolvedPublicationMarker(t *testing.T) {
	publication := fixtureRequest()
	publication.ProjectUID = ""
	api, req, state := responseFixtureFrom(t, publication)
	// fixtureRequest has an empty ProjectUID: K2 resolves and checkpoints the
	// full UID while its immutable request/root marker retain the original empty
	// input. A response must use both identities in their respective roles.
	assert.Empty(t, publication.ProjectUID)
	assert.NotEmpty(t, req.ProjectUID)
	assert.Empty(t, req.PublicationProjectUID)
	state, err := Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.NoError(t, err)
	assert.True(t, state.Done)
}

func TestResponseRecoversLostReceiptLedgerAcknowledgement(t *testing.T) {
	api, req, state := responseFixture(t)
	api.loseGuardAt = api.guards + 2
	state, err := Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.ErrorIs(t, err, ErrPending)
	assert.False(t, state.Done)
	state, err = Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.NoError(t, err)
	assert.True(t, state.Done)
	assert.Equal(t, 2, api.posts)
}

func TestResponsePreservesConcurrentLedgerAppend(t *testing.T) {
	api, req, state := responseFixture(t)
	api.appendConcurrent = true
	state, err := Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.ErrorIs(t, err, ErrPending)
	state, err = Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.NoError(t, err)
	assert.True(t, state.Done)
	env, err := decodeValue(api.issue.Metadata[EnvelopeKey])
	require.NoError(t, err)
	require.Len(t, env.Responses, 2)
	assert.Equal(t, "concurrent-operation", env.Responses[1].OperationID)
	assert.Equal(t, 2, api.posts)
}

func TestResponseChangedFieldsNeverRepost(t *testing.T) {
	for _, change := range []func(*Response){
		func(r *Response) { r.Body += " changed" }, func(r *Response) { r.Fingerprint = "changed" }, func(r *Response) { r.Root = "other-root" }, func(r *Response) { r.Target = "missing" }, func(r *Response) { r.Kind = "confirm" }, func(r *Response) { r.Teammate = "other" }, func(r *Response) { r.OperationID = "other-operation" },
	} {
		api, req, state := responseFixture(t)
		state, err := Deliver(t.Context(), api, req, state, func(State) error { return nil })
		require.NoError(t, err)
		change(&req.Responses[0])
		before := api.posts
		_, err = Deliver(t.Context(), api, req, state, func(State) error { return nil })
		require.Error(t, err)
		assert.Equal(t, before, api.posts)
	}
}

func TestResponseCheckpointFailureStopsLaterWrites(t *testing.T) {
	for failAt := 1; failAt <= 4; failAt++ {
		api, req, state := responseFixture(t)
		calls := 0
		last := State{}
		state, err := Deliver(t.Context(), api, req, state, func(s State) error {
			calls++
			if calls == failAt {
				return errors.New("checkpoint failed")
			}
			last = cloneState(s)
			return nil
		})
		require.ErrorIs(t, err, ErrPending)
		assert.False(t, state.Done)
		if failAt <= 2 {
			assert.Equal(t, 1, api.posts, "failed checkpoint prevents the response send")
		}
		resumed, err := Deliver(t.Context(), api, req, last, func(State) error { return nil })
		require.NoError(t, err)
		assert.True(t, resumed.Done)
		assert.Equal(t, 2, api.posts, "one root and exactly one recovered response")
	}
}

func TestContinuationRejectsAmbiguityAndForeignAncestors(t *testing.T) {
	for _, comments := range [][]kata.Comment{
		{{UID: "root", IssueUID: "issue"}, {UID: "one", IssueUID: "issue", Handle: "c:abc123", ReplyTo: "root", Kind: kata.CommentReply}, {UID: "two", IssueUID: "issue", Handle: "c:abc123", ReplyTo: "root", Kind: kata.CommentReply}},
		{{UID: "root", IssueUID: "foreign"}, {UID: "one", IssueUID: "issue", Handle: "c:abc123", ReplyTo: "root", Kind: kata.CommentReply}},
		{{UID: "root", IssueUID: "issue"}, {UID: "one", IssueUID: "issue", Handle: "c:abc123", ReplyTo: "missing", Kind: kata.CommentReply}},
		{{UID: "root", IssueUID: "issue"}, {UID: "one", IssueUID: "issue", Handle: "c:abc123", ReplyTo: "root", Kind: kata.CommentReply}, {UID: "one", IssueUID: "issue", ReplyTo: "root", Kind: kata.CommentReply}},
	} {
		_, err := ContinuationTarget(kata.Issue{UID: "issue", Comments: comments}, "root", "c:abc123")
		require.Error(t, err)
	}
}

func TestResponseProviderSizeFailureDoesNotMutateRemoteState(t *testing.T) {
	api, req, state := responseFixture(t)
	req.Responses[0].Body = strings.Repeat("x", kata.MaxRequestBytes+1)
	posts, guards := api.posts, api.guards
	state, err := Deliver(t.Context(), api, req, state, func(State) error { return nil })
	require.ErrorIs(t, err, ErrPending)
	assert.False(t, state.Done)
	assert.Equal(t, posts, api.posts)
	assert.Equal(t, guards, api.guards)
	assert.Len(t, req.Responses[0].Body, kata.MaxRequestBytes+1)
}
