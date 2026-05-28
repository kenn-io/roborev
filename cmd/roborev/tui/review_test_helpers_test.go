package tui

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

// --- testModelOption functions for review tests ---

func withReview(r *storage.Review) testModelOption {
	return func(m *model) { m.currentReview = r }
}

func withDimensions(w, h int) testModelOption {
	return func(m *model) { m.width = w; m.height = h }
}

func withBranchName(b string) testModelOption {
	return func(m *model) { m.currentBranch = b }
}

func withReviewFromView(v viewKind) testModelOption {
	return func(m *model) { m.reviewFromView = v }
}

func withFixPanel(open, focused bool) testModelOption {
	return func(m *model) {
		m.reviewFixPanelOpen = open
		m.reviewFixPanelFocused = focused
	}
}

func withFixPanelPending(pending bool) testModelOption {
	return func(m *model) { m.reviewFixPanelPending = pending }
}

func withFixPrompt(jobID int64, text string) testModelOption {
	return func(m *model) {
		m.fixPromptJobID = jobID
		m.fixPromptText = text
	}
}

func withClipboard(c ClipboardWriter) testModelOption {
	return func(m *model) { m.clipboard = c }
}

func withTasksEnabled(enabled bool) testModelOption {
	return func(m *model) { m.tasksEnabled = enabled }
}

// --- Assertion helpers ---

func assertFixPanelState(
	t *testing.T, m model, open, focused bool,
) {
	t.Helper()
	assert.Equal(t, open, m.reviewFixPanelOpen, "reviewFixPanelOpen")
	assert.Equal(t, focused, m.reviewFixPanelFocused, "reviewFixPanelFocused")
}

func assertFixPanelClosed(t *testing.T, m model) {
	t.Helper()
	assertFixPanelState(t, m, false, false)
	assert.Empty(t, m.fixPromptText, "fixPromptText")
	assert.Zero(t, m.fixPromptJobID, "fixPromptJobID")
}

func assertFixPanelOpen(
	t *testing.T, m model, jobID int64,
) {
	t.Helper()
	assertFixPanelState(t, m, true, true)
	assert.Equal(t, jobID, m.fixPromptJobID, "fixPromptJobID")
}

// --- Mock review server handler ---

func mockReviewHandler(
	review storage.Review, responses []storage.Response,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/review":
			json.NewEncoder(w).Encode(review)
		case "/api/comments":
			json.NewEncoder(w).Encode(map[string]any{
				"responses": responses,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}
