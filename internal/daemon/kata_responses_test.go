package daemon

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/internal/kataresponse"
	"go.kenn.io/roborev/internal/storage"
)

func seedResponseMapping(t *testing.T, s *Server, db *storage.DB, jobID int64, selector, policy string) {
	t.Helper()
	cfg := s.configWatcher.Config()
	cfg.Integrations.Kata.Enabled = true
	cfg.Integrations.Kata.Endpoint = "https://kata.example"
	snapshot, err := db.FindingSnapshot(t.Context(), selector)
	require.NoError(t, err)
	req := katapublish.Request{OperationID: "publish:fixture", Endpoint: cfg.Integrations.Kata.Endpoint, Project: "reviews", ProjectUID: "project-uid", Policy: policy, ReviewUUID: snapshot.ReviewUUID.String(), Teammate: "roborev", CredentialFingerprint: kataCredentialFingerprint(cfg.Integrations.Kata), Entries: []katapublish.Entry{{Selector: selector, ReviewUUID: snapshot.ReviewUUID.String(), DocumentHash: snapshot.DocumentHash, Index: snapshot.Index, Body: "Policy-filtered finding"}}} //nolint:forbidigo // Synthetic daemon/provider operation and review UUID text boundaries.
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, storage.InsertKataHandoffTx(t.Context(), tx, jobID, req))
	require.NoError(t, tx.Commit())
	state := katapublish.State{IssueUID: "issue-uid", IssueRef: "reviews#one", ProjectUID: "project-uid", Done: true, Receipts: map[string]katapublish.Receipt{selector: {UID: "root-uid"}}}
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE kata_handoffs SET state_json=?,status='done' WHERE operation_id=?`, string(raw), req.OperationID)
	require.NoError(t, err)
}

func TestKataResponseSummaryRetryAndStatus(t *testing.T) {
	s, db, _ := newTestServer(t)
	jobID, findings := rebuttalFindingParent(t, s, db)
	seedResponseMapping(t, s, db, jobID, findings[0].ID, "summary")
	intent := kataresponse.Request{JobID: jobID, OperationID: uuid.New().String(), FindingID: findings[0].ID, Kind: "refute", Body: "confidential-sentinel: the caller validates this input first."} //nolint:forbidigo // Synthetic daemon/provider operation and review UUID text boundaries.
	raw, err := json.Marshal(intent)
	require.NoError(t, err)
	rr := serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
	require.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())
	var status storage.KataResponseStatus
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &status))
	assert.Equal(t, "pending", status.State)
	assert.Equal(t, "root-uid", status.Target)
	handoffs, err := db.KataHandoffs(t.Context(), jobID)
	require.NoError(t, err)
	for _, h := range handoffs {
		data, err := json.Marshal(h.Request)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "confidential-sentinel")
	}
	saved, err := db.KataResponseIntent(t.Context(), intent.OperationID)
	require.NoError(t, err)
	assert.Equal(t, intent, saved, "summary filtering must retain the complete caller intent locally")
	publications, err := s.kataPublications(t.Context(), jobID, "")
	require.NoError(t, err)
	assert.Len(t, publications.Body.Publications, 1, "response handoffs are not additional root publications")
	// Retrying needs no current connection or mappings.
	s.configWatcher.Config().Integrations.Kata.Enabled = false
	rr = serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
	require.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())
	rr = serveHuma(t, s, http.MethodGet, "/api/kata/response?operation_id="+intent.OperationID, nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "confidential-sentinel")
	intent.Body += " Changed."
	raw, err = json.Marshal(intent)
	require.NoError(t, err)
	rr = serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.NotContains(t, rr.Body.String(), "confidential-sentinel")
}

func TestKataResponseSelectorResolvesOriginalJobWithoutChangingRetryIntent(t *testing.T) {
	s, db, _ := newTestServer(t)
	jobID, findings := rebuttalFindingParent(t, s, db)
	seedResponseMapping(t, s, db, jobID, findings[0].ID, "summary")
	intent := kataresponse.Request{OperationID: "Fix-42:Attempt-A", FindingID: strings.ToUpper(findings[0].ID), Kind: "refute", Body: "confidential-sentinel: the caller validates this input before the selected path."}
	raw, err := json.Marshal(intent)
	require.NoError(t, err)
	rr := serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
	require.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())
	saved, err := db.KataResponseIntent(t.Context(), intent.OperationID)
	require.NoError(t, err)
	assert.Equal(t, intent, saved, "job resolution must not rewrite the fingerprinted caller intent")
	handoffs, err := db.KataHandoffs(t.Context(), jobID)
	require.NoError(t, err)
	var responses []storage.KataHandoff
	for _, handoff := range handoffs {
		if len(handoff.Request.Responses) != 0 {
			responses = append(responses, handoff)
		}
	}
	require.Len(t, responses, 1)
	assert.Equal(t, jobID, responses[0].JobID)
	assert.Equal(t, findings[0].ID, responses[0].Request.Entries[0].Selector)
	assert.NotContains(t, rr.Body.String(), "confidential-sentinel")
	rr = serveHuma(t, s, http.MethodGet, "/api/kata/response?operation_id="+intent.OperationID, nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var status storage.KataResponseStatus
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &status))
	assert.Equal(t, intent.OperationID, status.OperationID)
	rr = serveHuma(t, s, http.MethodGet, "/api/kata/response?operation_id="+strings.ToLower(intent.OperationID), nil)
	assert.Equal(t, http.StatusNotFound, rr.Code, "opaque keys are case-sensitive")
	// Identical retries resolve the persisted intent before consulting the
	// current connection or parent job state.
	s.configWatcher.Config().Integrations.Kata.Enabled = false
	_, err = db.Exec(`UPDATE review_jobs SET status='queued' WHERE id=?`, jobID)
	require.NoError(t, err)
	rr = serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
	require.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())
	intent.Body += " Changed."
	raw, err = json.Marshal(intent)
	require.NoError(t, err)
	rr = serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "different payload")
	assert.NotContains(t, rr.Body.String(), "confidential-sentinel")
}

func TestKataPublicationsIncludeSafePendingResponseStatuses(t *testing.T) {
	s, db, _ := newTestServer(t)
	jobID, findings := rebuttalFindingParent(t, s, db)
	seedResponseMapping(t, s, db, jobID, findings[0].ID, "summary")
	intent := kataresponse.Request{JobID: jobID, OperationID: "0190abcd-1234-7000-8000-abcdef123456", FindingID: findings[0].ID, Kind: "reply", Body: "confidential-sentinel: validation is in progress."}
	raw, err := json.Marshal(intent)
	require.NoError(t, err)
	rr := serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
	require.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())
	rr = serveHuma(t, s, http.MethodGet, fmt.Sprintf("/api/kata/publications?job_id=%d", jobID), nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var body struct {
		Operations []storage.KataResponseStatus `json:"operations"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Operations, 1)
	assert.Equal(t, intent.OperationID, body.Operations[0].OperationID)
	assert.Equal(t, "pending", body.Operations[0].State)
	assert.Equal(t, "reply", body.Operations[0].Kind)
	assert.NotContains(t, rr.Body.String(), "confidential-sentinel")
}

func TestReassessmentCompletionOwnsAnnotationAndDelivery(t *testing.T) {
	for _, policy := range []string{"comments", "summary"} {
		for _, outcome := range []string{"confirmed", "refuted", "superseded"} {
			t.Run(policy+"/"+outcome, func(t *testing.T) {
				s, db, _ := newTestServer(t)
				jobID, findings := rebuttalFindingParent(t, s, db)
				seedResponseMapping(t, s, db, jobID, findings[0].ID, policy)
				s.configWatcher.Config().Integrations.Kata.PublishReassessments = true
				rr := serveHuma(t, s, http.MethodPost, "/api/rebuttal", rebuttalRequestBody(t, map[string]any{"job_id": jobID, "finding_id": findings[0].ID, "replacement": findings[1].ID, "commenter": "reader", "comment": "Check the guard."}))
				require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
				var job storage.ReviewJob
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &job))
				// Simulate worker completion after a caller has disconnected.
				_, err := db.Exec(`UPDATE review_jobs SET status='running' WHERE id=?`, job.ID)
				require.NoError(t, err)
				output := `{"outcome":"` + outcome + `","explanation":"confidential-sentinel: the caller validates this value before the selected path."}`
				require.NoError(t, db.CompleteJob(job.ID, "test", output))
				require.NoError(t, db.CompleteJob(job.ID, "test", output))
				result, err := db.Reassessment(t.Context(), job.ID)
				require.NoError(t, err)
				require.NotNil(t, result.Assessment)
				assert.Equal(t, outcome, result.Assessment.Outcome)
				handoffs, err := db.KataHandoffs(t.Context(), job.ID)
				require.NoError(t, err)
				require.Len(t, handoffs, 1)
				response := handoffs[0].Request.Responses[0]
				assert.Equal(t, map[string]string{"confirmed": "confirm", "refuted": "refute", "superseded": "supersede"}[outcome], response.Kind)
				assert.Equal(t, "roborev-test", response.Teammate)
				if policy == "summary" {
					data, err := json.Marshal(handoffs[0].Request)
					require.NoError(t, err)
					assert.NotContains(t, string(data), "confidential-sentinel")
				}
				comments, err := db.GetCommentsForJob(jobID)
				require.NoError(t, err)
				require.Len(t, comments, 2)
				assert.Contains(t, comments[1].Response, outcome)
				review, err := db.GetReviewByJobID(jobID)
				require.NoError(t, err)
				assert.False(t, review.Closed)
			})
		}
	}
}

func TestReassessmentPublicationIsOptIn(t *testing.T) {
	s, db, _ := newTestServer(t)
	jobID, findings := rebuttalFindingParent(t, s, db)
	seedResponseMapping(t, s, db, jobID, findings[0].ID, "comments")
	s.configWatcher.Config().Integrations.Kata.PublishReassessments = false
	rr := serveHuma(t, s, http.MethodPost, "/api/rebuttal", rebuttalRequestBody(t, map[string]any{"job_id": jobID, "finding_id": findings[0].ID, "commenter": "reader", "comment": "Check the guard."}))
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var job storage.ReviewJob
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &job))
	capture, err := db.Reassessment(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Nil(t, capture.Context.Delivery)
	assert.Equal(t, findings[0].ID, capture.Context.Selector)
	assert.NotEmpty(t, capture.Context.Finding.DocumentHash)
	assert.Equal(t, "reader", capture.Context.Requester)
	assert.Equal(t, "local", capture.Context.Policy)
}

func TestKataResponseMissingMappingAndLocalEvidenceValidation(t *testing.T) {
	s, db, _ := newTestServer(t)
	jobID, findings := rebuttalFindingParent(t, s, db)
	s.configWatcher.Config().Integrations.Kata.Enabled = true
	intent := kataresponse.Request{JobID: jobID, OperationID: uuid.New().String(), FindingID: findings[0].ID, Kind: "reply", Body: "Evidence"} //nolint:forbidigo // Synthetic daemon/provider operation and review UUID text boundaries.
	raw, err := json.Marshal(intent)
	require.NoError(t, err)
	rr := serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	intent.Kind = "confirm"
	intent.Body = strings.Repeat("界", 39)
	raw, err = json.Marshal(intent)
	require.NoError(t, err)
	rr = serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	handoffs, err := db.KataHandoffs(t.Context(), jobID)
	require.NoError(t, err)
	assert.Empty(t, handoffs)
}

func TestReassessmentShortEvidenceCompletesLocallyAndKeepsPendingKind(t *testing.T) {
	for _, policy := range []string{"comments", "summary"} {
		t.Run(policy, func(t *testing.T) {
			s, db, _ := newTestServer(t)
			jobID, findings := rebuttalFindingParent(t, s, db)
			seedResponseMapping(t, s, db, jobID, findings[0].ID, policy)
			s.configWatcher.Config().Integrations.Kata.PublishReassessments = true
			rr := serveHuma(t, s, http.MethodPost, "/api/rebuttal", rebuttalRequestBody(t, map[string]any{"job_id": jobID, "finding_id": findings[0].ID, "commenter": "reader", "comment": "Check this."}))
			require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
			var job storage.ReviewJob
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &job))
			_, err := db.Exec(`UPDATE review_jobs SET status='running' WHERE id=?`, job.ID)
			require.NoError(t, err)
			require.NoError(t, db.CompleteJob(job.ID, "test", `{"outcome":"refuted","explanation":"Guarded."}`))
			result, err := db.Reassessment(t.Context(), job.ID)
			require.NoError(t, err)
			require.NotNil(t, result.Assessment)
			handoffs, err := db.KataHandoffs(t.Context(), job.ID)
			require.NoError(t, err)
			require.Len(t, handoffs, 1)
			response := handoffs[0].Request.Responses[0]
			assert.Equal(t, "refute", response.Kind)
			assert.Equal(t, "pending", handoffs[0].Status)
			require.ErrorIs(t, kata.ValidateEvidence(kata.CommentKind(response.Kind), response.Body), kata.ErrEvidenceTooShort)
		})
	}
}

func TestReassessmentSupersededWithoutReplacementPublishesReply(t *testing.T) {
	s, db, _ := newTestServer(t)
	jobID, findings := rebuttalFindingParent(t, s, db)
	seedResponseMapping(t, s, db, jobID, findings[0].ID, "comments")
	s.configWatcher.Config().Integrations.Kata.PublishReassessments = true
	rr := serveHuma(t, s, http.MethodPost, "/api/rebuttal", rebuttalRequestBody(t, map[string]any{"job_id": jobID, "finding_id": findings[0].ID, "commenter": "reader", "comment": "Reassess this."}))
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var job storage.ReviewJob
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &job))
	_, err := db.Exec(`UPDATE review_jobs SET status='running' WHERE id=?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, db.CompleteJob(job.ID, "test", `{"outcome":"superseded","explanation":"The selected path has been removed."}`))
	handoffs, err := db.KataHandoffs(t.Context(), job.ID)
	require.NoError(t, err)
	require.Len(t, handoffs, 1)
	assert.Equal(t, "reply", handoffs[0].Request.Responses[0].Kind)
}

func TestKataResponseStoresCompleteOversizedEvidence(t *testing.T) {
	s, db, _ := newTestServer(t)
	jobID, findings := rebuttalFindingParent(t, s, db)
	seedResponseMapping(t, s, db, jobID, findings[0].ID, "comments")
	evidence := strings.Repeat("x", kata.MaxRequestBytes+1)
	intent := kataresponse.Request{JobID: jobID, OperationID: uuid.New().String(), FindingID: findings[0].ID, Kind: "reply", Body: evidence} //nolint:forbidigo // Daemon operation ID wire boundary.
	raw, err := json.Marshal(intent)
	require.NoError(t, err)
	rr := serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
	require.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())
	handoffs, err := db.KataHandoffs(t.Context(), jobID)
	require.NoError(t, err)
	var stored string
	for _, h := range handoffs {
		if h.OperationID == intent.OperationID {
			stored = h.Request.Responses[0].Body
		}
	}
	assert.Equal(t, evidence, stored, "provider rejection must not clip or discard the stored operation")
}

func TestReassessmentOfLocalSynthesisKeepsSessionAndDeliveryGuards(t *testing.T) {
	for _, guard := range []string{"available", "missing session", "isolated", "ci", "closed"} {
		t.Run(guard, func(t *testing.T) {
			s, db, _ := newTestServer(t)
			jobID, findings := rebuttalFindingParent(t, s, db)
			_, err := db.Exec(`UPDATE review_jobs SET job_type='synthesis',panel_role='synthesis' WHERE id=?`, jobID)
			require.NoError(t, err)
			_, err = db.Exec(`UPDATE reviews SET agent='test' WHERE job_id=?`, jobID)
			require.NoError(t, err)
			seedResponseMapping(t, s, db, jobID, findings[0].ID, "comments")
			s.configWatcher.Config().Integrations.Kata.PublishReassessments = true
			want := ""
			switch guard {
			case "missing session":
				_, err = db.Exec(`UPDATE review_jobs SET session_id=NULL WHERE id=?`, jobID)
				want = "stored session"
			case "isolated":
				_, err = db.Exec(`UPDATE review_jobs SET session_isolated=1 WHERE id=?`, jobID)
				want = "isolated review sessions"
			case "ci":
				_, err = db.Exec(`UPDATE review_jobs SET source='ci' WHERE id=?`, jobID)
				want = "completed local code review"
			case "closed":
				err = db.MarkReviewClosedByJobID(jobID, true)
				want = "open review"
			}
			require.NoError(t, err)
			rr := serveHuma(t, s, http.MethodPost, "/api/rebuttal", rebuttalRequestBody(t, map[string]any{"job_id": jobID, "finding_id": findings[0].ID, "commenter": "reader", "comment": "Recheck the synthesized finding."}))
			if guard != "available" {
				require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
				assert.Contains(t, rr.Body.String(), want)
				var count int
				require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM reassessments`).Scan(&count))
				assert.Zero(t, count)
				return
			}
			require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
			var queued storage.ReviewJob
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &queued))
			assert.Equal(t, "original-session", queued.SessionID)
			assert.True(t, queued.ReassessmentDurable)
			_, err = db.Exec(`UPDATE review_jobs SET status='running' WHERE id=?`, queued.ID)
			require.NoError(t, err)
			require.NoError(t, db.CompleteJob(queued.ID, "test", `{"outcome":"confirmed","explanation":"The synthesized finding still reproduces on the selected code path."}`))
			handoffs, err := db.KataHandoffs(t.Context(), queued.ID)
			require.NoError(t, err)
			require.Len(t, handoffs, 1)
			response := handoffs[0].Request.Responses[0]
			assert.Equal(t, "confirm", response.Kind)
			assert.Equal(t, "root-uid", response.Target)
			review, err := db.GetReviewByJobID(jobID)
			require.NoError(t, err)
			assert.False(t, review.Closed)
			assert.Equal(t, queued.OriginalReviewUUID, review.UUID)
		})
	}
}

func TestKataResponseReasonFrozenAfterDisclosureAndExactRetry(t *testing.T) {
	for _, policy := range []string{"comments", "summary"} {
		for _, reason := range []string{"false-positive", "spurious", "risk-accepted"} {
			t.Run(policy+"/"+reason, func(t *testing.T) {
				s, db, _ := newTestServer(t)
				jobID, findings := rebuttalFindingParent(t, s, db)
				seedResponseMapping(t, s, db, jobID, findings[0].ID, policy)
				intent := kataresponse.Request{JobID: jobID, OperationID: "reason-attempt", FindingID: findings[0].ID, Kind: "refute", Body: "Private evidence: " + strings.Repeat("界", 40), Reason: &reason}
				raw, err := json.Marshal(intent)
				require.NoError(t, err)
				rr := serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
				require.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())
				handoffs, err := db.KataHandoffs(t.Context(), jobID)
				require.NoError(t, err)
				var frozen string
				for _, h := range handoffs {
					if len(h.Request.Responses) > 0 {
						frozen = h.Request.Responses[0].Body
					}
				}
				require.NotEmpty(t, frozen)
				expected := intent.Body
				if policy == "summary" {
					expected = fmt.Sprintf("refute response for finding %s. Evidence is available locally under operation reason-attempt.", findings[0].ID)
				}
				assert.Equal(t, "<!-- roborev-refute:v1 reason="+reason+" -->\n"+expected, frozen)
				saved, err := db.KataResponseIntent(t.Context(), intent.OperationID)
				require.NoError(t, err)
				assert.Equal(t, intent, saved)
				s.configWatcher.Config().Integrations.Kata.Enabled = false
				rr = serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
				assert.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())
				changed := "spurious"
				if reason == changed {
					changed = "false-positive"
				}
				intent.Reason = &changed
				raw, err = json.Marshal(intent)
				require.NoError(t, err)
				rr = serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
				assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
			})
		}
	}
}

func TestKataResponseRejectsInvalidReasonBeforeMapping(t *testing.T) {
	s, _, _ := newTestServer(t)
	for _, payload := range []string{
		`"kind":"refute","reason":""`, `"kind":"refute","reason":"unknown"`,
		`"kind":"reply","reason":"spurious"`, `"kind":"confirm","reason":"spurious"`,
		`"kind":"supersede","reason":"spurious"`,
	} {
		raw := []byte(`{"operation_id":"invalid-reason","finding_id":"c:abcdef","body":"` + strings.Repeat("界", 40) + `",` + payload + `}`)
		rr := serveHuma(t, s, http.MethodPost, "/api/kata/response", raw)
		assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}
