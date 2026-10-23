package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/katacontext"
	"go.kenn.io/roborev/internal/storage"
)

func TestKataShowIssuePreservesAttributedClaims(t *testing.T) {
	const ref = "reviews#example"
	body := katacontext.KataReplyContext{IssueRef: ref, Snapshots: []katacontext.Snapshot{{
		IssueRef: ref, ReviewUUID: "review-example", Findings: []katacontext.Finding{{
			Selector: "f:example", Availability: "available", Replies: []kata.Comment{{
				UID: "reply-example", Author: "reviewer", Teammate: "helper", Kind: "reply",
				Body: "Committed a proposed fix; validate the claim independently.",
			}},
		}},
	}}}
	NewMockDaemon(t, MockRefineHooks{OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
		if r.URL.Path != "/api/kata/context" {
			return false
		}
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, ref, r.URL.Query().Get("ref"))
		require.NoError(t, json.MarshalWrite(w, body))
		return true
	}})
	for _, asJSON := range []bool{false, true} {
		cmd := kataCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		args := []string{"show", "--issue", ref}
		if asJSON {
			args = append(args, "--json")
		}
		cmd.SetArgs(args)
		require.NoError(t, cmd.Execute())
		if asJSON {
			var got kataShowView
			require.NoError(t, json.Unmarshal(out.Bytes(), &got))
			assert.Equal(t, body.IssueRef, got.IssueRef)
			assert.Equal(t, body.Snapshots, got.Snapshots)
		} else {
			assert.Contains(t, out.String(), "reviewer")
			assert.Contains(t, out.String(), "helper")
			assert.Contains(t, out.String(), "validate the claim independently")
			assert.Contains(t, out.String(), "untrusted")
		}
	}
}

func TestKataShowCombinesSavedPublicationRepliesAndPendingStatus(t *testing.T) {
	publication := daemon.KataPublication{OperationID: "publication-example", ReviewUUID: "review-example", Issue: "reviews#example", State: "done", Policy: "comments", Findings: []daemon.KataFindingPublication{}}
	operation := storage.KataResponseStatus{OperationID: "Readback-Case-Key", State: "pending", Kind: "reply", Target: "root-example", Message: "delivery pending"}
	NewMockDaemon(t, MockRefineHooks{OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
		switch r.URL.Path {
		case "/api/kata/publications":
			assert.Equal(t, "42", r.URL.Query().Get("job_id"))
			require.NoError(t, json.MarshalWrite(w, daemon.KataPublicationsBody{JobID: 42, Publications: []daemon.KataPublication{publication}, Operations: []storage.KataResponseStatus{operation}}))
			return true
		case "/api/kata/context":
			assert.Equal(t, "review-example", r.URL.Query().Get("review_uuid"))
			assert.NoError(t, json.MarshalWrite(w, katacontext.KataReplyContext{JobID: 42, Snapshots: []katacontext.Snapshot{{ReviewUUID: "review-example", IssueRef: "reviews#example"}}, Operations: []storage.KataResponseStatus{operation}}))
		default:
			return false
		}
		return true
	}})
	cmd := kataCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"show", "--job", "42", "--json"})
	require.NoError(t, cmd.Execute())
	var got kataShowView
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Equal(t, int64(42), got.JobID)
	assert.Equal(t, []daemon.KataPublication{publication}, got.Publications)
	require.Len(t, got.Snapshots, 1)
	assert.Equal(t, "review-example", got.Snapshots[0].ReviewUUID)
	assert.Equal(t, []storage.KataResponseStatus{operation}, got.Operations, "the same safe pending status must not be duplicated")
}

func TestKataShowKeepsSavedDeliveryWhenRepliesUnavailable(t *testing.T) {
	publication := daemon.KataPublication{OperationID: "publication-example", ReviewUUID: "review-example", Issue: "reviews#example", State: "pending", Policy: "summary", Findings: []daemon.KataFindingPublication{}}
	NewMockDaemon(t, MockRefineHooks{OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
		switch r.URL.Path {
		case "/api/kata/publications":
			require.NoError(t, json.MarshalWrite(w, daemon.KataPublicationsBody{JobID: 42, Publications: []daemon.KataPublication{publication}}))
			return true
		case "/api/kata/context":
			http.Error(w, "synthetic-provider-detail", http.StatusServiceUnavailable)
		default:
			return false
		}
		return true
	}})
	cmd := kataCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"show", "--job", "42", "--json"})
	require.NoError(t, cmd.Execute())
	var got kataShowView
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Equal(t, []daemon.KataPublication{publication}, got.Publications)
	assert.True(t, got.ContextUnavailable)
	assert.Empty(t, got.Snapshots)
	assert.NotContains(t, out.String(), "synthetic-provider-detail")
}

func TestKataShowRejectsAmbiguousEntryPoints(t *testing.T) {
	for _, args := range [][]string{{"show", "--issue", "reviews#example", "42"}, {"show", "--issue", "reviews#example", "--job"}} {
		cmd := kataCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		require.Error(t, cmd.Execute())
	}
}

func TestKataShowIssueRejectsAnotherIssueContext(t *testing.T) {
	NewMockDaemon(t, MockRefineHooks{OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
		if r.URL.Path != "/api/kata/context" {
			return false
		}
		assert.NoError(t, json.MarshalWrite(w, katacontext.KataReplyContext{IssueRef: "reviews#different", Snapshots: []katacontext.Snapshot{{IssueRef: "reviews#different"}}}))
		return true
	}})
	cmd := kataCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"show", "--issue", "reviews#example", "--json"})
	require.ErrorContains(t, cmd.Execute(), "another issue")
	assert.NotContains(t, out.String(), "reviews#different")
}

func TestKataShowIssueKeepsPendingPublicationWithoutReplies(t *testing.T) {
	publication := daemon.KataPublication{OperationID: "publication-example", Issue: "reviews#example", IssueUID: "issue-example", State: "pending", Policy: "summary", LastError: "Kata publication pending"}
	NewMockDaemon(t, MockRefineHooks{OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
		if r.URL.Path != "/api/kata/context" {
			return false
		}
		assert.NoError(t, json.MarshalWrite(w, katacontext.KataReplyContext{IssueRef: "reviews#example", Publications: []katacontext.KataPublication{publication}, Message: "Kata finding reply context is disabled"}))
		return true
	}})
	cmd := kataCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"show", "--issue", "reviews#example"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "publication-example")
	assert.Contains(t, out.String(), "pending (summary)")
	assert.Contains(t, out.String(), "Kata publication pending")
	assert.NotContains(t, out.String(), "No available saved")
}

func TestKataPromptReaderFreezesUnavailableFirstObservation(t *testing.T) {
	requests := 0
	NewMockDaemon(t, MockRefineHooks{OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
		if r.URL.Path != "/api/kata/context" {
			return false
		}
		requests++
		if requests == 1 {
			http.Error(w, "synthetic interruption", http.StatusServiceUnavailable)
			return true
		}
		var request daemon.KataConsumeContextRequest
		require.NoError(t, json.UnmarshalRead(r.Body, &request))
		require.NoError(t, json.MarshalWrite(w, katacontext.KataReplyContext{JobID: request.JobID, RunID: request.RunID, Snapshots: []katacontext.Snapshot{{ReviewUUID: request.ReviewUUID, IssueRef: "reviews#example"}}}))
		return true
	}})
	reviewUUID := uuid.New()
	review := &storage.Review{JobID: 42, UUID: &reviewUUID}
	reader := kataReplyPromptReader(getDaemonEndpoint().APIClient(0), "fix")
	implementation := reader(t.Context(), review)
	planning := reader(t.Context(), review)
	assert.Equal(t, implementation, planning)
	assert.Contains(t, implementation, "unavailable")
	assert.Equal(t, 1, requests, "one run must retain even an unavailable first observation")
	fresh := kataReplyPromptReader(getDaemonEndpoint().APIClient(0), "fix")(t.Context(), review)
	assert.Contains(t, fresh, "reviews#example")
	assert.Equal(t, 2, requests, "a new run may consume new evidence")
}

func TestKataConsumedContextRejectsMismatchedIdentity(t *testing.T) {
	const runID = "11111111-1111-4111-8111-111111111111"
	for _, mismatch := range []string{"job", "review", "run"} {
		t.Run(mismatch, func(t *testing.T) {
			body := katacontext.KataReplyContext{JobID: 42, RunID: runID, Snapshots: []katacontext.Snapshot{{ReviewUUID: "review-example"}}}
			switch mismatch {
			case "job":
				body.JobID = 43
			case "review":
				body.Snapshots[0].ReviewUUID = "different-review"
			case "run":
				body.RunID = "22222222-2222-4222-8222-222222222222"
			}
			NewMockDaemon(t, MockRefineHooks{OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
				if r.URL.Path != "/api/kata/context" {
					return false
				}
				assert.Equal(t, http.MethodPost, r.Method)
				var request daemon.KataConsumeContextRequest
				require.NoError(t, json.UnmarshalRead(r.Body, &request))
				assert.Equal(t, runID, request.RunID)
				assert.Equal(t, "fix", request.Purpose)
				require.NoError(t, json.MarshalWrite(w, body))
				return true
			}})
			got, err := fetchKataReplyContext(t.Context(), getDaemonEndpoint().APIClient(0), 42, "review-example", "", runID, "fix")
			require.ErrorContains(t, err, "another "+mismatch)
			assert.Empty(t, got.Snapshots, "mismatched claims must never reach a prompt")
		})
	}
}

// Bundled skills put the inherited flag immediately after the command group.
// Exercise Cobra and the actual typed-response request with that invocation.
func TestKataSkillFlagBeforeCommentReachesTypedResponse(t *testing.T) {
	previous := fromSkill
	t.Cleanup(func() { fromSkill = previous })
	called := false
	NewMockDaemon(t, MockRefineHooks{OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
		if r.URL.Path != "/api/kata/response" {
			return false
		}
		called = true
		assert.True(t, fromSkill)
		assert.Equal(t, http.MethodPost, r.Method)
		var request struct {
			FindingID   string `json:"finding_id"`
			OperationID string `json:"operation_id"`
		}
		require.NoError(t, json.UnmarshalRead(r.Body, &request))
		assert.Equal(t, "c:abcdef", request.FindingID)
		assert.Equal(t, "Skill-Key", request.OperationID)
		w.WriteHeader(http.StatusAccepted)
		require.NoError(t, json.MarshalWrite(w, storage.KataResponseStatus{OperationID: request.OperationID, State: "pending", Kind: "reply"}))
		return true
	}})
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"kata", "--from-skill", "comment", "--reply", "c:abcdef", "-m", "Proposed change is committed; validate the evidence.", "--idempotency-key", "Skill-Key"})
	require.NoError(t, cmd.Execute(), out.String())
	assert.True(t, called)
	assert.Contains(t, out.String(), "Skill-Key")
}
