package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghpkg "go.kenn.io/roborev/internal/github"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
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
					_, err := h.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ? WHERE job_id = ?`, `{"schema_version":0,"legacy":{"markdown":"No issues found.","recorded_verdict":true}}`, synth.ID)
					require.NoError(t, err)
				} else {
					_, err := h.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ?, verdict_bool = 0 WHERE job_id = ?`, `{"schema_version":2,"summary":"Stored review.","verdict":"fail","findings":[{"severity":"low","problem":"Minor concern.","fix":"Fix it.","location":null}]}`, synth.ID)
					require.NoError(t, err)
				}
			}
			_, err := h.DB.ExecContext(t.Context(), `UPDATE review_jobs SET min_severity = 'high' WHERE id = ?`, synth.ID)
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
				reviewed, _, err := h.Poller.alreadyReviewedPR("acme/api", ghPR{Number: 7, HeadRefOid: "head-a"})
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
	_, err := h.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ?, verdict_bool = 0 WHERE job_id = ?`, `{"schema_version":2,"summary":"Notes.","verdict":"fail","findings":[{"severity":"medium","problem":"Minor concern.","fix":"Fix it.","location":null},{"severity":"high","problem":"State is lost.","fix":"Persist it.","location":null}]}`, synth.ID)
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
	_, err := h.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ? WHERE job_id = ?`, `{"schema_version":2,"summary":"Clean.","verdict":"pass","findings":[]}`, synth.ID)
	require.NoError(t, err)
	h.Poller.handleReviewCompleted(ciEvent(synth.ID, "review.completed"))
	assert.Empty(t, *comments)
	// Successful suppression must clear stale attempt failures when rerunning.
	_, err = h.DB.ExecContext(t.Context(), `UPDATE ci_pr_review_attempts SET last_error_class='genuine',last_error_excerpt='old error' WHERE github_repo='acme/api'`)
	require.NoError(t, err)
	server := newServerWithLogs(h.DB, h.Cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	rerun, err := server.humaRerunJob(t.Context(), &RerunJobInput{Body: RerunJobRequest{JobID: synth.ID}})
	require.NoError(t, err)
	attempt, err := h.DB.GetReviewAttempt("acme/api", 7, "head-a")
	require.NoError(t, err)
	assert.Empty(t, attempt.LastErrorClass)
	assert.Empty(t, attempt.LastErrorExcerpt)
	_, err = server.humaCancelJob(t.Context(), &CancelJobInput{Body: CancelJobRequest{JobID: rerun.Body.JobID}})
	require.NoError(t, err)
	reviewed, _, err := h.Poller.alreadyReviewedPR("acme/api", ghPR{Number: 7, HeadRefOid: "head-a"})
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
		{Severity: "medium", Problem: "Omitted concern.", Fix: "Fix it."},
	}}
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	_, err = h.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ?, verdict_bool = 0 WHERE job_id = ?`, string(raw), synth.ID)
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

func TestCIPanelCommentMinConfidence(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	floor := 0.8
	h.Cfg.CI.CommentMinConfidence = &floor
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a", []jobSpec{{Agent: "test", Status: "done", Output: "Review notes."}})
	document := reviewpkg.StructuredReview{SchemaVersion: 4, Summary: "Complete review.", Verdict: "fail", Findings: []reviewpkg.StructuredFinding{
		{Severity: "high", Kind: "defect", Confidence: 0.5, Problem: "Filtered finding.", Fix: "Fix it."},
		{Severity: "high", Kind: "defect", Confidence: 0.9, Problem: "Published finding.", Fix: "Fix it."},
	}}
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, h.DB.MaybeReleasePanelSynthesisContext(t.Context(), panel.PanelRunUUID))
	claimed, err := h.DB.ClaimJobContext(t.Context(), "fixture-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, synth.ID, claimed.ID)
	require.NoError(t, testutil.CompleteReviewFixture(h.DB, synth.ID, synth.Agent, "", string(raw)))
	members, err := h.DB.GetPanelMemberReviews(panel.PanelRunUUID)
	require.NoError(t, err)
	body, err := h.Poller.panelCommentBody(panel, members)
	require.NoError(t, err)
	assert.Contains(t, body, "Published finding.")
	assert.NotContains(t, body, "Filtered finding.")
}

func TestCICommentRemembersID(t *testing.T) {
	for _, pages := range []int{1, 10} {
		t.Run(fmt.Sprintf("pages=%d", pages), func(t *testing.T) {
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			t.Setenv("GH_TOKEN", "test-token")
			h.Cfg.CI.UpsertComments = true
			var requests []string
			comments := []int{pages * 100}
			nextID := pages * 100
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := strings.TrimPrefix(r.URL.Path, "/api/v3")
				requests = append(requests, r.Method+" "+path)
				w.Header().Set("Content-Type", "application/json")
				switch r.Method {
				case http.MethodGet:
					page := r.URL.Query().Get("page")
					if page == "" {
						page = "1"
					}
					n, err := strconv.Atoi(page)
					assert.NoError(t, err)
					all := make([]map[string]any, 0, pages*100-1+len(comments))
					for i := 0; i < pages*100-1; i++ {
						all = append(all, map[string]any{"id": i + 1, "body": "ordinary"})
					}
					for _, id := range comments {
						all = append(all, map[string]any{"id": id, "body": ghpkg.CommentMarker})
					}
					start := (n - 1) * 100
					end := min(start+100, len(all))
					if end < len(all) {
						w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d&per_page=100>; rel="next"`, r.Host, r.URL.Path, n+1))
					}
					assert.NoError(t, json.NewEncoder(w).Encode(all[start:end]))
				case http.MethodPatch:
					id, err := strconv.Atoi(path[strings.LastIndex(path, "/")+1:])
					assert.NoError(t, err)
					if !slices.Contains(comments, id) {
						w.WriteHeader(http.StatusNotFound)
					}
					_, _ = fmt.Fprint(w, `{}`)
				case http.MethodPost:
					nextID++
					comments = append(comments, nextID)
					w.WriteHeader(http.StatusCreated)
					_, _ = fmt.Fprintf(w, `{"id":%d}`, comments[len(comments)-1])
				}
			}))
			defer api.Close()
			h.Poller.githubAPIURL = api.URL
			require.NoError(t, h.Poller.postPRComment("acme/api", 7, "first"))
			require.Len(t, requests, pages+1)
			for _, request := range requests[:pages] {
				assert.Equal(t, "GET /repos/acme/api/issues/7/comments", request)
			}
			assert.Equal(t, fmt.Sprintf("PATCH /repos/acme/api/issues/comments/%d", pages*100), requests[pages])
			requests = nil
			require.NoError(t, h.Poller.postPRComment("acme/api", 7, "second"))
			updated, err := h.Poller.updateExistingPRComment("acme/api", 7, "clean")
			require.NoError(t, err)
			require.True(t, updated)
			assert.Equal(t, []string{fmt.Sprintf("PATCH /repos/acme/api/issues/comments/%d", pages*100), fmt.Sprintf("PATCH /repos/acme/api/issues/comments/%d", pages*100)}, requests)
			requests = nil
			h.Poller = NewCIPoller(h.DB, NewStaticConfig(h.Cfg), nil)
			h.Poller.githubAPIURL = api.URL
			require.NoError(t, h.Poller.postPRComment("acme/api", 7, "restart"))
			require.Len(t, requests, pages+1)
			for _, request := range requests[:pages] {
				assert.Equal(t, "GET /repos/acme/api/issues/7/comments", request)
			}
			assert.Equal(t, fmt.Sprintf("PATCH /repos/acme/api/issues/comments/%d", pages*100), requests[pages])
			requests = nil
			comments = nil
			require.NoError(t, h.Poller.postPRComment("acme/api", 7, "replacement"))
			require.NoError(t, h.Poller.postPRComment("acme/api", 7, "next"))
			require.Len(t, requests, pages+3)
			assert.Equal(t, fmt.Sprintf("PATCH /repos/acme/api/issues/comments/%d", pages*100), requests[0])
			for _, request := range requests[1 : pages+1] {
				assert.Equal(t, "GET /repos/acme/api/issues/7/comments", request)
			}
			assert.Equal(t, "POST /repos/acme/api/issues/7/comments", requests[pages+1])
			assert.Equal(t, fmt.Sprintf("PATCH /repos/acme/api/issues/comments/%d", pages*100+1), requests[pages+2])
			requests = nil
			h.Cfg.CI.UpsertComments = false
			require.NoError(t, h.Poller.postPRComment("acme/api", 7, "new"))
			h.Cfg.CI.UpsertComments = true
			require.NoError(t, h.Poller.postPRComment("acme/api", 7, "after toggle"))
			require.Len(t, requests, pages+3)
			assert.Equal(t, "POST /repos/acme/api/issues/7/comments", requests[0])
			for _, request := range requests[1 : pages+2] {
				assert.Equal(t, "GET /repos/acme/api/issues/7/comments", request)
			}
			assert.Equal(t, fmt.Sprintf("PATCH /repos/acme/api/issues/comments/%d", pages*100+2), requests[pages+2])
		})
	}
}

func TestCIPanelCommentFoldAndShowAll(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a", []jobSpec{{Agent: "test", Status: "done", Output: "Review notes."}})
	h.completeSynthesisWithReview(t, synth.ID, "Review notes.")
	document := `{"schema_version":4,"summary":"Notes.","verdict":"fail","findings":[{"severity":"medium","kind":"defect","confidence":0.9,"problem":"defect","fix":"fix"},{"severity":"low","kind":"improvement","confidence":0.9,"problem":"naming issue","fix":"fix","nit":true},{"severity":"high","kind":"defect","confidence":0.9,"problem":"rare","fix":"fix","unlikely":true},{"severity":"medium","kind":"defect","confidence":0.9,"problem":"rare medium","fix":"fix","unlikely":true}]}`
	_, err := h.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ?, verdict_bool = 0 WHERE job_id = ?`, document, synth.ID)
	require.NoError(t, err)
	_, err = h.DB.ExecContext(t.Context(), `UPDATE review_jobs SET min_severity = 'medium' WHERE id = ?`, synth.ID)
	require.NoError(t, err)
	members, err := h.DB.GetPanelMemberReviews(panel.PanelRunUUID)
	require.NoError(t, err)
	body, err := h.Poller.panelCommentBody(panel, members)
	require.NoError(t, err)
	assert.Contains(t, body, "<details>")
	h.Cfg.CI.CommentShowAll = true
	body, err = h.Poller.panelCommentBody(panel, members)
	require.NoError(t, err)
	assert.NotContains(t, body, "<details>")
	assert.Contains(t, body, "naming issue")
}

func TestCIPanelCommentStructuredVerdict(t *testing.T) {
	for _, tc := range []struct {
		name, severity, floor, guidance, count string
		unlikely, showAll                      bool
		verdictBool                            int
	}{
		{"folded failure", "medium", "medium", "Changes require fixes before merge.", "0 findings shown; 1 folded finding", true, false, 0},
		{"show all below floor", "low", "high", "No fixes needed before merge.", "1 finding shown; 0 folded findings", false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			h.Cfg.CI.CommentShowAll = tc.showAll
			panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 7, "head-a", "base..head-a", []jobSpec{{Agent: "test", Status: "done", Output: "Review notes."}})
			h.completeSynthesisWithReview(t, synth.ID, "Review notes.")
			doc := reviewpkg.StructuredReview{Summary: "Notes.", Verdict: "fail", Findings: []reviewpkg.StructuredFinding{{Severity: tc.severity, Problem: "Finding", Fix: "Fix", Unlikely: tc.unlikely}}}
			data, err := json.Marshal(doc)
			require.NoError(t, err)
			_, err = h.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ?, verdict_bool = ? WHERE job_id = ?`, string(data), tc.verdictBool, synth.ID)
			require.NoError(t, err)
			_, err = h.DB.ExecContext(t.Context(), `UPDATE review_jobs SET min_severity = ? WHERE id = ?`, tc.floor, synth.ID)
			require.NoError(t, err)
			members, err := h.DB.GetPanelMemberReviews(panel.PanelRunUUID)
			require.NoError(t, err)
			body, err := h.Poller.panelCommentBody(panel, members)
			require.NoError(t, err)
			assert.Contains(t, body, tc.guidance)
			assert.Contains(t, body, tc.count)
			assert.Contains(t, body, "Finding")
		})
	}
}

func TestCIPanelCommentTruncationDropsFold(t *testing.T) {
	doc := reviewpkg.StructuredReview{Findings: []reviewpkg.StructuredFinding{{Severity: "low", Nit: true, Problem: strings.Repeat("x", 100)}}}
	body, foldAt := reviewpkg.FormatCommentWithFold(reviewpkg.PrepareComment(reviewpkg.CommentConfig{}, reviewpkg.ReviewResult{Structured: &doc}))
	got := truncatePanelPRBodyForFooter(body, foldAt, "footer", 150)
	assert.Equal(t, body[:foldAt.Start]+panelCommentTruncSuffix, got)
}

func TestCIPanelCommentTruncationDropsFoldedFindings(t *testing.T) {
	doc := reviewpkg.StructuredReview{Findings: []reviewpkg.StructuredFinding{
		{Severity: "low", Nit: true, Problem: "Keep this finding.", Fix: "Fix it."},
		{Severity: "low", Nit: true, Problem: strings.Repeat("oversized ", reviewpkg.MaxCommentLen), Fix: "Fix it."},
	}}
	prepared := reviewpkg.PrepareComment(reviewpkg.CommentConfig{}, reviewpkg.ReviewResult{Structured: &doc})
	stored := &storage.Review{Job: &storage.ReviewJob{Agent: "test"}}
	got := formatPreparedPanelPRCommentWithHead(prepared, stored, "F", nil, false, "abcdef123456")
	assert.NotContains(t, got, "Keep this finding.")
	assert.NotContains(t, got, "oversized")
	assert.Contains(t, got, panelCommentTruncSuffix)
	assert.LessOrEqual(t, len(got), reviewpkg.MaxCommentLen)
}

func TestCIPanelCommentTruncationPreservesVisibleExample(t *testing.T) {
	example := "```html\n<details><summary>Example</summary>\ncontent\n</details>\n```"
	doc := reviewpkg.StructuredReview{Findings: []reviewpkg.StructuredFinding{
		{Severity: "high", Problem: example, Fix: "Fix it."},
		{Severity: "low", Nit: true, Problem: strings.Repeat("nit ", reviewpkg.MaxCommentLen), Fix: "Fix it."},
	}}
	prepared := reviewpkg.PrepareComment(reviewpkg.CommentConfig{}, reviewpkg.ReviewResult{Structured: &doc})
	stored := &storage.Review{Job: &storage.ReviewJob{Agent: "test"}}
	got := formatPreparedPanelPRCommentWithHead(prepared, stored, "F", nil, false, "abcdef123456")
	assert.Contains(t, got, example)
	assert.NotContains(t, got, "1 nit hidden")
	assert.Contains(t, got, "Reviewers:")
	assert.LessOrEqual(t, len(got), reviewpkg.MaxCommentLen)
}
