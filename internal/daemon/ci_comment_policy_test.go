package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghpkg "go.kenn.io/roborev/internal/github"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

// Exercise real GitHub HTTP delivery: silence may patch a stale marker, but
// may never POST a replacement, and HTTP failures retain normal finalization.
func TestCIPanelCommentPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                                                 string
		upsert, existing, prose, fallback, defaults, partial bool
		patchStatus                                          int
		outcome, state                                       string
		posts, patches                                       int
	}{
		{name: "silent", outcome: "review_suppressed", state: "success"},
		{name: "partial execution failure", partial: true, outcome: "review_suppressed", state: "failure"},
		{name: "upsert missing", upsert: true, outcome: "review_suppressed", state: "success"},
		{name: "upsert existing", upsert: true, existing: true, patchStatus: 200, outcome: "review_posted", state: "success", patches: 1},
		{name: "upsert deleted", upsert: true, existing: true, patchStatus: 404, outcome: "review_suppressed", state: "success", patches: 1},
		{name: "upsert forbidden", upsert: true, existing: true, patchStatus: 403, outcome: "abandoned", state: "error", patches: 1},
		{name: "upsert transient", upsert: true, existing: true, patchStatus: 500, state: "error", patches: 1},
		{name: "verbatim prose", prose: true, outcome: "review_posted", state: "success", posts: 1},
		{name: "raw fallback", fallback: true, outcome: "review_posted", state: "success", posts: 1},
		{name: "default posts", defaults: true, outcome: "review_posted", state: "success", posts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			t.Setenv("GH_TOKEN", "test-token")
			clean := false
			if !tc.defaults {
				h.Cfg.CI.CommentWhenClean = &clean
			}
			h.Cfg.CI.CommentMaxFindings = 1
			h.Cfg.CI.UpsertComments = tc.upsert
			statuses := h.CaptureCommitStatuses()
			var posts, patches int
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Method {
				case http.MethodGet:
					if tc.existing {
						_, _ = fmt.Fprint(w, `[{"id":42,"body":"<!-- roborev-pr-comment --> stale"}]`)
					} else {
						_, _ = fmt.Fprint(w, `[]`)
					}
				case http.MethodPatch:
					patches++
					var body struct{ Body string }
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					assert.Contains(t, body.Body, "No findings at or above high severity.")
					w.WriteHeader(tc.patchStatus)
					if tc.patchStatus == 403 {
						_, _ = fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
					} else {
						_, _ = fmt.Fprint(w, `{"id":42}`)
					}
				case http.MethodPost:
					posts++
					w.WriteHeader(http.StatusCreated)
					_, _ = fmt.Fprint(w, `{"id":43}`)
				default:
					assert.Contains(t, []string{"GET", "POST", "PATCH"}, r.Method)
				}
			}))
			defer api.Close()
			h.Poller.githubAPIURL = api.URL
			h.Poller.postPRCommentFn = nil
			specs := []jobSpec{{Agent: "test", Status: "done", Output: "### Low\nMinor concern."}}
			if tc.partial {
				specs = append(specs, jobSpec{Agent: "test", Status: "failed", Error: "reviewer failed"})
			}
			panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a", specs)
			if tc.fallback {
				h.markJobFailed(t, synth.ID, "synthesis unavailable")
			} else {
				h.completeSynthesisWithReview(t, synth.ID, "No issues found.")
				if tc.prose {
					_, err := h.DB.Exec(`UPDATE reviews SET structured_output = ? WHERE job_id = ?`, `{"schema_version":0,"legacy":{"markdown":"No issues found.","recorded_verdict":true}}`, synth.ID)
					require.NoError(t, err)
				} else {
					_, err := h.DB.Exec(`UPDATE reviews SET structured_output = ?, verdict_bool = 0 WHERE job_id = ?`, `{"schema_version":2,"summary":"Stored review.","verdict":"fail","findings":[{"severity":"low","problem":"Minor concern.","fix":"Fix it.","location":null}]}`, synth.ID)
					require.NoError(t, err)
				}
			}
			_, err := h.DB.Exec(`UPDATE review_jobs SET min_severity = 'high' WHERE id = ?`, synth.ID)
			require.NoError(t, err)
			won, err := h.DB.ClaimPanelForPosting(panel.ID, panelPostingStaleWindow)
			require.NoError(t, err)
			require.True(t, won)
			members, err := h.DB.GetPanelMemberReviews(panel.PanelRunUUID)
			require.NoError(t, err)
			h.Poller.postPanelComment(panel, members)
			assert.Equal(t, tc.posts, posts)
			assert.Equal(t, tc.patches, patches)
			stored, err := h.DB.GetCIPanelByPRSHA("acme/api", 7, "head-a")
			require.NoError(t, err)
			if tc.outcome != "" {
				require.NotNil(t, stored.Outcome)
				assert.Equal(t, tc.outcome, *stored.Outcome)
				require.NotNil(t, stored.PostedAt)
			} else {
				assert.Nil(t, stored.PostedAt)
			}
			require.NotEmpty(t, *statuses)
			assert.Equal(t, tc.state, (*statuses)[len(*statuses)-1].State)
			if !tc.prose && !tc.fallback {
				rev, err := h.DB.GetReviewByJobID(synth.ID)
				require.NoError(t, err)
				assert.Equal(t, storage.VerdictFail, rev.Verdict())
				assert.Contains(t, rev.Output, "Minor concern.")
			}
			if tc.outcome == "review_suppressed" {
				attempt, err := h.DB.GetReviewAttempt("acme/api", 7, "head-a")
				require.NoError(t, err)
				assert.Equal(t, "done", attempt.State)
				reviewed, err := h.Poller.alreadyReviewedPR("acme/api", ghPR{Number: 7, HeadRefOid: "head-a"})
				require.NoError(t, err)
				assert.True(t, reviewed)
				h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))
				assert.Equal(t, tc.posts, posts)
				assert.Equal(t, tc.patches, patches)
			}
		})
	}
}

func TestCIPanelCommentCapReference(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.CommentMaxFindings = 1
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a", []jobSpec{{Agent: "test", Status: "done", Output: "Review notes."}})
	h.completeSynthesisWithReview(t, synth.ID, "Review notes.")
	_, err := h.DB.Exec(`UPDATE reviews SET structured_output = ?, verdict_bool = 0 WHERE job_id = ?`, `{"schema_version":2,"summary":"Notes.","verdict":"fail","findings":[{"severity":"low","problem":"Minor concern.","fix":"Fix it.","location":null},{"severity":"high","problem":"State is lost.","fix":"Persist it.","location":null}]}`, synth.ID)
	require.NoError(t, err)
	members, err := h.DB.GetPanelMemberReviews(panel.PanelRunUUID)
	require.NoError(t, err)
	body, err := h.Poller.panelCommentBody(panel, members)
	require.NoError(t, err)
	assert.Contains(t, body, "State is lost.")
	assert.NotContains(t, body, "Minor concern.")
	assert.Contains(t, body, fmt.Sprintf("1 more finding in the full review: `roborev show --job %d`", synth.ID))
}

func TestCIPanelSuppressedRerunCancellation(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	clean := false
	h.Cfg.CI.CommentWhenClean = &clean
	comments := h.CaptureComments()
	h.CaptureCommitStatuses()
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a", []jobSpec{{Agent: "test", Status: "done", Output: "No issues found."}})
	h.completeSynthesisWithReview(t, synth.ID, "No issues found.")
	// This fixture has a structured empty result, rather than clean-looking prose.
	_, err := h.DB.Exec(`UPDATE reviews SET structured_output = ? WHERE job_id = ?`, `{"schema_version":2,"summary":"Clean.","verdict":"pass","findings":[]}`, synth.ID)
	require.NoError(t, err)
	h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))
	assert.Empty(t, *comments)
	// Successful suppression must clear stale attempt failures when rerunning.
	_, err = h.DB.Exec(`UPDATE ci_pr_review_attempts SET last_error_class='genuine',last_error_excerpt='old error' WHERE github_repo='acme/api'`)
	require.NoError(t, err)
	server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	rerun, err := server.humaRerunJob(context.Background(), &RerunJobInput{Body: RerunJobRequest{JobID: synth.ID}})
	require.NoError(t, err)
	attempt, err := h.DB.GetReviewAttempt("acme/api", 7, "head-a")
	require.NoError(t, err)
	assert.Empty(t, attempt.LastErrorClass)
	assert.Empty(t, attempt.LastErrorExcerpt)
	_, err = server.humaCancelJob(context.Background(), &CancelJobInput{Body: CancelJobRequest{JobID: rerun.Body.JobID}})
	require.NoError(t, err)
	reviewed, err := h.Poller.alreadyReviewedPR("acme/api", ghPR{Number: 7, HeadRefOid: "head-a"})
	require.NoError(t, err)
	assert.True(t, reviewed)
	original, err := h.DB.GetCIPanelBySynthesisJobID(synth.ID)
	require.NoError(t, err)
	assert.Equal(t, panel.ID, original.ID)
	assert.Equal(t, "review_suppressed", *original.Outcome)
}

// Exercise the final HTTP body, including marker and metadata footer, so no
// later transport truncation can discard the capped review's retrieval path.
func TestCIPanelCappedPublicationPreservesOmissionNotice(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.CommentMaxFindings = 1
	h.CaptureCommitStatuses()
	t.Setenv("GH_TOKEN", "test-token")
	var published string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		var payload struct {
			Body string `json:"body"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		published = payload.Body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"id":42}`)
	}))
	defer api.Close()
	h.Poller.githubAPIURL = api.URL
	h.Poller.postPRCommentFn = nil
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a", []jobSpec{{Agent: "test", Status: "done", Output: "Review notes."}})
	h.completeSynthesisWithReview(t, synth.ID, "Review notes.")
	document := reviewpkg.StructuredReview{SchemaVersion: 2, Summary: "Complete review.", Verdict: "fail", Findings: []reviewpkg.StructuredFinding{
		{Severity: "high", Problem: strings.Repeat("界", reviewpkg.MaxCommentLen), Fix: "Persist it."},
		{Severity: "low", Problem: "Omitted concern.", Fix: "Fix it."},
	}}
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	_, err = h.DB.Exec(`UPDATE reviews SET structured_output = ?, verdict_bool = 0 WHERE job_id = ?`, string(raw), synth.ID)
	require.NoError(t, err)
	before, err := h.DB.GetReviewByJobID(synth.ID)
	require.NoError(t, err)
	members, err := h.DB.GetPanelMemberReviews(panel.PanelRunUUID)
	require.NoError(t, err)
	won, err := h.DB.ClaimPanelForPosting(panel.ID, panelPostingStaleWindow)
	require.NoError(t, err)
	require.True(t, won)
	h.Poller.postPanelComment(panel, members)
	assert.LessOrEqual(t, len(published), reviewpkg.MaxCommentLen)
	assert.True(t, utf8.ValidString(published))
	assert.True(t, strings.HasPrefix(published, ghpkg.CommentMarker+"\n"))
	assert.Contains(t, published, fmt.Sprintf("1 more finding in the full review: `roborev show --job %d`", synth.ID))
	assert.Contains(t, published, "Reviewers: test")
	assert.Contains(t, published, "Synthesis: test")
	assert.NotContains(t, published, "Omitted concern.")
	after, err := h.DB.GetReviewByJobID(synth.ID)
	require.NoError(t, err)
	assert.Equal(t, before.Output, after.Output)
	assert.Equal(t, before.StructuredOutput, after.StructuredOutput)
	assert.Equal(t, storage.VerdictFail, after.Verdict())
}
