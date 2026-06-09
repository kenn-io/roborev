package tui

import (
	"encoding/json"
	"net/http"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestReviewFixPanelOpenFromReview(t *testing.T) {
	job := makeJob(1)
	m := initTestModel(
		withCurrentView(viewReview),
		withReview(&storage.Review{JobID: 1, Job: &job}),
		withTestJobs(job),
		withSelection(0, 1),
		withTasksEnabled(true),
	)

	got, _ := pressKey(m, 'F')

	assertFixPanelOpen(t, got, 1)
	assertView(t, got, viewReview)
}

func TestReviewFixPanelTabTogglesReviewFocus(t *testing.T) {
	m := initTestModel(
		withCurrentView(viewReview),
		withFixPanel(true, true),
	)

	// Tab shifts focus to review
	got, _ := pressSpecial(m, tea.KeyTab)
	assertFixPanelState(t, got, true, false)

	// Tab again shifts focus back to fix panel
	got2, _ := pressSpecial(got, tea.KeyTab)
	assertFixPanelState(t, got2, true, true)
}

func TestReviewFixPanelTextInput(t *testing.T) {
	m := initTestModel(
		withCurrentView(viewReview),
		withFixPanel(true, true),
	)

	for _, ch := range "hello" {
		m, _ = pressKey(m, ch)
	}

	assert.Equal(t, "hello", m.fixPromptText)
}

func TestReviewFixPanelTextNotCapturedWhenUnfocused(t *testing.T) {
	m := initTestModel(
		withCurrentView(viewReview),
		withFixPanel(true, false),
	)

	got, _ := pressKey(m, 'x')
	assert.Empty(t, got.fixPromptText)
}

func TestReviewFixPanelEscWhenFocusedClosesPanel(t *testing.T) {
	m := initTestModel(
		withCurrentView(viewReview),
		withFixPanel(true, true),
		withFixPrompt(0, "some text"),
	)

	got, _ := pressSpecial(m, tea.KeyEsc)

	assertFixPanelClosed(t, got)
	assertView(t, got, viewReview)
}

func TestReviewFixPanelEscWhenUnfocusedClosesPanel(t *testing.T) {
	job := makeJob(1)
	m := initTestModel(
		withCurrentView(viewReview),
		withReview(&storage.Review{Job: &job}),
		withFixPanel(true, false),
		withReviewFromView(viewQueue),
	)

	got, _ := pressSpecial(m, tea.KeyEsc)

	assertFixPanelState(t, got, false, false)
	// Should stay in review view (not navigate back to queue)
	assertView(t, got, viewReview)
}

func TestReviewFixPanelPendingConsumedOnLoad(t *testing.T) {
	m := initTestModel(
		withFixPanelPending(true),
		withFixPrompt(5, ""),
		withSelection(0, 5),
	)

	review := &storage.Review{ID: 1, JobID: 5}
	got, _ := updateModel(t, m, reviewMsg{review: review, jobID: 5})

	assert.False(t, got.reviewFixPanelPending)
	assertFixPanelOpen(t, got, 5)
}

func TestReviewFixPanelEnterSubmitsAndNavigatesToTasks(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(storage.ReviewJob{ID: 1})
	})
	m.currentView = viewReview
	m.tasksEnabled = true
	m.reviewFixPanelOpen = true
	m.reviewFixPanelFocused = true
	m.fixPromptJobID = 1
	m.fixPromptText = "fix the lint errors"

	got, _ := pressSpecial(m, tea.KeyEnter)

	assertFixPanelClosed(t, got)
	assertView(t, got, viewTasks)
}

func TestReviewFixPanelBackspaceDeletesRune(t *testing.T) {
	m := initTestModel(
		withCurrentView(viewReview),
		withFixPanel(true, true),
		withFixPrompt(0, "hello"),
	)

	got, _ := pressSpecial(m, tea.KeyBackspace)

	assert.Equal(t, "hell", got.fixPromptText)
}

func TestFixKeyFromQueueFetchesReviewWithPendingFlag(t *testing.T) {
	review := storage.Review{
		ID: 1, JobID: 42,
		Job: &storage.ReviewJob{ID: 42, Status: storage.JobStatusDone},
	}
	_, m := mockServerModel(t, mockReviewHandler(
		review, []storage.Response{},
	))
	job := makeJob(42)
	m.currentView = viewQueue
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx = 0
	m.selectedJobID = 42
	m.tasksEnabled = true

	got, cmd := pressKey(m, 'F')

	assert.True(t, got.reviewFixPanelPending)
	assert.EqualValues(t, 42, got.selectedJobID)
	assert.NotNil(t, cmd)
}

func TestFixKeyDisabledShowsFlash(t *testing.T) {
	job := makeJob(42)
	m := initTestModel(
		withCurrentView(viewQueue),
		withTestJobs(job),
		withSelection(0, 42),
	)

	got, cmd := pressKey(m, 'F')

	assert.Nil(t, cmd)
	assert.False(t, got.reviewFixPanelPending)
	assert.Contains(t, got.flashMessage, "Tasks workflow disabled")
}

func TestFixPanelClosedOnReviewNavNext(t *testing.T) {
	job1 := makeJob(1)
	job2 := makeJob(2)
	m := initTestModel(
		withCurrentView(viewReview),
		withReview(&storage.Review{JobID: 1, Job: &job1}),
		withTestJobs(job1, job2),
		withSelection(0, 1),
		withFixPanel(true, false),
		withFixPrompt(1, "some instructions"),
	)

	// Navigate to next review (j)
	got, _ := pressKey(m, 'j')

	assertFixPanelClosed(t, got)
}

func TestFixPanelClosedOnReviewNavPrev(t *testing.T) {
	job1 := makeJob(1)
	job2 := makeJob(2)
	m := initTestModel(
		withCurrentView(viewReview),
		withReview(&storage.Review{JobID: 2, Job: &job2}),
		withTestJobs(job1, job2),
		withSelection(1, 2),
		withFixPanel(true, false),
		withFixPrompt(2, "fix it"),
	)

	// Navigate to previous review (k)
	got, _ := pressKey(m, 'k')

	assertFixPanelClosed(t, got)
}

func TestFixPanelClosedOnQuitFromReview(t *testing.T) {
	job := makeJob(1)
	m := initTestModel(
		withCurrentView(viewReview),
		withReview(&storage.Review{JobID: 1, Job: &job}),
		withTestJobs(job),
		withSelection(0, 1),
		withFixPanel(true, false),
		withFixPrompt(1, "instructions"),
	)

	got, _ := pressKey(m, 'q')

	assertFixPanelClosed(t, got)
	assert.NotEqual(t, viewReview, got.currentView)
}

func TestFixPanelPendingNotConsumedByWrongReview(t *testing.T) {
	m := initTestModel(
		withFixPanelPending(true),
		withFixPrompt(5, ""),
		withSelection(0, 10),
	)

	// A review for job 10 loads, but pending was for job 5
	got, _ := updateModel(t, m, reviewMsg{
		review: &storage.Review{ID: 2, JobID: 10}, jobID: 10,
	})

	assert.False(t, got.reviewFixPanelOpen)
	assert.True(t, got.reviewFixPanelPending)
}

func TestFixPanelPendingClearedOnStaleFetch(t *testing.T) {
	m := initTestModel(
		withFixPanelPending(true),
		withFixPrompt(5, ""),
		withSelection(0, 10), // User navigated away
	)

	// Stale review for job 5 arrives after user moved to job 10
	got, _ := updateModel(t, m, reviewMsg{
		review: &storage.Review{ID: 1, JobID: 5}, jobID: 5,
	})

	assert.False(t, got.reviewFixPanelPending)
	assertFixPanelClosed(t, got)
}

func TestFixPanelClosedOnPromptKey(t *testing.T) {
	job := makeJob(1)
	m := initTestModel(
		withCurrentView(viewReview),
		withReview(&storage.Review{
			JobID:  1,
			Job:    &job,
			Prompt: "review prompt text",
		}),
		withTestJobs(job),
		withSelection(0, 1),
		withFixPanel(true, false),
		withFixPrompt(1, "fix instructions"),
	)

	// Press 'p' to switch to prompt view
	got, _ := pressKey(m, 'p')

	assertView(t, got, viewKindPrompt)
	assertFixPanelClosed(t, got)
}

func TestFixPanelPendingClearedOnEscFromReview(t *testing.T) {
	job := makeJob(1)
	m := initTestModel(
		withCurrentView(viewReview),
		withReview(&storage.Review{JobID: 1, Job: &job}),
		withTestJobs(job),
		withSelection(0, 1),
		withFixPanelPending(true),
		withFixPrompt(1, ""),
	)

	got, _ := pressSpecial(m, tea.KeyEsc)

	assert.NotEqual(t, viewReview, got.currentView)
	assert.False(t, got.reviewFixPanelPending)
	assert.EqualValues(t, 0, got.fixPromptJobID)
}
