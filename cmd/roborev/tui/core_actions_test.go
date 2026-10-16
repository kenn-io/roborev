package tui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/splitlayout"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestCoreActionReviewVisits(t *testing.T) {
	enableTelemetryEnv(t)
	expectViews := func(t *testing.T, transport *recordingTransport, count int) {
		t.Helper()
		require.Len(t, transport.events, count)
		for _, body := range transport.events {
			assert.JSONEq(t, `{"event":"review_viewed","properties":{"surface":"tui"}}`, body)
		}
	}
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "interaction while loading and job switches", run: func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.update(f.review())
			expectViews(t, f.transport, 0)
			// A key while loading remains eligible when the review response arrives.
			f.m.currentReview = nil
			f.update(tea.KeyPressMsg{Code: '0'})
			f.update(f.review())
			expectViews(t, f.transport, 1)
			f.update(tea.KeyPressMsg{Code: '?'})
			require.Equal(t, viewHelp, f.m.currentView)
			f.update(tea.KeyPressMsg{Code: 'q'})
			expectViews(t, f.transport, 1)
			f.m.selectedJobID = 43
			f.update(f.review())
			f.m.selectedJobID = 42
			f.update(f.review())
			expectViews(t, f.transport, 1)
			f.update(tea.KeyPressMsg{Code: '0'})
			expectViews(t, f.transport, 2)
		}},
		{name: "pending interaction belongs to its review", run: func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.update(tea.KeyPressMsg{Code: '0'})
			f.m.selectedJobID = 43
			f.update(f.review())
			expectViews(t, f.transport, 0)
			f.m.selectedJobID = 42
			f.update(f.review())
			expectViews(t, f.transport, 0)
			f.update(tea.KeyPressMsg{Code: '0'})
			expectViews(t, f.transport, 1)
		}},
		{name: "queue interaction cannot authorize a later review", run: func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.m.currentView = viewQueue
			f.m.layout = splitlayout.Stacked
			f.update(tea.KeyPressMsg{Code: '0'})
			f.m.currentView = viewReview
			f.update(f.review())
			expectViews(t, f.transport, 0)
		}},
		{name: "same-day reload after rerun", run: func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.update(f.review())
			f.update(tea.KeyPressMsg{Code: '0'})
			expectViews(t, f.transport, 1)
			f.update(rerunResultMsg{jobID: f.m.selectedJobID})
			assert.Nil(t, f.m.currentReview)
			require.Equal(t, viewQueue, f.m.currentView)
			f.update(f.review())
			require.NotNil(t, f.m.currentReview)
			expectViews(t, f.transport, 1)
		}},
		{name: "one view per UTC day", run: func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newCoreActionReviewVisitFixture()
				f.update(tea.KeyPressMsg{Code: '0'})
				expectViews(t, f.transport, 0)
				time.Sleep(24 * time.Hour)
				f.update(f.review())
				expectViews(t, f.transport, 0)
				f.update(tea.KeyPressMsg{Code: '0'})
				expectViews(t, f.transport, 1)
				f.update(tea.KeyPressMsg{Code: '?'})
				f.update(tea.KeyPressMsg{Code: 'q'})
				expectViews(t, f.transport, 1)
				time.Sleep(24 * time.Hour)
				f.update(displayTickMsg{})
				expectViews(t, f.transport, 1)
				f.update(tea.KeyPressMsg{Code: '0'})
				expectViews(t, f.transport, 2)
			})
		}},
		{name: "quit before first interaction", run: func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.m.currentReview = splitTestReview()
			assert.Contains(t, f.update(tea.KeyPressMsg{Code: 'q'}), tea.QuitMsg{})
			expectViews(t, f.transport, 0)
		}},
		{name: "failed job has no persisted review", run: func(t *testing.T) {
			f := newCoreActionReviewVisitFixture()
			f.update(tea.KeyPressMsg{Code: 'j'})
			require.Equal(t, int64(1), f.m.selectedJobID)
			f.update(detailFollowTickMsg{gen: f.m.detailFollowGen})
			require.NotNil(t, f.m.currentReview)
			assert.Contains(t, f.m.currentReview.Output, "Job failed")
			assert.Zero(t, f.m.currentReview.ID)
			expectViews(t, f.transport, 0)
			f.update(tea.KeyPressMsg{Code: 'k'})
			f.update(f.review())
			expectViews(t, f.transport, 1)
		}},
	} {
		t.Run(tc.name, tc.run)
	}
}

type coreActionReviewVisitFixture struct {
	m         model
	transport *recordingTransport
}

func newCoreActionReviewVisitFixture() *coreActionReviewVisitFixture {
	f := &coreActionReviewVisitFixture{m: splitModel(), transport: &recordingTransport{}}
	f.m.endpoint = testEndpointFromURL("http://127.0.0.1:7373")
	f.m.client = &http.Client{Transport: f.transport}
	return f
}

func (f *coreActionReviewVisitFixture) update(msg tea.Msg) []tea.Msg {
	result, cmd := f.m.Update(msg)
	f.m = result.(model)
	return collectMsgs(cmd)
}

func (f *coreActionReviewVisitFixture) review() reviewMsg {
	review := splitTestReview()
	review.JobID, review.Job.ID = f.m.selectedJobID, f.m.selectedJobID
	return reviewMsg{review: review, jobID: f.m.selectedJobID, follow: true, gen: f.m.detailFollowGen, fetchSeq: f.m.reviewFetchSeq, attempt: f.m.jobAttemptGen[f.m.selectedJobID]}
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestCoreActionExplicitClose(t *testing.T) {
	enableTelemetryEnv(t)
	for _, tc := range []struct {
		name         string
		closed       bool
		err          error
		count        int
		programmatic bool
	}{
		{name: "close", closed: true, count: 1},
		{name: "reopen", closed: false},
		{name: "failure", closed: true, err: errors.New("failed")},
		{name: "programmatic", programmatic: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bodies []string
			closed := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/review/close" {
					closed = true
					w.WriteHeader(http.StatusOK)
					return
				}
				if r.URL.Path == "/api/telemetry/events" {
					body, _ := io.ReadAll(r.Body)
					bodies = append(bodies, string(body))
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			defer srv.Close()
			m := newModel(testEndpointFromURL(srv.URL), withExternalIODisabled())
			if tc.programmatic {
				m.jobs = []storage.ReviewJob{makeJob(42, withClosed(new(false)))}
				updated, response, cmd := m.handleCtrlCloseReview([]byte(`{"job_id":42,"closed":true}`))
				require.True(t, response.OK)
				for _, msg := range collectMsgs(cmd) {
					result, next := updated.Update(msg)
					updated = result.(model)
					collectMsgs(next)
				}
				assert.True(t, closed)
				assert.Empty(t, bodies)
				return
			}
			_, cmd := m.handleClosedResultMsg(closedResultMsg{jobID: 42, newState: tc.closed, err: tc.err})
			if cmd != nil {
				cmd()
			}
			require.Len(t, bodies, tc.count)
			if tc.count == 1 {
				assert.JSONEq(t, `{"event":"review_closed","properties":{"surface":"tui"}}`, bodies[0])
			}
		})
	}
}

//nolint:paralleltest // t.Setenv of telemetry opt-out variables
func TestCoreActionCommittedPatch(t *testing.T) {
	enableTelemetryEnv(t)
	for _, tc := range []struct {
		name                 string
		failCommit, failMark bool
	}{
		{name: "committed"}, {name: "commit failure", failCommit: true}, {name: "mark failure", failMark: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.InitTestRepo(t)
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			path := filepath.Join(repo.Root, "base.txt")
			require.NoError(t, os.WriteFile(path, []byte("changed\n"), 0o644))
			patch := repo.Run("diff", "--", "base.txt") + "\n"
			repo.Run("checkout", "--", "base.txt")
			if tc.failCommit {
				require.NoError(t, os.WriteFile(filepath.Join(repo.Root, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
			}
			var bodies []string
			marked := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/telemetry/events" {
					body, _ := io.ReadAll(r.Body)
					bodies = append(bodies, string(body))
					w.WriteHeader(http.StatusAccepted)
					return
				}
				if r.URL.Path == "/api/job/applied" {
					marked = true
					if tc.failMark {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"success":true}`)
			}))
			defer srv.Close()
			m := newModel(testEndpointFromURL(srv.URL), withExternalIODisabled())
			before := repo.Run("rev-parse", "HEAD")
			result := m.checkApplyCommitPatch(context.Background(), 42, &storage.ReviewJob{}, repo.Root, patch)
			assert.Equal(t, tc.failCommit, result.commitFailed)
			assert.Empty(t, bodies, "the patch result must return before telemetry starts")
			_, reportCmd := m.handleApplyPatchResultMsg(result)
			collectMsgs(reportCmd)
			if tc.failCommit {
				require.Error(t, result.err)
				assert.Empty(t, bodies)
				assert.Equal(t, before, repo.Run("rev-parse", "HEAD"))
			} else {
				if tc.failMark {
					require.Error(t, result.err)
				} else {
					require.NoError(t, result.err)
				}
				assert.True(t, marked)
				assert.NotEqual(t, before, repo.Run("rev-parse", "HEAD"))
				require.Len(t, bodies, 1)
				assert.JSONEq(t, `{"event":"finding_fixed","properties":{"surface":"tui"}}`, bodies[0])
			}
		})
	}
}

type recordingTransport struct{ events []string }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
	}
	if strings.Contains(string(body), `"review_viewed"`) {
		r.events = append(r.events, string(body))
	}
	return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Header: make(http.Header)}, nil
}
