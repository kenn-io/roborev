package katacontext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

func TestMain(m *testing.M) { testenv.RunIsolatedMain(m) }

type readFixture struct {
	db       *storage.DB
	server   *katafake.Server
	api      *kata.APIClient
	cfg      config.KataIntegrationConfig
	job      *storage.ReviewJob
	snapshot storage.FindingSnapshot
	state    katapublish.State
}

func newReadFixture(t *testing.T) readFixture {
	return newReadPublicationFixture(t, false)
}

type interruptSecondRoot struct {
	katapublish.API
	count int
}

func (a *interruptSecondRoot) Comment(ctx context.Context, req kata.CommentReq) (kata.CommentResult, error) {
	a.count++
	if a.count == 2 {
		return kata.CommentResult{}, errors.New("synthetic interruption before second root")
	}
	return a.API.Comment(ctx, req)
}

func newReadPublicationFixture(t *testing.T, partial bool) readFixture {
	t.Helper()
	f := readFixture{db: testutil.OpenTestDB(t), server: katafake.New(t)}
	f.cfg = config.KataIntegrationConfig{Enabled: true, Endpoint: f.server.URL(), APIKey: "synthetic-kata-credential", DefaultProject: "reviews", Teammate: "roborev"}
	var err error
	f.api, err = kata.NewAPIClient("", f.cfg, kata.WithoutWorkspace())
	require.NoError(t, err)
	t.Cleanup(f.api.CloseIdleConnections)
	repo, err := f.db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	document := `{"schema_version":2,"summary":"A finding","verdict":"fail","findings":[{"severity":"high","problem":"Original error remains reproducible.","fix":"Check the input.","location":"a.go:1"}]}`
	if partial {
		document = `{"schema_version":2,"summary":"Two findings","verdict":"fail","findings":[{"severity":"high","problem":"Original error remains reproducible.","fix":"Check the input.","location":"a.go:1"},{"severity":"medium","problem":"Cancellation is ignored.","fix":"Check cancellation.","location":"b.go:1"}]}`
	}
	f.job = testutil.CreateCompletedReview(t, f.db, repo.ID, "abc123", "test", document)
	snapshots, err := f.db.AssignFindingSnapshots(t.Context(), f.job.ID)
	require.NoError(t, err)
	if partial {
		require.Len(t, snapshots, 2)
	} else {
		require.Len(t, snapshots, 1)
	}
	f.snapshot = snapshots[0]
	binding, err := f.api.ResolveProject(t.Context(), "reviews")
	require.NoError(t, err)
	req := katapublish.Request{
		OperationID: "publication-example", Endpoint: f.server.URL(), Project: "reviews", ProjectUID: binding.ProjectUID,
		ReviewUUID: f.snapshot.ReviewUUID.String(), Policy: "comments", Teammate: "roborev", //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
		Create:  kata.CreateReq{Title: "Review findings", Project: "reviews"},
		Entries: []katapublish.Entry{{Selector: f.snapshot.Selector, ReviewUUID: f.snapshot.ReviewUUID.String(), DocumentHash: f.snapshot.DocumentHash, Index: 0, Body: "Original error remains reproducible."}}, //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	}
	var deliveryAPI katapublish.API = f.api
	if partial {
		second := snapshots[1]
		req.Entries = append(req.Entries, katapublish.Entry{Selector: second.Selector, ReviewUUID: second.ReviewUUID.String(), DocumentHash: second.DocumentHash, Index: second.Index, Body: second.Finding.Problem}) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
		deliveryAPI = &interruptSecondRoot{API: f.api}
	}
	f.state, err = katapublish.Deliver(t.Context(), deliveryAPI, req, katapublish.State{}, func(katapublish.State) error { return nil })
	if partial {
		require.Error(t, err)
		require.False(t, f.state.Done)
	} else {
		require.NoError(t, err)
		require.True(t, f.state.Done)
	}
	require.NoError(t, storage.InsertKataHandoffTx(t.Context(), f.db, f.job.ID, req))
	raw, err := json.Marshal(f.state)
	require.NoError(t, err)
	status := "done"
	if partial {
		status = "pending"
	}
	_, err = f.db.Exec(`UPDATE kata_handoffs SET state_json=?,status=? WHERE operation_id=?`, string(raw), status, req.OperationID)
	require.NoError(t, err)
	return f
}

func TestReadbackKeepsDeliveredRootWhenSiblingIsPending(t *testing.T) {
	f := newReadPublicationFixture(t, true)
	_, err := f.api.Comment(t.Context(), kata.CommentReq{
		Project: "reviews", Issue: f.state.IssueUID, Kind: kata.CommentReply,
		ReplyTo: f.state.Receipts[f.snapshot.Selector].UID, Body: "Evidence on the delivered root", Teammate: "reviewer", IdempotencyKey: "partial-evidence",
	})
	require.NoError(t, err)
	before := len(f.server.AllRequests())
	got, err := ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), Run{}) //nolint:forbidigo // Synthetic review UUID at the daemon API boundary.
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Findings, 2)
	assert.Equal(t, "available", got[0].Findings[0].Availability)
	require.Len(t, got[0].Findings[0].Replies, 1)
	assert.Equal(t, "Evidence on the delivered root", got[0].Findings[0].Replies[0].Body)
	assert.Equal(t, "target_unavailable", got[0].Findings[1].Availability)
	for _, request := range f.server.AllRequests()[before:] {
		assert.Equal(t, http.MethodGet, request.Method)
	}
}

func TestReadbackRecoversValidatedRootDespitePendingSibling(t *testing.T) {
	f := newReadPublicationFixture(t, true)
	root := f.state.Receipts[f.snapshot.Selector].UID
	_, err := f.api.Comment(t.Context(), kata.CommentReq{
		Project: "reviews", Issue: f.state.IssueUID, Kind: kata.CommentReply, ReplyTo: root,
		Body: "Evidence survives the lost local root receipt", Teammate: "reviewer", IdempotencyKey: "lost-local-receipt-evidence",
	})
	require.NoError(t, err)
	handoffs, err := f.db.KataHandoffs(t.Context(), f.job.ID)
	require.NoError(t, err)
	require.Len(t, handoffs, 1)
	state := handoffs[0].State
	state.Receipts = map[string]katapublish.Receipt{}
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE kata_handoffs SET state_json=? WHERE operation_id=?`, string(raw), handoffs[0].OperationID)
	require.NoError(t, err)
	before := len(f.server.AllRequests())
	got, err := ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), Run{}) //nolint:forbidigo // Synthetic review UUID at the daemon API boundary.
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Findings, 2)
	assert.Equal(t, "available", got[0].Findings[0].Availability)
	assert.Equal(t, root, got[0].Findings[0].RootUID)
	require.Len(t, got[0].Findings[0].Replies, 1)
	assert.Equal(t, "Evidence survives the lost local root receipt", got[0].Findings[0].Replies[0].Body)
	assert.Equal(t, "target_unavailable", got[0].Findings[1].Availability)
	for _, request := range f.server.AllRequests()[before:] {
		assert.Equal(t, http.MethodGet, request.Method)
	}
	saved, err := f.db.KataHandoffs(t.Context(), f.job.ID)
	require.NoError(t, err)
	require.Len(t, saved, 1)
	assert.Empty(t, saved[0].State.Receipts, "read-back must not checkpoint recovered candidates")
}

func TestReadJobShowsClaimWithoutInferringFix(t *testing.T) {
	f := newReadFixture(t)
	const claim = "Fixed in abc123; the input validation regression passes."
	posted, err := f.api.Comment(t.Context(), kata.CommentReq{
		Project: "reviews", Issue: f.state.IssueUID, Kind: kata.CommentReply,
		ReplyTo: f.state.Receipts[f.snapshot.Selector].UID, Body: claim, Teammate: "fixer", IdempotencyKey: "fix-example",
	})
	require.NoError(t, err)
	before := len(f.server.Requests())
	snapshots, err := ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), Run{}) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	require.Len(t, snapshots[0].Findings, 1)
	finding := snapshots[0].Findings[0]
	assert.Equal(t, "available", finding.Availability)
	require.NotNil(t, finding.Snapshot)
	assert.Equal(t, "Original error remains reproducible.", finding.Snapshot.Finding.Problem)
	require.Len(t, finding.Replies, 1)
	assert.Equal(t, posted.UID, finding.Replies[0].UID)
	assert.Equal(t, claim, finding.Replies[0].Body)
	assert.Equal(t, kata.CommentReply, finding.Replies[0].Kind)
	assert.Equal(t, "fixer", finding.Replies[0].Teammate)
	for _, request := range f.server.Requests()[before:] {
		assert.Equal(t, http.MethodGet, request.Method, "read-back must not repair the ledger or publish")
	}
	review, err := f.db.GetReviewByJobID(f.job.ID)
	require.NoError(t, err)
	assert.False(t, review.Closed)
	assert.Equal(t, storage.VerdictFail, review.Verdict())
}

func TestReadJobHonorsConnectionAndReviewBoundaries(t *testing.T) {
	f := newReadFixture(t)
	for _, tc := range []struct {
		name   string
		cfg    config.KataIntegrationConfig
		review string
	}{
		{"disabled", config.KataIntegrationConfig{}, f.snapshot.ReviewUUID.String()},                                                                                      //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
		{"different endpoint", config.KataIntegrationConfig{Enabled: true, Endpoint: "https://other.example", DefaultProject: "reviews"}, f.snapshot.ReviewUUID.String()}, //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
		{"different review", f.cfg, uuid.New().String()},                                                                                                                  //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(f.server.AllRequests())
			got, err := ReadJob(t.Context(), f.db, tc.cfg, f.job.ID, tc.review, Run{})
			require.NoError(t, err)
			assert.Empty(t, got)
			assert.Len(t, f.server.AllRequests(), before)
		})
	}
	_, err := f.db.Exec(`UPDATE review_jobs SET source='ci' WHERE id=?`, f.job.ID)
	require.NoError(t, err)
	before := len(f.server.AllRequests())
	got, err := ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), Run{}) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Len(t, f.server.AllRequests(), before, "CI must not even fetch reply context")
}

func TestReadJobKeepsFrozenDestinationAndUnavailableSnapshot(t *testing.T) {
	f := newReadFixture(t)
	// Current defaults do not authorize changing a captured destination.
	f.cfg.DefaultProject = "other-project"
	got, err := ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), Run{}) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, f.state.IssueRef, got[0].IssueRef)
	require.Len(t, got[0].Findings, 1)
	assert.Equal(t, "available", got[0].Findings[0].Availability)

	_, err = f.db.Exec(`DELETE FROM finding_snapshots WHERE selector=?`, f.snapshot.Selector)
	require.NoError(t, err)
	got, err = ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), Run{}) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Findings, 1)
	assert.Equal(t, f.snapshot.Selector, got[0].Findings[0].Selector)
	assert.Equal(t, "snapshot_unavailable", got[0].Findings[0].Availability)
	assert.Nil(t, got[0].Findings[0].Snapshot)
}

func TestReadJobRecordsExactlyTheConsumedRun(t *testing.T) {
	f := newReadFixture(t)
	root := f.state.Receipts[f.snapshot.Selector].UID
	_, err := f.api.Comment(t.Context(), kata.CommentReq{
		Project: "reviews", Issue: f.state.IssueUID, Kind: kata.CommentReply,
		ReplyTo: root, Body: "First evidence", Teammate: "first", IdempotencyKey: "evidence-first",
	})
	require.NoError(t, err)
	run := Run{ID: uuid.New().String(), Purpose: "fix"}                                            //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	first, err := ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), run) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Len(t, first[0].Findings, 1)
	require.Len(t, first[0].Findings[0].Replies, 1)
	_, err = f.api.Comment(t.Context(), kata.CommentReq{
		Project: "reviews", Issue: f.state.IssueUID, Kind: kata.CommentReply,
		ReplyTo: root, Body: "Later evidence", Teammate: "second", IdempotencyKey: "evidence-later",
	})
	require.NoError(t, err)

	before := len(f.server.AllRequests())
	sameRun, err := ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), run) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	assert.Equal(t, first, sameRun)
	assert.Len(t, f.server.AllRequests(), before, "a run reuses the exact snapshot it consumed")

	later, err := ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), Run{ID: uuid.New().String(), Purpose: "review"}) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	require.Len(t, later, 1)
	require.Len(t, later[0].Findings, 1)
	require.Len(t, later[0].Findings[0].Replies, 2)
	assert.Equal(t, "First evidence", later[0].Findings[0].Replies[0].Body)
	assert.Equal(t, "Later evidence", later[0].Findings[0].Replies[1].Body)
}

// A response retains its selected Entry for verification. That does not make
// it a second publication of the root while the response is pending.
func TestReadJobDoesNotTreatResponseHandoffAsRootPublication(t *testing.T) {
	f := newReadFixture(t)
	handoffs, err := f.db.KataHandoffs(t.Context(), f.job.ID)
	require.NoError(t, err)
	require.Len(t, handoffs, 1)
	response := handoffs[0].Request
	response.OperationID = "response-example"
	response.IssueUID = f.state.IssueUID
	response.Responses = []katapublish.Response{{
		OperationID: "response-example", Target: f.state.Receipts[f.snapshot.Selector].UID,
		Kind: "reply", Body: "Verification evidence", Teammate: "validator",
	}}
	require.NoError(t, storage.InsertKataHandoffTx(t.Context(), f.db, f.job.ID, response))
	stateJSON, err := json.Marshal(f.state)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE kata_handoffs SET state_json=? WHERE operation_id=?`, string(stateJSON), response.OperationID)
	require.NoError(t, err)

	got, err := ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), Run{}) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	require.Len(t, got, 1, "a response operation must not create another root publication view")
	require.Len(t, got[0].Findings, 1)
	assert.Equal(t, f.snapshot.Selector, got[0].Findings[0].Selector)
	assert.Equal(t, f.state.Receipts[f.snapshot.Selector].UID, got[0].Findings[0].RootUID)
}

func TestReadJobFreezesEmptyObservation(t *testing.T) {
	f := newReadFixture(t)
	handoffs, err := f.db.KataHandoffs(t.Context(), f.job.ID)
	require.NoError(t, err)
	require.Len(t, handoffs, 1)
	original := handoffs[0]
	_, err = f.db.Exec(`DELETE FROM kata_handoffs WHERE operation_id=?`, original.OperationID)
	require.NoError(t, err)
	run := Run{ID: uuid.New().String(), Purpose: "planning"}                                       //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	first, err := ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), run) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	assert.Empty(t, first)
	require.NoError(t, storage.InsertKataHandoffTx(t.Context(), f.db, f.job.ID, original.Request))
	raw, err := json.Marshal(original.State)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE kata_handoffs SET state_json=?,status='done' WHERE operation_id=?`, string(raw), original.OperationID)
	require.NoError(t, err)
	before := len(f.server.AllRequests())
	same, err := ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), run) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	assert.Empty(t, same)
	assert.Len(t, f.server.AllRequests(), before)
}

func TestCachedReadbackCannotCrossEndpointBoundaries(t *testing.T) {
	f := newReadFixture(t)
	run := Run{ID: uuid.New().String(), Purpose: "fix"}                                            //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	first, err := ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), run) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	require.Len(t, first, 1)
	handoffs, err := f.db.KataHandoffs(t.Context(), f.job.ID)
	require.NoError(t, err)
	other := handoffs[0].Request
	other.OperationID = "other-publication"
	other.Endpoint = "https://other.example"
	require.NoError(t, storage.InsertKataHandoffTx(t.Context(), f.db, f.job.ID, other))
	cfg := f.cfg
	cfg.Endpoint = other.Endpoint
	before := len(f.server.AllRequests())
	got, err := ReadJob(t.Context(), f.db, cfg, f.job.ID, f.snapshot.ReviewUUID.String(), run) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	assert.Empty(t, got, "a second matching handoff cannot authorize returning the old endpoint's cache")
	assert.Len(t, f.server.AllRequests(), before)
}

func TestUnavailableConnectionObservationIsFrozen(t *testing.T) {
	f := newReadFixture(t)
	broken := f.cfg
	broken.APIKey = ""
	broken.APIKeyFile = filepath.Join(t.TempDir(), "missing-credential")
	run := Run{ID: uuid.New().String(), Purpose: "planning"}                                        //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	first, err := ReadJob(t.Context(), f.db, broken, f.job.ID, f.snapshot.ReviewUUID.String(), run) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Len(t, first[0].Findings, 1)
	assert.Equal(t, "connection_unavailable", first[0].Findings[0].Availability)
	before := len(f.server.AllRequests())
	same, err := ReadJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), run) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	assert.Equal(t, first, same)
	assert.Len(t, f.server.AllRequests(), before)
}

func TestIssueConsumptionRecordsOnlyReturnedPublication(t *testing.T) {
	f := newReadFixture(t)
	handoffs, err := f.db.KataHandoffs(t.Context(), f.job.ID)
	require.NoError(t, err)
	other := handoffs[0].Request
	other.OperationID = "sibling-publication"
	state, err := katapublish.Deliver(t.Context(), f.api, other, katapublish.State{}, func(katapublish.State) error { return nil })
	require.NoError(t, err)
	require.NoError(t, storage.InsertKataHandoffTx(t.Context(), f.db, f.job.ID, other))
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE kata_handoffs SET state_json=?,status='done' WHERE operation_id=?`, string(raw), other.OperationID)
	require.NoError(t, err)
	run := Run{ID: uuid.New().String(), Purpose: "mcp"}                                                                 //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	got, err := ReadIssueJob(t.Context(), f.db, f.cfg, f.job.ID, f.snapshot.ReviewUUID.String(), f.state.IssueUID, run) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, f.state.IssueUID, got[0].IssueUID)
	stored, err := f.db.KataReplySnapshot(t.Context(), run.ID, f.job.ID, f.snapshot.ReviewUUID.String()) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	var captured []Snapshot
	require.NoError(t, json.Unmarshal(stored, &captured))
	assert.Equal(t, got, captured)
}

// Every immutable ledger identity is a boundary: changing any one must reject
// the target, for arbitrary hostile replacement strings and field choices.
func FuzzPublicationIdentityCannotBeSubstituted(f *testing.F) {
	for field := range uint8(11) {
		f.Add(field, "replacement")
	}
	f.Fuzz(func(t *testing.T, field uint8, replacement string) {
		req := katapublish.Request{
			OperationID: "publication", Endpoint: "https://ledger.example", Project: "reviews", ProjectUID: "project-1", ReviewUUID: "review-1", Policy: "comments", Teammate: "roborev",
			Entries: []katapublish.Entry{{Selector: "f:example", ReviewUUID: "review-1", DocumentHash: "document-hash", Index: 0, Body: "Finding evidence"}},
		}
		state := katapublish.State{IssueUID: "I0000000000000000000000001", ProjectUID: "project-1", Receipts: map[string]katapublish.Receipt{
			"f:example": {UID: "C0000000000000000000000001", Attribution: kata.Attribution{Author: "reviewer", Teammate: "validator"}},
		}}
		fingerprint, err := katapublish.Fingerprint(req)
		require.NoError(t, err)
		manifest, err := katapublish.PublicationManifest(req, state)
		require.NoError(t, err)
		env := katapublish.Envelope{
			Version: 1, WriterEndpoint: req.Endpoint, ProjectUID: state.ProjectUID, OperationID: req.OperationID,
			Fingerprint: fingerprint, Policy: req.Policy, ReviewUUID: req.ReviewUUID, Entries: manifest, Responses: []katapublish.LedgerResponse{},
		}
		action := sha256.Sum256([]byte(req.Endpoint + "\x00" + state.ProjectUID + "\x00" + req.OperationID))
		issue := kata.Issue{UID: state.IssueUID, ProjectUID: state.ProjectUID, Metadata: map[string]any{
			katapublish.ActionKey: hex.EncodeToString(action[:]), katapublish.FingerprintKey: fingerprint, katapublish.EnvelopeKey: env,
		}}
		handoff := storage.KataHandoff{Request: req, State: state, Fingerprint: fingerprint, ReviewUUID: req.ReviewUUID}
		require.True(t, publicationMatches(issue, handoff, state))
		// The fixed prefix guarantees a changed value without narrowing inputs.
		value := "changed:" + replacement
		switch field % 11 {
		case 0:
			issue.UID = value
		case 1:
			issue.ProjectUID = value
		case 2:
			env.WriterEndpoint = value
		case 3:
			env.OperationID = value
		case 4:
			env.ReviewUUID = value
		case 5:
			env.Entries[0].Selector = value
		case 6:
			env.Entries[0].DocumentHash = value
		case 7:
			env.Entries[0].Index = 1
		case 8:
			env.Entries[0].ReceiptUID = value
		case 9:
			env.Entries[0].AttributionHash = value
		case 10:
			env.Entries[0].BodyHash = value
		}
		issue.Metadata[katapublish.EnvelopeKey] = env
		assert.False(t, publicationMatches(issue, handoff, state))
	})
}
