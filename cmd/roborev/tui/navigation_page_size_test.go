package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestPromptPageDownUsesRenderedVisibleLines(t *testing.T) {
	t.Parallel()
	m := initTestModel(withCurrentView(viewKindPrompt), withDimensions(12, 30))
	job := makeJob(1, withAgent("test"))
	m.currentReview = &storage.Review{Job: &job, Prompt: strings.Repeat("line\n\n", 100)}
	m.promptCmdExpanded = true
	m.renderPromptView()
	wantPageSize := len(m.mdCache.promptLines) - m.mdCache.lastPromptMaxScroll
	require.Positive(t, wantPageSize)

	got, _ := pressSpecial(m, tea.KeyPgDown)
	assert.Equal(t, wantPageSize, got.promptScroll)
}

func TestPromptPageDownAccountsForExpandedCommandHeader(t *testing.T) {
	t.Parallel()
	pageDown := func(expanded bool) int {
		m := initTestModel(withCurrentView(viewKindPrompt), withDimensions(12, 30))
		job := makeJob(1, withAgent("test"))
		job.CommandLine = "roborev review " + strings.Repeat("--flag value ", 8)
		m.currentReview = &storage.Review{Job: &job, Prompt: strings.Repeat("line\n\n", 100)}
		m.promptCmdExpanded = expanded

		got, _ := pressSpecial(m, tea.KeyPgDown)
		return got.promptScroll
	}

	collapsedPage := pageDown(false)
	expandedPage := pageDown(true)
	assert.Greater(t, collapsedPage, expandedPage)
}

func TestReviewPageDownUsesRenderedVisibleLines(t *testing.T) {
	t.Parallel()
	m := initTestModel(withCurrentView(viewReview), withDimensions(12, 30))
	job := makeJob(1, withAgent("test"))
	m.currentReview = &storage.Review{Job: &job, Output: strings.Repeat("line\n\n", 100)}
	m.renderReviewView()
	wantPageSize := len(m.mdCache.reviewLines) - m.mdCache.lastReviewMaxScroll
	require.Positive(t, wantPageSize)

	got, _ := pressSpecial(m, tea.KeyPgDown)
	assert.Equal(t, wantPageSize, got.reviewScroll)
}

func TestHelpPageKeysUseHelpVisibleLines(t *testing.T) {
	t.Parallel()
	m := initTestModel(withCurrentView(viewHelp), withDimensions(100, 24))
	wantPageSize := max(m.height-3, 5)

	m, _ = pressSpecial(m, tea.KeyPgDown)
	assert.Equal(t, wantPageSize, m.helpScroll)
	m, _ = pressSpecial(m, tea.KeyPgUp)
	assert.Zero(t, m.helpScroll)
}

func TestHelpPageUpClampsBeforePaging(t *testing.T) {
	t.Parallel()
	m := initTestModel(withCurrentView(viewHelp), withDimensions(100, 24))
	pageSize := m.helpPageSize()
	maxScroll := m.helpMaxScroll()
	require.Positive(t, maxScroll)
	m.helpScroll = maxScroll + pageSize/2

	got, _ := pressSpecial(m, tea.KeyPgUp)

	assert.Equal(t, max(0, maxScroll-pageSize), got.helpScroll)
}
