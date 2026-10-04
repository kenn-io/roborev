package tui

import (
	"encoding/json"
	"net/http"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestReviewFixPlanningToggleAndSubmit(t *testing.T) {
	t.Parallel()
	var request struct {
		ParentJobID int64 `json:"parent_job_id"`
		PlanFirst   bool  `json:"plan_first"`
	}
	_, m := mockServerModel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/job/fix", r.URL.Path)
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, json.NewEncoder(w).Encode(storage.ReviewJob{ID: 8}))
	}))
	m.currentView = viewReview
	m.currentReview = &storage.Review{JobID: 7, Output: "High: missing cancellation check"}
	m.reviewFixPanelOpen = true
	m.reviewFixPanelFocused = true
	m.fixPromptJobID = 7
	m.tasksEnabled = true
	m.width = 110
	m.height = 35
	got, _ := pressCtrl(m, 'p')
	assert.True(t, got.fixPlanFirst)
	assert.Contains(t, got.renderReviewView(), "Plan first: on")
	got, cmd := pressSpecial(got, tea.KeyEnter)
	require.NotNil(t, cmd)
	_ = cmd()
	assert.True(t, request.PlanFirst)
	assert.EqualValues(t, 7, request.ParentJobID)
	assert.False(t, got.fixPlanFirst)
}

func TestReviewFixPlanningReset(t *testing.T) {
	t.Parallel()
	m := initTestModel(withCurrentView(viewReview), withFixPanel(true, true))
	m.fixPlanFirst = true
	got, _ := pressSpecial(m, tea.KeyEsc)
	assert.False(t, got.fixPlanFirst)
}

func TestReviewFixPlanResultRendering(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()
	m := initTestModel(withCurrentView(viewReview), withReview(&storage.Review{JobID: 7, Output: "## Plan\n\nCheck cancellation before claiming work\n\n## Implementation\n\nAdded cancellation check"}))
	m.width = 110
	m.height = 35
	rendered := m.renderReviewView()
	assert.Contains(rendered, "Check cancellation before claiming work")
	assert.Contains(rendered, "Added cancellation check")
	assert.Contains(rendered, "Plan")
	assert.Contains(rendered, "Implementation")
}

func TestReviewPaneOmitsMetadataOnlyUsage(t *testing.T) {
	t.Parallel()
	job := makeJob(7)
	job.TokenUsage = `{"cost_usd":0,"provider_session_ids":["planner-session","implementation-session"],"expected_provider_sessions":2}`
	m := initTestModel(withReview(&storage.Review{JobID: 7, Job: &job, Output: "Fix completed"}))
	header := m.reviewPaneHeaderLines(200)
	require.Greater(t, len(header), 1)
	assert.NotContains(t, header[1], "[]")
}
