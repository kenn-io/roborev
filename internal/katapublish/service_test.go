package katapublish

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
)

type fakeAPI struct {
	issue                  kata.Issue
	creates, posts, guards int
	postErr, guardErr      error
	loseReceipt            bool
	edited                 bool
	duplicates             bool
	checkpointed           bool
}

func (f *fakeAPI) Endpoint() string { return "https://ledger.example" }
func (f *fakeAPI) ResolveProject(context.Context, string) (kata.Binding, error) {
	return kata.Binding{Project: "reviews", ProjectUID: "project-1"}, nil
}

func (f *fakeAPI) ListProject(_ context.Context, _ string, opts kata.ListOpts) ([]kata.Issue, error) {
	if opts.Status != "all" || !opts.Unlimited {
		return nil, errors.New("incomplete lookup")
	}
	if f.issue.UID == "" {
		return nil, nil
	}
	if f.duplicates {
		return []kata.Issue{f.issue, f.issue}, nil
	}
	return []kata.Issue{f.issue}, nil
}
func (f *fakeAPI) Show(context.Context, string) (kata.Issue, error) { return f.issue, nil }
func (f *fakeAPI) ShowComplete(context.Context, string) (kata.Issue, error) {
	i := f.issue
	return i, nil
}

func (f *fakeAPI) Create(_ context.Context, req kata.CreateReq) (kata.CreateResult, error) {
	f.creates++
	if !f.checkpointed {
		return kata.CreateResult{}, errors.New("send before checkpoint")
	}
	if err := kata.ValidateCreateRequest(req); err != nil {
		return kata.CreateResult{}, err
	}
	f.issue = kata.Issue{Status: "open", UID: "issue-1", ShortID: "one", QualifiedID: "reviews#one", ProjectUID: "project-1", Metadata: req.Metadata}
	return kata.CreateResult{IssueUID: f.issue.UID, ShortID: f.issue.ShortID, ProjectUID: f.issue.ProjectUID}, nil
}

func (f *fakeAPI) MutateMetadata(_ context.Context, _ string, key string, previous, value any) (kata.Issue, error) {
	f.guards++
	if err := kata.ValidateMetadataMutation(key, previous, value); err != nil {
		return kata.Issue{}, err
	}
	if f.guardErr != nil {
		return kata.Issue{}, f.guardErr
	}
	old, _ := json.Marshal(f.issue.Metadata[key], json.Deterministic(true))
	want, _ := json.Marshal(previous, json.Deterministic(true))
	if string(old) != string(want) {
		return kata.Issue{}, errors.New("guard conflict")
	}
	f.issue.Metadata[key] = value
	return f.issue, nil
}

func (f *fakeAPI) Comment(_ context.Context, req kata.CommentReq) (kata.CommentResult, error) {
	f.posts++
	if f.postErr != nil {
		return kata.CommentResult{}, f.postErr
	}
	c := kata.Comment{UID: fmt.Sprintf("comment-%d", f.posts), IssueUID: f.issue.UID, Body: req.Body, Kind: req.Kind, ReplyTo: req.ReplyTo, Author: "service", Teammate: req.Teammate}
	if f.edited {
		c.EditedAt = new(time.Now())
	}
	f.issue.Comments = append(f.issue.Comments, c)
	if f.loseReceipt {
		return kata.CommentResult{}, errors.New("network interrupted")
	}
	return kata.CommentResult{UID: c.UID, Kind: c.Kind, ReplyTo: c.ReplyTo, Attribution: c.Attribution}, nil
}

func deliver(t *testing.T, f *fakeAPI, req Request, state State) (State, error) {
	t.Helper()
	return Deliver(context.Background(), f, req, state, func(State) error { f.checkpointed = true; return nil })
}

func TestDeliveryCompletesOnlyAfterGuardedReceipts(t *testing.T) {
	f := &fakeAPI{}
	state, err := deliver(t, f, fixtureRequest(), State{})
	require.NoError(t, err)
	assert.True(t, state.Done)
	assert.Equal(t, 1, f.creates)
	assert.Equal(t, 1, f.posts)
	assert.Equal(t, "comment-1", state.Receipts["f:one"].UID)
	assert.Equal(t, 1, f.guards)
	env, err := decodeValue(f.issue.Metadata[EnvelopeKey])
	require.NoError(t, err)
	assert.Empty(t, env.Entries[0].Pending)
	assert.Equal(t, "comment-1", env.Entries[0].ReceiptUID)
	state, err = deliver(t, f, fixtureRequest(), state)
	require.NoError(t, err)
	assert.True(t, state.Done)
	assert.Equal(t, 1, f.posts)
}

func TestDeliveryRecoversLostReceiptWithoutReposting(t *testing.T) {
	f := &fakeAPI{loseReceipt: true}
	state, err := deliver(t, f, fixtureRequest(), State{})
	require.ErrorIs(t, err, ErrPending)
	assert.False(t, state.Done)
	f.loseReceipt = false
	state, err = deliver(t, f, fixtureRequest(), state)
	require.NoError(t, err)
	assert.True(t, state.Done)
	assert.Equal(t, 1, f.posts)
}

func TestDeliveryRetryIgnoresQuotedReplyMarkers(t *testing.T) {
	req := fixtureRequest()
	req.Entries = append(req.Entries, Entry{Selector: "f:two", ReviewUUID: req.ReviewUUID, DocumentHash: "hash-1", Index: 1, Body: "Second finding"})
	f := &fakeAPI{}
	state, err := Deliver(t.Context(), f, req, State{}, func(state State) error {
		f.checkpointed = true
		if !state.FirstAttempt["f:two"].IsZero() {
			return errors.New("interrupted before second finding")
		}
		return nil
	})
	require.ErrorIs(t, err, ErrPending)
	require.Len(t, f.issue.Comments, 1)
	root := f.issue.Comments[0]
	f.issue.Comments = append(f.issue.Comments,
		kata.Comment{UID: "quoted-reply", IssueUID: root.IssueUID, Kind: kata.CommentReply, ReplyTo: root.UID, Body: "Quoting: " + root.Body, Author: "service", Teammate: req.Teammate},
		kata.Comment{UID: "other-teammate", IssueUID: root.IssueUID, Body: root.Body, Author: "service", Teammate: "another-reviewer"},
	)
	state, err = deliver(t, f, req, state)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.True(state.Done)
	assert.Equal(2, f.posts, "retry must post only the missing finding")
	assert.Equal(root.UID, state.Receipts["f:one"].UID)
	assert.Equal("comment-2", state.Receipts["f:two"].UID)
}

func TestDeliveryUnresolvedCases(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"mismatch", kata.ErrIdempotencyMismatch}, {"private error", errors.New("confidential-sentinel")}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAPI{postErr: tc.err}
			state, err := deliver(t, f, fixtureRequest(), State{})
			require.ErrorIs(t, err, ErrPending)
			assert.NotContains(t, err.Error(), "confidential-sentinel")
			assert.False(t, state.Done)
			assert.Empty(t, state.Receipts)
		})
	}
	for _, tc := range []struct {
		name string
		f    *fakeAPI
	}{{"edited", &fakeAPI{edited: true}}, {"guard denied", &fakeAPI{guardErr: errors.New("claim_denied")}}} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := deliver(t, tc.f, fixtureRequest(), State{})
			require.ErrorIs(t, err, ErrPending)
			assert.False(t, state.Done)
		})
	}
}

func TestRecoveryAfterReplayWindowNeverReposts(t *testing.T) {
	req := fixtureRequest()
	f := &fakeAPI{postErr: errors.New("network")}
	state, err := deliver(t, f, req, State{})
	require.Error(t, err)
	state.FirstAttempt["f:one"] = time.Now().Add(-8 * 24 * time.Hour)
	f.postErr = nil
	_, err = deliver(t, f, req, state)
	require.ErrorIs(t, err, ErrPending)
	assert.Equal(t, 1, f.posts)
}

func TestLookupMismatchAndMissingLedgerNeverCreateOrRepair(t *testing.T) {
	req := fixtureRequest()
	f := &fakeAPI{}
	state, err := deliver(t, f, req, State{})
	require.NoError(t, err)
	f.issue.Metadata[FingerprintKey] = "different"
	_, err = deliver(t, f, req, State{})
	require.ErrorIs(t, err, ErrPending)
	assert.Equal(t, 1, f.creates)
	f.issue.Metadata[FingerprintKey], _ = Fingerprint(req)
	delete(f.issue.Metadata, EnvelopeKey)
	_, err = deliver(t, f, req, state)
	require.ErrorIs(t, err, ErrPending)
	assert.Equal(t, 1, f.posts)
}

func TestOversizePublicationStaysLocal(t *testing.T) {
	req := fixtureRequest()
	req.Entries[0].Body = strings.Repeat("x", kata.MaxRequestBytes)
	f := &fakeAPI{}
	_, err := deliver(t, f, req, State{})
	require.ErrorIs(t, err, ErrPending)
	assert.Zero(t, f.creates)
	assert.Zero(t, f.posts)
}

func TestDestinationAndCheckpointFailuresNeverSend(t *testing.T) {
	for _, change := range []func(*Request, *State){
		func(r *Request, _ *State) { r.ProjectUID = "replacement-project" },
		func(r *Request, _ *State) { r.Endpoint = "https://other.example" },
		func(_ *Request, s *State) { s.ProjectUID = "replacement-project" },
	} {
		req := fixtureRequest()
		state := State{}
		change(&req, &state)
		f := &fakeAPI{}
		_, err := deliver(t, f, req, state)
		require.ErrorIs(t, err, ErrPending)
		assert.Zero(t, f.creates)
		assert.Zero(t, f.posts)
	}
	f := &fakeAPI{}
	_, err := Deliver(context.Background(), f, fixtureRequest(), State{}, func(State) error { return errors.New("private-checkpoint-error") })
	require.ErrorIs(t, err, ErrPending)
	assert.NotContains(t, err.Error(), "private-checkpoint-error")
	assert.Zero(t, f.creates)
}

func TestLedgerPendingDoesNotCompleteAndChangedMappingIsNotRepaired(t *testing.T) {
	req := fixtureRequest()
	f := &fakeAPI{postErr: errors.New("disconnected")}
	state, err := deliver(t, f, req, State{})
	require.ErrorIs(t, err, ErrPending)
	assert.False(t, state.Done)
	env, err := decodeValue(f.issue.Metadata[EnvelopeKey])
	require.NoError(t, err)
	env.Entries[0].Index = 123
	f.issue.Metadata[EnvelopeKey] = env
	f.postErr = nil
	_, err = deliver(t, f, req, state)
	require.ErrorIs(t, err, ErrPending)
	assert.Equal(t, 1, f.posts)
	assert.Zero(t, f.guards)
}

func TestRecoveryFindsExpiredReceiptOnClosedIssue(t *testing.T) {
	req := fixtureRequest()
	f := &fakeAPI{loseReceipt: true}
	state, err := deliver(t, f, req, State{})
	require.Error(t, err)
	state.FirstAttempt["f:one"] = time.Now().Add(-8 * 24 * time.Hour)
	f.issue.Status = "closed"
	f.loseReceipt = false
	state, err = deliver(t, f, req, state)
	require.NoError(t, err)
	assert.True(t, state.Done)
	assert.Equal(t, 1, f.posts)
}

func TestAmbiguousMarkerAndKnownEditedReceiptStayPending(t *testing.T) {
	f := &fakeAPI{}
	state, err := deliver(t, f, fixtureRequest(), State{})
	require.NoError(t, err)
	f.duplicates = true
	_, err = deliver(t, f, fixtureRequest(), state)
	require.ErrorIs(t, err, ErrPending)
	f.duplicates = false
	f.issue.Comments[0].EditedAt = new(time.Now())
	_, err = deliver(t, f, fixtureRequest(), state)
	require.ErrorIs(t, err, ErrPending)
	assert.Equal(t, 1, f.posts)
}

func FuzzSummaryErrorsDoNotPublishPrivateDiagnostic(f *testing.F) {
	f.Add("provider returned private source")
	f.Add("quote\"\n<script>:")
	f.Fuzz(func(t *testing.T, detail string) {
		private := "private-sentinel:" + detail
		req := fixtureRequest()
		req.Policy = "summary"
		req.Create.Body = "Inspect locally"
		req.Entries[0].Body = "Inspect finding locally"
		api := &fakeAPI{postErr: errors.New(private)}
		state, err := deliver(t, api, req, State{})
		require.ErrorIs(t, err, ErrPending)
		assert.NotContains(t, err.Error(), private)
		assert.False(t, state.Done)
		payload, err := json.Marshal(api.issue)
		require.NoError(t, err)
		assert.NotContains(t, string(payload), "private-sentinel:")
	})
}

func TestSafeTypedLedgerErrorsAndUnsupportedResponses(t *testing.T) {
	f := &fakeAPI{guardErr: &kata.APIError{Status: 409, Code: "claim_denied", Message: "private-sentinel", Class: kata.ErrConflict}}
	state, err := deliver(t, f, fixtureRequest(), State{})
	require.ErrorIs(t, err, ErrPending)
	require.ErrorIs(t, err, kata.ErrConflict)
	assert.Equal(t, "claim_denied", kata.ErrorCode(err))
	assert.NotContains(t, err.Error(), "private-sentinel")
	assert.False(t, state.Done)
	req := fixtureRequest()
	req.Responses = []Response{{OperationID: "response"}}
	f = &fakeAPI{}
	_, err = deliver(t, f, req, State{})
	require.ErrorIs(t, err, ErrPending)
	assert.Zero(t, f.creates)
	assert.Zero(t, f.posts)
}

func TestOfflineProjectResolutionIsPinnedBeforeSend(t *testing.T) {
	req := fixtureRequest()
	req.ProjectUID = ""
	api := &fakeAPI{}
	var saved State
	state, err := Deliver(context.Background(), api, req, State{}, func(s State) error {
		require.Equal(t, "project-1", s.ProjectUID)
		api.checkpointed = true
		saved = s
		return nil
	})
	require.NoError(t, err)
	assert.True(t, state.Done)
	assert.Equal(t, "project-1", saved.ProjectUID)
	state.ProjectUID = "previous-project"
	_, err = deliver(t, api, req, state)
	require.ErrorIs(t, err, ErrPending)
	assert.Equal(t, 1, api.creates)
	api = &fakeAPI{}
	_, err = Deliver(context.Background(), api, req, State{}, func(State) error { return errors.New("disk full") })
	require.ErrorIs(t, err, ErrPending)
	assert.Zero(t, api.creates)
}

func TestReceiptValidationRejectsChangedImmutableFields(t *testing.T) {
	req := fixtureRequest()
	body := req.Entries[0].Body + "\n\n" + marker(req, req.Entries[0].Selector)
	original := kata.Comment{UID: "receipt", IssueUID: "issue", Body: body, Author: "writer", Teammate: req.Teammate}
	for _, change := range []func(*kata.Comment){
		func(c *kata.Comment) { c.UID = "different" }, func(c *kata.Comment) { c.IssueUID = "different" }, func(c *kata.Comment) { c.Body = "edited" }, func(c *kata.Comment) { c.Kind = kata.CommentReply }, func(c *kata.Comment) { c.ReplyTo = "target" }, func(c *kata.Comment) { c.Teammate = "other" }, func(c *kata.Comment) { c.EditedAt = new(time.Now()) },
	} {
		comment := original
		change(&comment)
		_, ok, err := findComment(kata.Issue{UID: "issue", Comments: []kata.Comment{comment}}, body, "", "", req.Teammate, "receipt")
		require.ErrorIs(t, err, ErrPending)
		assert.False(t, ok)
	}
	_, _, err := findComment(kata.Issue{UID: "issue", Comments: []kata.Comment{original, original}}, body, "", "", req.Teammate, "receipt")
	require.ErrorIs(t, err, ErrPending)
}

func TestLookupRecoversReceiptsWithoutAnyWrites(t *testing.T) {
	req := fixtureRequest()
	api := &fakeAPI{loseReceipt: true}
	state, err := deliver(t, api, req, State{})
	require.ErrorIs(t, err, ErrPending)
	state, err = Lookup(context.Background(), api, req, state)
	require.ErrorIs(t, err, ErrPending)
	assert.False(t, state.Done)
	assert.Equal(t, "comment-1", state.Receipts["f:one"].UID)
	assert.Equal(t, 1, api.creates)
	assert.Equal(t, 1, api.posts)
	assert.Zero(t, api.guards)
	api.loseReceipt = false
	state, err = deliver(t, api, req, state)
	require.NoError(t, err)
	state, err = Lookup(context.Background(), api, req, state)
	require.NoError(t, err)
	assert.True(t, state.Done)
	assert.Equal(t, 1, api.guards)
}

func TestLedgerReceiptAttributionMustMatchStoredComment(t *testing.T) {
	api := &fakeAPI{}
	state, err := deliver(t, api, fixtureRequest(), State{})
	require.NoError(t, err)
	env, err := decodeValue(api.issue.Metadata[EnvelopeKey])
	require.NoError(t, err)
	env.Entries[0].AttributionHash = attributionHash(kata.Attribution{Author: "different-writer"})
	api.issue.Metadata[EnvelopeKey] = env
	_, err = Lookup(context.Background(), api, fixtureRequest(), state)
	require.ErrorIs(t, err, ErrPending)
	_, err = deliver(t, api, fixtureRequest(), state)
	require.ErrorIs(t, err, ErrPending)
	assert.Equal(t, 1, api.guards)
}

func TestFailedDiagnosticIssueOnlyDoesNotNeedReviewIdentity(t *testing.T) {
	req := fixtureRequest()
	req.ReviewUUID = ""
	req.Entries = nil
	api := &fakeAPI{}
	state, err := deliver(t, api, req, State{})
	require.NoError(t, err)
	assert.True(t, state.Done)
	assert.Equal(t, 1, api.creates)
	assert.Zero(t, api.posts)
	_, err = Lookup(context.Background(), api, req, state)
	require.NoError(t, err)
}

func TestExplicitExistingIssueTargetIsReservedForK3(t *testing.T) {
	req := fixtureRequest()
	req.IssueUID = "existing-issue"
	api := &fakeAPI{}
	_, err := deliver(t, api, req, State{})
	require.ErrorIs(t, err, ErrPending)
	assert.Zero(t, api.creates)
	assert.Zero(t, api.posts)
}

func TestPublicationPreflightSizesReachableReceiptTransition(t *testing.T) {
	req := fixtureRequest()
	req.Entries[0].Body = strings.Repeat("x", 600_000)
	api := &fakeAPI{}
	state, err := deliver(t, api, req, State{})
	require.NoError(t, err)
	assert.True(t, state.Done)
	assert.Equal(t, 1, api.creates)
	assert.Equal(t, 1, api.posts)
	assert.Equal(t, 1, api.guards)
}

func TestPublicationPreflightIncludesCompletedReceipts(t *testing.T) {
	req := fixtureRequest()
	withEntries := func(count int) (Request, Envelope) {
		r := req
		r.Entries = nil
		for i := range count {
			r.Entries = append(r.Entries, Entry{Selector: fmt.Sprintf("f:%06d", i), ReviewUUID: req.ReviewUUID, DocumentHash: strings.Repeat("a", 64), Index: i, Body: "x"})
		}
		env, err := newEnvelope(r, r.ProjectUID)
		require.NoError(t, err)
		return r, env
	}
	// Find the largest many-short-findings pending envelope accepted by the
	// provider's exact serialized guard bound. Receipts need more space than x.
	low, high := 1, 2
	for {
		_, env := withEntries(high)
		if validateGuard(env, env) != nil {
			break
		}
		low = high
		high *= 2
	}
	for low+1 < high {
		middle := (low + high) / 2
		_, env := withEntries(middle)
		if validateGuard(env, env) == nil {
			low = middle
		} else {
			high = middle
		}
	}
	req, env := withEntries(low)
	require.NoError(t, validateGuard(env, env))
	api := &fakeAPI{}
	_, err := deliver(t, api, req, State{})
	require.ErrorIs(t, err, ErrPending)
	assert.Zero(t, api.creates, "full-lifecycle oversize must be refused before issue creation")
	assert.Zero(t, api.posts)
}

func TestDeliveryClosedIssueWaitsForReopen(t *testing.T) {
	assert := assert.New(t)
	f := &fakeAPI{postErr: errors.New("offline")}
	state, err := deliver(t, f, fixtureRequest(), State{})
	require.ErrorIs(t, err, ErrPending)
	f.postErr = nil
	f.issue.Status = "closed"
	state, err = deliver(t, f, fixtureRequest(), state)
	require.ErrorIs(t, err, ErrPending)
	assert.False(state.Done)
	assert.Equal(1, f.posts)
	assert.Empty(state.Receipts)
	f.issue.Status = "open"
	state, err = deliver(t, f, fixtureRequest(), state)
	require.NoError(t, err)
	assert.True(state.Done)
	assert.Equal(2, f.posts)
}
