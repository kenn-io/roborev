package daemon

import (
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/internal/kataresponse"
	"go.kenn.io/roborev/internal/mcpserver"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestKataContextListsSafePendingOperations(t *testing.T) {
	s, db, _ := newTestServer(t)
	jobID, findings := rebuttalFindingParent(t, s, db)
	seedResponseMapping(t, s, db, jobID, findings[0].ID, "summary")
	intent := kataresponse.Request{JobID: jobID, OperationID: "Readback-Case-Key", FindingID: findings[0].ID, Kind: "reply", Body: "synthetic-evidence-sentinel: validate this proposed change independently."}
	raw, err := json.Marshal(intent)
	require.NoError(t, err)
	queued := serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
	require.Equal(t, http.StatusAccepted, queued.Code, queued.Body.String())
	// Safe local delivery status remains inspectable while the connection is
	// disabled. No reply claims are fetched or exposed by that connection.
	s.configWatcher.Config().Integrations.Kata.Enabled = false
	for _, in := range []KataContextInput{{JobID: jobID}, {Ref: "reviews#one"}} {
		out, err := s.humaKataContext(t.Context(), &in)
		require.NoError(t, err)
		assert.Empty(t, out.Body.Snapshots)
		require.Len(t, out.Body.Operations, 1)
		assert.Equal(t, intent.OperationID, out.Body.Operations[0].OperationID)
		assert.Equal(t, "pending", out.Body.Operations[0].State)
		data, err := json.Marshal(out.Body)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "synthetic-evidence-sentinel")
		operation, err := json.Marshal(out.Body.Operations[0])
		require.NoError(t, err)
		assert.NotContains(t, string(operation), "job_id", "delivery status must not expose its internal source job ID")
	}
	_, err = db.Exec(`UPDATE review_jobs SET source='ci' WHERE id=?`, jobID)
	require.NoError(t, err)
	ci, err := s.humaKataContext(t.Context(), &KataContextInput{JobID: jobID})
	require.NoError(t, err)
	assert.Empty(t, ci.Body.Snapshots)
	assert.Empty(t, ci.Body.Operations)
}

func TestKataConsumedRunRequiresUUIDDespiteOpaqueDeliveryKeys(t *testing.T) {
	s, fake := kataAPIServer(t)
	before := len(fake.AllRequests())
	_, err := s.humaKataConsumeContext(t.Context(), &KataConsumeContextInput{Body: KataConsumeContextRequest{Ref: "reviews#one", RunID: "Readback-Case-Key", Purpose: "fix"}})
	require.Error(t, err)
	var status interface{ GetStatus() int }
	require.ErrorAs(t, err, &status)
	assert.Equal(t, http.StatusBadRequest, status.GetStatus())
	assert.Len(t, fake.AllRequests(), before)
}

func TestIssueContextKeepsLegacyPublicationWhenConnectionDisabled(t *testing.T) {
	s, fake := kataAPIServer(t)
	jobID, findings := rebuttalFindingParent(t, s, s.db)
	seedResponseMapping(t, s, s.db, jobID, findings[0].ID, "comments")
	handoffs, err := s.db.KataHandoffs(t.Context(), jobID)
	require.NoError(t, err)
	require.Len(t, handoffs, 1)
	req := handoffs[0].Request
	req.OperationID, req.Policy, req.Entries = "publication-legacy", "legacy", nil
	require.NoError(t, storage.InsertKataHandoffTx(t.Context(), s.db, jobID, req))
	state := katapublish.State{IssueUID: "legacy-issue", IssueRef: "reviews#legacy", ProjectUID: "project-uid"}
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE kata_handoffs SET state_json=?,last_error=? WHERE operation_id=?`, string(raw), "synthetic-provider-detail", req.OperationID)
	require.NoError(t, err)
	s.configWatcher.Config().Integrations.Kata.Enabled = false
	before := len(fake.AllRequests())
	out, err := s.humaKataContext(t.Context(), &KataContextInput{Ref: state.IssueRef})
	require.NoError(t, err)
	assert.Empty(t, out.Body.Snapshots)
	require.Len(t, out.Body.Publications, 1)
	assert.Equal(t, req.OperationID, out.Body.Publications[0].OperationID)
	assert.Equal(t, "legacy", out.Body.Publications[0].Policy)
	assert.Equal(t, "pending", out.Body.Publications[0].State)
	assert.NotEmpty(t, out.Body.Publications[0].LastError)
	data, err := json.Marshal(out.Body)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "synthetic-provider-detail")
	assert.Len(t, fake.AllRequests(), before)
}

func TestDiagnosticPublicationNeedsNoReviewOrKataRead(t *testing.T) {
	s, fake := kataAPIServer(t)
	job := createTestJob(t, s.db, t.TempDir(), "abc123", "test")
	_, err := s.db.ClaimJob("worker")
	require.NoError(t, err)
	failed, err := s.db.FailJob(job.ID, "worker", "synthetic failure")
	require.NoError(t, err)
	require.True(t, failed)
	_, err = s.db.GetReviewByJobID(job.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	req := katapublish.Request{OperationID: "publication-diagnostic", Endpoint: fake.URL(), Project: "reviews", ProjectUID: fake.Projects[0].UID, Policy: "legacy", FailureRun: "failed-run", Teammate: "reviewer"}
	require.NoError(t, storage.InsertKataHandoffTx(t.Context(), s.db, job.ID, req))
	state := katapublish.State{IssueUID: "diagnostic-issue", IssueRef: "reviews#diagnostic", ProjectUID: req.ProjectUID}
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE kata_handoffs SET state_json=? WHERE operation_id=?`, string(raw), req.OperationID)
	require.NoError(t, err)
	before := len(fake.AllRequests())
	for _, in := range []KataContextInput{{Ref: state.IssueRef}, {JobID: job.ID}} {
		out, err := s.humaKataContext(t.Context(), &in)
		require.NoError(t, err)
		require.Len(t, out.Body.Publications, 1)
		assert.Equal(t, req.OperationID, out.Body.Publications[0].OperationID)
		assert.Empty(t, out.Body.Snapshots)
	}
	consumed, err := s.humaKataConsumeContext(t.Context(), &KataConsumeContextInput{Body: KataConsumeContextRequest{JobID: job.ID, RunID: "11111111-1111-4111-8111-111111111111", Purpose: "mcp"}})
	require.NoError(t, err)
	require.Len(t, consumed.Body.Publications, 1)
	assert.Empty(t, consumed.Body.Snapshots)
	assert.Len(t, fake.AllRequests(), before, "a failed-job diagnostic has no finding discussion to fetch")
}

func TestConsumedContextFreezesEmptyObservationBeforePublication(t *testing.T) {
	for _, explicitReview := range []bool{false, true} {
		t.Run(map[bool]string{false: "current review", true: "explicit immutable review"}[explicitReview], func(t *testing.T) {
			s, fake := kataAPIServer(t)
			jobID, findings := rebuttalFindingParent(t, s, s.db)
			snapshot, err := s.db.FindingSnapshot(t.Context(), findings[0].ID)
			require.NoError(t, err)
			request := KataConsumeContextInput{Body: KataConsumeContextRequest{JobID: jobID, RunID: "11111111-1111-4111-8111-111111111111", Purpose: "fix"}}
			if explicitReview {
				request.Body.ReviewUUID = snapshot.ReviewUUID.String()
			} //nolint:forbidigo // Synthetic immutable review UUID at the consume API boundary.
			before := len(fake.AllRequests())
			first, err := s.humaKataConsumeContext(t.Context(), &request)
			require.NoError(t, err)
			assert.Empty(t, first.Body.Snapshots)
			req := katapublish.Request{OperationID: "publication-later", Endpoint: fake.URL(), Project: "reviews", ProjectUID: fake.Projects[0].UID, Policy: "comments", ReviewUUID: snapshot.ReviewUUID.String(), Teammate: "roborev", Entries: []katapublish.Entry{{Selector: snapshot.Selector, ReviewUUID: snapshot.ReviewUUID.String(), DocumentHash: snapshot.DocumentHash, Index: snapshot.Index, Body: "Synthetic finding"}}} //nolint:forbidigo // Synthetic immutable UUIDs in the frozen publication fixture.
			require.NoError(t, storage.InsertKataHandoffTx(t.Context(), s.db, jobID, req))
			second, err := s.humaKataConsumeContext(t.Context(), &request)
			require.NoError(t, err)
			assert.Equal(t, first.Body.Snapshots, second.Body.Snapshots)
			assert.Len(t, fake.AllRequests(), before, "a consumed empty snapshot must not fetch newly published replies")
			stored, err := s.db.KataReplySnapshot(t.Context(), request.Body.RunID, jobID, snapshot.ReviewUUID.String()) //nolint:forbidigo // Synthetic immutable review UUID at the snapshot storage boundary.
			require.NoError(t, err)
			assert.JSONEq(t, `[]`, string(stored))
			request.Body.RunID = "22222222-2222-4222-8222-222222222222"
			fresh, err := s.humaKataConsumeContext(t.Context(), &request)
			require.NoError(t, err)
			assert.Len(t, fresh.Body.Snapshots, 1, "a new run may acquire the new publication")
		})
	}
}

func TestKataContextCIExclusionForBothMarkers(t *testing.T) {
	for _, marker := range []string{"source='ci'", "ci_base_branch='main'"} {
		t.Run(marker, func(t *testing.T) {
			s, fake := kataAPIServer(t)
			jobID, findings := rebuttalFindingParent(t, s, s.db)
			seedResponseMapping(t, s, s.db, jobID, findings[0].ID, "comments")
			_, err := s.db.Exec(`UPDATE review_jobs SET `+marker+` WHERE id=?`, jobID)
			require.NoError(t, err)
			before := len(fake.AllRequests())
			out, err := s.humaKataContext(t.Context(), &KataContextInput{JobID: jobID})
			require.NoError(t, err)
			assert.Empty(t, out.Body.Snapshots)
			assert.Len(t, fake.AllRequests(), before)
		})
	}
}

func TestDaemonMCPKataWritesRequireExplicitOption(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit"}[allow], func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.MCP.Enabled = true
			var options []ServerOption
			if allow {
				options = append(options, WithKataWrites())
			}
			s := newServerWithLogs(testutil.OpenTestDB(t), cfg, "", newTestErrorLog(), newTestActivityLog(), options...)
			jobID, findings := rebuttalFindingParent(t, s, s.db)
			seedResponseMapping(t, s, s.db, jobID, findings[0].ID, "comments")
			t.Cleanup(func() { _ = s.Close() })
			httpServer := httptest.NewServer(s.httpServer.Handler)
			t.Cleanup(httpServer.Close)
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
			session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: httpServer.URL + mcpserver.HTTPPath, HTTPClient: httpServer.Client(), DisableStandaloneSSE: true}, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = session.Close() })
			listed, err := session.ListTools(t.Context(), nil)
			require.NoError(t, err)
			var names []string
			for _, tool := range listed.Tools {
				names = append(names, tool.Name)
			}
			assert.Contains(t, names, "roborev_kata_issues")
			assert.Contains(t, names, "roborev_kata_context")
			if allow {
				assert.Contains(t, names, "roborev_kata_reply")
				const operation = "11111111-1111-4111-8111-111111111111"
				result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "roborev_kata_reply", Arguments: map[string]any{
					"job_id": jobID, "operation_id": operation, "finding_id": findings[0].ID, "kind": "reply", "body": "Proposed fix committed; validate the evidence independently.",
				}})
				require.NoError(t, err)
				assert.False(t, result.IsError)
				status, err := s.db.KataResponseStatus(t.Context(), operation)
				require.NoError(t, err)
				assert.Equal(t, "pending", status.State)
				assert.Equal(t, "root-uid", status.Target)
				review, err := s.db.GetReviewByJobID(jobID)
				require.NoError(t, err)
				assert.False(t, review.Closed)
			} else {
				assert.NotContains(t, names, "roborev_kata_reply")
			}
		})
	}
}

func TestCIConsumerNeverReadsMappedLocalPriorReplies(t *testing.T) {
	for _, marker := range []string{"source", "base branch"} {
		t.Run(marker, func(t *testing.T) {
			s, fake := kataAPIServer(t)
			jobID, findings := rebuttalFindingParent(t, s, s.db)
			seedResponseMapping(t, s, s.db, jobID, findings[0].ID, "comments")
			review, err := s.db.GetReviewByJobID(jobID)
			require.NoError(t, err)
			consumer := &storage.ReviewJob{ID: jobID + 1}
			if marker == "source" {
				consumer.Source = "ci"
			} else {
				consumer.CIBaseBranch = "main"
			}
			before := len(fake.AllRequests())
			builder := s.workerPool.basePromptBuilderForJob(t.Context(), preparedJobCheckout{promptRepoPath: t.TempDir()}, consumer, s.configWatcher.Config())
			text, err := builder.BuildAddressPrompt(review, nil, "")
			require.NoError(t, err)
			assert.NotContains(t, text, "Kata finding replies")
			assert.NotContains(t, text, "Kata finding reply context")
			assert.Len(t, fake.AllRequests(), before, "CI cannot fetch even local prior reviews' mapped discussions")
		})
	}
}
