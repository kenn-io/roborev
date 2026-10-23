//go:build kata && !windows

package katapublish_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/findingid"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/internal/kataresponse"
)

// Only acknowledgements are lost here: every mutation reaches the disposable
// real provider before the wrapper reports the simulated disconnect.
type responseLedgerFaultAPI struct {
	katapublish.API
	loseComment                  bool
	loseReceiptFor               string
	comments, mutations, creates int
}

func (a *responseLedgerFaultAPI) Create(context.Context, kata.CreateReq) (kata.CreateResult, error) {
	a.creates++
	return kata.CreateResult{}, errors.New("response attempted to create an issue")
}

func (a *responseLedgerFaultAPI) Comment(ctx context.Context, request kata.CommentReq) (kata.CommentResult, error) {
	a.comments++
	result, err := a.API.Comment(ctx, request)
	if err == nil && a.loseComment {
		a.loseComment = false
		return kata.CommentResult{}, errors.New("simulated lost comment acknowledgement")
	}
	return result, err
}

func (a *responseLedgerFaultAPI) MutateMetadata(ctx context.Context, ref, key string, previous, next any) (kata.Issue, error) {
	a.mutations++
	result, err := a.API.MutateMetadata(ctx, ref, key, previous, next)
	if err == nil && a.loseReceiptFor != "" {
		env, ok := next.(katapublish.Envelope)
		if ok {
			for _, slot := range env.Responses {
				if slot.OperationID == a.loseReceiptFor && slot.ReceiptUID != "" {
					a.loseReceiptFor = ""
					return kata.Issue{}, errors.New("simulated lost guarded receipt acknowledgement")
				}
			}
		}
	}
	return result, err
}

// Exercise the response ledger against Kata's real guarded metadata
// and typed-comment semantics, including recovery from both lost acknowledgements.
func TestResponseLedgerE2E(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	server := katatest.StartReal(t, ctx)
	cfg := config.KataIntegrationConfig{Enabled: true, Endpoint: server.SocketEndpoint(t, ctx), DefaultProject: "e2etest", Teammate: "roborev"}
	client, err := kata.NewAPIClient(server.Repo, cfg)
	require.NoError(err)
	defer client.CloseIdleConnections()
	binding, err := client.ResolveProject(ctx, "e2etest")
	require.NoError(err)
	reviewUUID := uuid.New()
	selector, err := findingid.New(reviewUUID, 0, strings.Repeat("b", 64))
	require.NoError(err)
	publication := katapublish.Request{OperationID: "response-ledger-publication", Endpoint: client.Endpoint(), Project: "e2etest", ProjectUID: binding.ProjectUID, Policy: "comments", ReviewUUID: reviewUUID.String(), Teammate: "roborev", Create: kata.CreateReq{Title: "Typed response findings", Body: "Inspect the review locally", ForceNew: true}, Entries: []katapublish.Entry{{Selector: selector.String(), ReviewUUID: reviewUUID.String(), DocumentHash: strings.Repeat("b", 64), Body: "The selected code path has no input guard."}}} //nolint:forbidigo // Review UUID crosses the provider publication boundary.
	published, err := katapublish.Deliver(ctx, client, publication, katapublish.State{}, func(katapublish.State) error { return nil })
	require.NoError(err)
	require.True(published.Done)
	root := published.Receipts[selector.String()]
	require.NotEmpty(root.UID)
	before, err := client.ShowComplete(ctx, published.IssueRef)
	require.NoError(err)
	raw, err := json.Marshal(before.Metadata[katapublish.EnvelopeKey])
	require.NoError(err)
	original, err := katapublish.DecodeEnvelope(raw)
	require.NoError(err)
	fingerprint, err := katapublish.Fingerprint(publication)
	require.NoError(err)
	manifest, err := katapublish.PublicationManifest(publication, published)
	require.NoError(err)
	responseRequest := func(operation, kind, body, target string) katapublish.Request {
		t.Helper()
		request := publication
		request.OperationID = operation
		intent := kataresponse.Request{JobID: 42, OperationID: request.OperationID, FindingID: selector.String(), Kind: kind, Body: body}
		if target != root.UID {
			intent.TargetComment = target
		}
		require.NoError(intent.Validate())
		intentFingerprint, err := intent.Fingerprint()
		require.NoError(err)
		request.Create = kata.CreateReq{}
		request.IssueUID = published.IssueUID
		request.PublicationOperationID = publication.OperationID
		request.PublicationFingerprint = fingerprint
		request.PublicationProjectUID = publication.ProjectUID
		request.PublicationEntries = manifest
		request.RootAttributionHash = katapublish.AttributionFingerprint(root.Attribution)
		request.Teammate = "reviewer"
		request.Responses = []katapublish.Response{{Root: root.UID, Target: target, OperationID: request.OperationID, Fingerprint: intentFingerprint, Body: body, Kind: kind, Teammate: request.Teammate}}
		return request
	}
	// Serialize every checkpoint, rather than retaining a shallow map copy, so
	// retries use only state that could have survived a daemon restart.
	var persisted []byte
	checkpoint := func(state katapublish.State) error {
		raw, err := json.Marshal(state)
		if err == nil {
			persisted = raw
		}
		return err
	}
	restore := func() katapublish.State {
		t.Helper()
		var state katapublish.State
		require.NoError(json.Unmarshal(persisted, &state))
		return state
	}
	api := &responseLedgerFaultAPI{API: client, loseComment: true}
	refute := responseRequest("Finding-Reply-A", "refute", "The caller validates this input before reaching the selected code path.", root.UID)
	state, err := katapublish.Deliver(ctx, api, refute, katapublish.State{}, checkpoint)
	require.ErrorIs(err, katapublish.ErrPending)
	assert.False(state.Done)
	require.Equal(1, api.comments)
	state, err = katapublish.Deliver(ctx, api, refute, restore(), checkpoint)
	require.NoError(err)
	require.True(state.Done)
	refuteReceipt := state.Receipts[refute.OperationID]
	require.NotEmpty(refuteReceipt.UID)
	assert.Equal("reviewer", refuteReceipt.Attribution.Teammate)
	assert.Equal(1, api.comments, "lost comment acknowledgement must reconcile by the intact marker")
	_, err = katapublish.Deliver(ctx, api, refute, restore(), checkpoint)
	require.NoError(err)
	assert.Equal(1, api.comments)

	confirm := responseRequest("Confirm-Reply-B", "confirm", "The selected path still reproduces the original finding under this input.", root.UID)
	api.loseReceiptFor = confirm.OperationID
	state, err = katapublish.Deliver(ctx, api, confirm, katapublish.State{}, checkpoint)
	require.ErrorIs(err, katapublish.ErrPending)
	assert.False(state.Done)
	mutations := api.mutations
	state, err = katapublish.Deliver(ctx, api, confirm, restore(), checkpoint)
	require.NoError(err)
	require.True(state.Done)
	assert.Equal(2, api.comments)
	assert.Equal(mutations, api.mutations, "lost receipt acknowledgement must reconcile without another write")
	require.NotEmpty(state.Receipts[confirm.OperationID].UID)

	duplicate := responseRequest(strings.ToLower(refute.OperationID), "refute", "The caller has another explanation for the same guarded code path.", root.UID)
	state, err = katapublish.Deliver(ctx, api, duplicate, katapublish.State{}, checkpoint)
	require.ErrorIs(err, kata.ErrDuplicateReply)
	assert.False(state.Done)
	assert.Equal(refuteReceipt.UID, state.Receipts["duplicate:"+duplicate.OperationID].UID)
	assert.Empty(state.Receipts[duplicate.OperationID].UID)

	complete, err := client.ShowComplete(ctx, published.IssueRef)
	require.NoError(err)
	target, err := katapublish.ContinuationTarget(complete, root.UID, refuteReceipt.UID)
	require.NoError(err)
	continuation := responseRequest("Continuation-Reply-D", "reply", "The follow-up preserves the selected finding thread.", target)
	state, err = katapublish.Deliver(ctx, api, continuation, katapublish.State{}, checkpoint)
	require.NoError(err)
	assert.True(state.Done)
	assert.Equal(0, api.creates, "response delivery must never create an issue")
	complete, err = client.ShowComplete(ctx, published.IssueRef)
	require.NoError(err)
	require.Len(complete.Comments, 4, "one root and exactly three successful responses")
	assert.Equal(root.UID, complete.Comments[1].ReplyTo)
	assert.Equal(kata.CommentRefute, complete.Comments[1].Kind)
	assert.Equal(root.UID, complete.Comments[2].ReplyTo)
	assert.Equal(kata.CommentConfirm, complete.Comments[2].Kind)
	assert.Equal(refuteReceipt.UID, complete.Comments[3].ReplyTo)
	assert.Equal(kata.CommentReply, complete.Comments[3].Kind)
	raw, err = json.Marshal(complete.Metadata[katapublish.EnvelopeKey])
	require.NoError(err)
	final, err := katapublish.DecodeEnvelope(raw)
	require.NoError(err)
	assert.Equal(original.OperationID, final.OperationID)
	assert.Equal(original.Fingerprint, final.Fingerprint)
	assert.Equal(original.Entries, final.Entries)
	require.Len(final.Responses, 4, "the unresolved duplicate retains its guarded pending slot")
	assert.Equal(refuteReceipt.UID, final.Responses[0].ReceiptUID)
	assert.NotEmpty(final.Responses[0].AttributionHash)
	assert.NotEmpty(final.Responses[1].ReceiptUID)
	assert.Empty(final.Responses[2].ReceiptUID)
	assert.NotEmpty(final.Responses[3].ReceiptUID)
}
