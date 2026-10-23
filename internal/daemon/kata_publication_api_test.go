package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestKataPublicationMappingsExcludePayloads(t *testing.T) {
	h := storage.KataHandoff{OperationID: "publish:example", ReviewUUID: "review-example", Status: "pending", LastError: "private-server-echo", Request: katapublish.Request{Endpoint: "https://private.example", Policy: "summary", Create: kata.CreateReq{Body: "private-review-text"}, Entries: []katapublish.Entry{{Selector: "f:example", Body: "private-finding-text"}}}, State: katapublish.State{IssueRef: "reviews#abcd", IssueUID: "issue-example", Receipts: map[string]katapublish.Receipt{"f:example": {UID: "comment-example"}}}}
	got := kataPublicationMapping(h)
	data, err := json.Marshal(got)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.NotContains(string(data), "private")
	assert.NotContains(string(data), "endpoint")
	require.Len(t, got.Findings, 1)
	assert.Equal("f:example", got.Findings[0].ID)
	assert.Equal("comment-example", got.Findings[0].Comment)
	assert.Equal("pending", got.Findings[0].State, "a local comment receipt does not complete the ledger")
	assert.Equal("summary", got.Findings[0].Policy)
}

func TestKataPublishRequiresEnabledConnection(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	job := testutil.CreateCompletedReview(t, db, repo.ID, "publication-disabled", "test", findingsReviewDocument)
	w := kataPost(t, server, "/api/kata/publish", map[string]any{"job_id": job.ID})
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	records, err := db.KataHandoffs(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestKataPublicationAPIExplicitCaptureAndRead(t *testing.T) {
	server, db, _ := newTestServer(t)
	server.configWatcher.Config().Integrations.Kata.Enabled = true
	repo := testutil.CreateTestRepo(t, db)
	job := testutil.CreateCompletedReview(t, db, repo.ID, "publication-explicit", "test", findingsReviewDocument)
	db.SetKataHandoffBuilder(func(_ context.Context, c storage.KataCompletion) ([]katapublish.Request, error) {
		return []katapublish.Request{{OperationID: "publish:explicit", ReviewUUID: c.ReviewUUID, Policy: "summary", Entries: []katapublish.Entry{{Selector: c.Snapshots[0].Selector, ReviewUUID: c.ReviewUUID}}}}, nil
	})
	w := kataPost(t, server, "/api/kata/publish", map[string]any{"job_id": job.ID})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeKata[KataPublicationsBody](t, w)
	require.Len(t, got.Publications, 1)
	assert.Equal(t, "publish:explicit", got.Publications[0].OperationID)
	assert.Equal(t, "pending", got.Publications[0].State)
	require.Len(t, got.Publications[0].Findings, 1)
	assert.NotEmpty(t, got.Publications[0].Findings[0].ID)
	read := kataGet(t, server, "/api/kata/publications", mapQueryJob(job.ID))
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	again := decodeKata[KataPublicationsBody](t, read)
	assert.Equal(t, got.Publications[0].ReviewUUID, again.Publications[0].ReviewUUID)
}

func TestKataExplicitPublishRejectsHistoricalRunAuthorization(t *testing.T) {
	for _, terminal := range []string{"done", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			server, db, _ := newTestServer(t)
			server.configWatcher.Config().Integrations.Kata.Enabled = true
			repo := testutil.CreateTestRepo(t, db)
			job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "publication-rerun", Agent: "test"})
			require.NoError(t, err)
			builder := func(_ context.Context, c storage.KataCompletion) ([]katapublish.Request, error) {
				identity := c.ReviewUUID
				if identity == "" {
					identity = storage.KataFailureRun(c.Job)
				}
				return []katapublish.Request{{OperationID: "publish:" + identity, ReviewUUID: c.ReviewUUID, Policy: "summary"}}, nil
			}
			complete := func() {
				t.Helper()
				claimed, err := db.ClaimJob("publication-worker")
				require.NoError(t, err)
				require.NotNil(t, claimed)
				if terminal == "done" {
					require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "", findingsReviewDocument))
				} else {
					updated, err := db.FailJob(job.ID, "publication-worker", "synthetic failure")
					require.NoError(t, err)
					require.True(t, updated)
				}
			}
			db.SetKataHandoffBuilder(builder)
			complete()
			first := kataPost(t, server, "/api/kata/publish", map[string]any{"job_id": job.ID})
			require.Equal(t, http.StatusOK, first.Code, first.Body.String())
			original := decodeKata[KataPublicationsBody](t, first)
			require.Len(t, original.Publications, 1)
			db.SetKataHandoffBuilder(nil)
			require.NoError(t, db.ReenqueueJob(job.ID, storage.ReenqueueOpts{}))
			complete()
			response := kataPost(t, server, "/api/kata/publish", map[string]any{"job_id": job.ID})
			require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "no matching Kata hook")
			db.SetKataHandoffBuilder(builder)
			response = kataPost(t, server, "/api/kata/publish", map[string]any{"job_id": job.ID})
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			current := decodeKata[KataPublicationsBody](t, response)
			require.Len(t, current.Publications, 1, "explicit publication returns only the current run")
			assert.NotEqual(t, original.Publications[0].OperationID, current.Publications[0].OperationID)
		})
	}
}

func TestKataPublicationAPIRejectsUnknownAndIncompleteJobs(t *testing.T) {
	server, db, _ := newTestServer(t)
	server.configWatcher.Config().Integrations.Kata.Enabled = true
	repo := testutil.CreateTestRepo(t, db)
	queued, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "queued", Agent: "test"})
	require.NoError(t, err)
	w := kataGet(t, server, "/api/kata/publications?job_id=999999", nil)
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	w = kataPost(t, server, "/api/kata/publish", map[string]any{"job_id": queued.ID})
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	w = kataPost(t, server, "/api/kata/publish", map[string]any{"job_id": 999999})
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func mapQueryJob(id int64) url.Values { return url.Values{"job_id": {strconv.FormatInt(id, 10)}} }

// Simulate a successful create whose response never reached the delivery
// worker. The issue and metadata survive; no local issue UID is known yet.
type lostKataCreateReceipt struct{ *kata.APIClient }

func (c lostKataCreateReceipt) Create(ctx context.Context, req kata.CreateReq) (kata.CreateResult, error) {
	if _, err := c.APIClient.Create(ctx, req); err != nil {
		return kata.CreateResult{}, err
	}
	return kata.CreateResult{}, errors.New("response lost")
}

func TestKataPublicationAPIRecoversIssueByMetadataWithoutWrites(t *testing.T) {
	server, fake := kataAPIServer(t)
	db := server.db
	repo := testutil.CreateTestRepo(t, db)
	job := testutil.CreateCompletedReview(t, db, repo.ID, "publication-lost-response", "test", findingsReviewDocument)
	client, err := kata.NewAPIClient("", server.configWatcher.Config().Integrations.Kata, kata.WithoutWorkspace())
	require.NoError(t, err)
	binding, err := client.ResolveProject(t.Context(), "reviews")
	require.NoError(t, err)
	db.SetKataHandoffBuilder(func(_ context.Context, c storage.KataCompletion) ([]katapublish.Request, error) {
		entry := c.Snapshots[0]
		return []katapublish.Request{{OperationID: "publish:lost-response", Endpoint: client.Endpoint(), Project: "reviews", ProjectUID: binding.ProjectUID, Policy: "summary", ReviewUUID: c.ReviewUUID, Teammate: "roborev", CredentialFingerprint: kataCredentialFingerprint(server.configWatcher.Config().Integrations.Kata), Create: kata.CreateReq{Title: "Review notice", Body: "Inspect locally."}, Entries: []katapublish.Entry{{Selector: entry.Selector, ReviewUUID: c.ReviewUUID, DocumentHash: entry.DocumentHash, Index: entry.Index, Body: "Inspect locally."}}}}, nil
	})
	require.NoError(t, db.CaptureKataHandoffs(t.Context(), job.ID))
	records, err := db.KataHandoffs(t.Context(), job.ID)
	require.NoError(t, err)
	require.Len(t, records, 1)
	_, err = katapublish.Deliver(t.Context(), lostKataCreateReceipt{client}, records[0].Request, katapublish.State{}, func(katapublish.State) error { return nil })
	require.ErrorIs(t, err, katapublish.ErrPending)
	before := len(fake.AllRequests())
	response := kataGet(t, server, "/api/kata/publications", mapQueryJob(job.ID))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	got := decodeKata[KataPublicationsBody](t, response)
	require.Len(t, got.Publications, 1)
	assert := assert.New(t)
	assert.NotEmpty(got.Publications[0].Issue)
	assert.NotEmpty(got.Publications[0].IssueUID)
	assert.Equal("pending", got.Publications[0].State)
	for _, request := range fake.AllRequests()[before:] {
		assert.Equal(http.MethodGet, request.Method, "lookup must never deliver work")
	}
	after, err := db.KataHandoffs(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Empty(after[0].State.IssueUID, "read-only recovery does not checkpoint worker state")
}
