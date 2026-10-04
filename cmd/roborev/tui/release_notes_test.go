package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestReleaseNotesViewRendersAndCloses(t *testing.T) {
	t.Parallel()
	m := initTestModel(withCurrentView(viewReleaseNotes), withDimensions(100, 28))
	m.releaseNotesFromView = viewQueue
	m.releaseNotes = []storage.ReleaseNote{{
		TagName: "v1.2.3", Name: "Roborev 1.2.3", Body: "## Changes\n\nFixed the queue.",
		PublishedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	}}

	output := m.renderReleaseNotesView()
	assert.Contains(t, output, "Roborev 1.2.3")
	assert.Contains(t, output, "Fixed the queue")

	updated, _ := m.handleReleaseNotesKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	closed, ok := updated.(model)
	require.True(t, ok)
	assert.Equal(t, viewQueue, closed.currentView)
}

func TestReleaseNotesShortcutOpensAndRefreshes(t *testing.T) {
	t.Parallel()
	m := initTestModel(withDimensions(100, 24))

	m, cmd := pressKey(m, 'u')
	assert.Equal(t, viewReleaseNotes, m.currentView)
	assert.True(t, m.releaseNotesLoading)
	assert.NotNil(t, cmd)

	m.releaseNotesLoading = false
	m, cmd = pressKey(m, 'u')
	assert.True(t, m.releaseNotesLoading)
	assert.NotNil(t, cmd)
}

func TestReleaseNotesViewFitsWrappedHelpTable(t *testing.T) {
	t.Parallel()
	for _, width := range []int{40, 50, 60, 80, 100} {
		m := initTestModel(withDimensions(width, 24))
		output := m.renderReleaseNotesView()
		assert.Equal(t, m.height, strings.Count(output, "\n")+1, "terminal width %d", width)
	}
}

func TestReleaseNotesIgnoresRemovedJumpAliases(t *testing.T) {
	t.Parallel()
	m := initTestModel(withDimensions(100, 24))
	m.releaseNotes = []storage.ReleaseNote{{
		Name: "Roborev", TagName: "v1", Body: strings.Repeat("Release note paragraph.\n\n", 80),
	}}
	maxScroll := m.releaseNotesMaxScroll()
	require.Greater(t, maxScroll, 1)
	startScroll := maxScroll / 2

	keys := []struct {
		name string
		msg  tea.KeyPressMsg
	}{
		{name: "home", msg: tea.KeyPressMsg{Code: tea.KeyHome}},
		{name: "g", msg: tea.KeyPressMsg{Code: 'g'}},
		{name: "end", msg: tea.KeyPressMsg{Code: tea.KeyEnd}},
		{name: "G", msg: tea.KeyPressMsg{Code: 'G'}},
	}
	for _, key := range keys {
		t.Run(key.name, func(t *testing.T) {
			m.releaseNotesScroll = startScroll
			updated, cmd := m.handleReleaseNotesKey(key.msg)
			got, ok := updated.(model)
			require.True(t, ok)
			assert.Equal(t, startScroll, got.releaseNotesScroll)
			assert.Nil(t, cmd)
		})
	}
}
