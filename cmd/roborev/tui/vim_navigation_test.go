package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestVimNavigationPagesAndEnds(t *testing.T) {
	t.Parallel()
	for _, view := range []viewKind{viewReview, viewKindPrompt, viewCommitMsg, viewHelp, viewLog, viewPatch, viewReleaseNotes} {
		t.Run(view.String(), func(t *testing.T) {
			m := initTestModel(withCurrentView(view), withDimensions(200, 20))
			content := strings.Repeat("line\n", 100)
			m.currentReview = &storage.Review{Output: content, Prompt: content}
			m.commitMsgContent, m.patchText = content, content
			m.logLines = make([]logLine, 100)
			m.releaseNotes = []storage.ReleaseNote{{Name: "Release", Body: content}}
			m.mdCache = nil // navigation must also work before a cached render
			scroll := func(m model) int {
				switch view {
				case viewReview:
					return m.reviewScroll
				case viewKindPrompt:
					return m.promptScroll
				case viewCommitMsg:
					return m.commitMsgScroll
				case viewHelp:
					return m.helpScroll
				case viewLog:
					return m.logScroll
				case viewPatch:
					return m.patchScroll
				default:
					return m.releaseNotesScroll
				}
			}
			m, _ = pressKey(m, 'd')
			page := 16
			if view == viewHelp || view == viewReleaseNotes {
				page = 17
			}
			if view == viewLog {
				page = 16 // title, separator, status and footer; no job command
			}
			assert.Equal(t, page, scroll(m), "one full visible page")
			m, _ = pressKey(m, 'u')
			assert.Zero(t, scroll(m))
			m, _ = pressKey(m, 'G')
			bottom := scroll(m)
			assert.Positive(t, bottom)
			for range 3 {
				m, _ = pressKey(m, 'd')
			}
			assert.Equal(t, bottom, scroll(m), "stored position clamps at bottom")
			m, _ = pressKey(m, 'u')
			assert.Equal(t, max(bottom-page, 0), scroll(m))
			m, _ = pressKey(m, 'g')
			m, _ = pressKey(m, 'g')
			assert.Zero(t, scroll(m), "gg deterministically stays at top")
			if view == viewLog {
				assert.False(t, m.logFollow)
			}
			m, _ = pressSpecial(m, tea.KeyEnd)
			assert.Equal(t, bottom, scroll(m))
			if view == viewLog {
				assert.True(t, m.logFollow)
			}
			m, _ = pressSpecial(m, tea.KeyPgUp)
			assert.Equal(t, max(bottom-page, 0), scroll(m))
			m, _ = pressSpecial(m, tea.KeyPgDown)
			assert.Equal(t, bottom, scroll(m))
		})
	}
}

func TestVimNavigationQueueAndTasks(t *testing.T) {
	t.Parallel()
	for _, view := range []viewKind{viewQueue, viewTasks} {
		t.Run(view.String(), func(t *testing.T) {
			m := initTestModel(withCurrentView(view), withDimensions(200, 10))
			m.jobs, m.fixJobs = navigationJobs(40), navigationJobs(40)
			m.selectedIdx, m.selectedJobID = 0, 1
			index := func(m model) int {
				if view == viewTasks {
					return m.fixSelectedIdx
				}
				return m.selectedIdx
			}
			page := 9 // compact queue reserves one title row
			if view == viewTasks {
				page = 2 // six chrome rows and two footer rows
			}
			m, _ = pressKey(m, 'd')
			assert.Equal(t, page, index(m))
			m, _ = pressKey(m, 'u')
			assert.Zero(t, index(m))
			m, _ = pressKey(m, 'G')
			assert.Equal(t, 39, index(m))
			m, _ = pressKey(m, 'd')
			assert.Equal(t, 39, index(m))
			m, _ = pressSpecial(m, tea.KeyPgUp)
			assert.Equal(t, 39-page, index(m))
			m, _ = pressSpecial(m, tea.KeyEnd)
			assert.Equal(t, 39, index(m))
			m, _ = pressKey(m, 'g')
			m, _ = pressKey(m, 'g')
			assert.Zero(t, index(m))
		})
	}
}

func TestReleaseNotesUppercaseShortcut(t *testing.T) {
	t.Parallel()
	m := initTestModel(withDimensions(100, 24))
	m, cmd := pressKey(m, 'U')
	assert.Equal(t, viewReleaseNotes, m.currentView)
	assert.True(t, m.releaseNotesLoading)
	assert.NotNil(t, cmd)
	m.releaseNotesLoading = false
	m, cmd = pressKey(m, 'U')
	assert.True(t, m.releaseNotesLoading)
	assert.NotNil(t, cmd)
}

func TestVimNavigationPreservesTextInputs(t *testing.T) {
	t.Parallel()
	for _, view := range []viewKind{viewKindComment, viewFilter, viewPatch, viewReview} {
		t.Run(view.String(), func(t *testing.T) {
			m := initTestModel(withCurrentView(view))
			m.currentReview = &storage.Review{Output: "review"}
			m.savePatchInputActive = true
			m.reviewFixPanelOpen, m.reviewFixPanelFocused = true, true
			for _, key := range "udggG" {
				m, _ = pressKey(m, key)
			}
			var text string
			switch view {
			case viewKindComment:
				text = m.commentText
			case viewFilter:
				text = m.filterSearch
			case viewPatch:
				text = m.savePatchInput
			case viewReview:
				text = m.fixPromptText
			}
			assert.Equal(t, "udggG", text)
			assert.Equal(t, view, m.currentView)
		})
	}
}

func TestVimNavigationEmptyViews(t *testing.T) {
	t.Parallel()
	for _, view := range []viewKind{viewQueue, viewTasks, viewReview, viewKindPrompt, viewCommitMsg, viewLog, viewPatch, viewHelp, viewReleaseNotes} {
		t.Run(view.String(), func(t *testing.T) {
			m := initTestModel(withCurrentView(view), withDimensions(80, 10))
			m.mdCache = nil
			for _, key := range "dugG" {
				res, _ := m.handleKeyMsg(keyPressMsg(key))
				m = res.(model)
			}
			assert.GreaterOrEqual(t, m.reviewScroll, 0)
			assert.GreaterOrEqual(t, m.promptScroll, 0)
			assert.GreaterOrEqual(t, m.patchScroll, 0)
			assert.GreaterOrEqual(t, m.fixSelectedIdx, 0)
		})
	}
}

func TestVimNavigationBottomPanelMemberPaginates(t *testing.T) {
	t.Parallel()
	m := seededPanelModel(t)
	m.jobs = append(navigationJobs(30), m.jobs[1])
	for i := range 30 {
		m.jobs[i].ID += 100
	}
	m.expandedPanels[testUUID("R")] = true
	m.hasMore = true
	m.loadingJobs = false
	m, cmd := pressKey(m, 'G')
	assert.Equal(t, int64(12), m.selectedJobID)
	assert.Equal(t, -1, m.selectedIdx)
	assert.True(t, m.loadingMore)
	require.NotNil(t, cmd)
}

func TestVimNavigationFreshAndResizedMarkdownBounds(t *testing.T) {
	t.Parallel()
	for _, view := range []viewKind{viewReview, viewKindPrompt} {
		t.Run(view.String(), func(t *testing.T) {
			m := initTestModel(withCurrentView(view), withDimensions(200, 20))
			content := strings.Repeat("line\n\n", 100) + "LASTLINE"
			m.currentReview = &storage.Review{Output: content, Prompt: content}
			m, _ = pressKey(m, 'G') // cache exists but no render has populated bounds
			assert.Contains(t, m.viewContent(), "LASTLINE")
			old := m.reviewScroll + m.promptScroll
			m.height = 30
			m, _ = pressKey(m, 'G')
			assert.Less(t, m.reviewScroll+m.promptScroll, old)
			assert.Contains(t, m.viewContent(), "LASTLINE")
		})
	}
}

func TestVimNavigationSplitQueueFollowsSelection(t *testing.T) {
	t.Parallel()
	m := splitModel()
	m.jobs = navigationJobs(80)
	m.selectedIdx, m.selectedJobID = 0, 1
	m.loadingJobs = false
	oldGen := m.detailFollowGen
	m, cmd := pressKey(m, 'G')
	assert.Equal(t, int64(80), m.selectedJobID)
	assert.Greater(t, m.detailFollowGen, oldGen)
	assert.NotNil(t, cmd)
}

func TestVimNavigationSplitReviewPageAndBottom(t *testing.T) {
	t.Parallel()
	review := splitTestReview()
	review.Output = strings.Repeat("line\n\n", 100) + "LASTLINE"
	m := splitModel(withReview(review), withDimensions(200, 40))
	m.currentView, m.focus = viewReview, focusDetail
	m.reviewFixPanelOpen = true // unfocused panel reduces the visible body
	m, _ = pressKey(m, 'd')
	assert.Equal(t, 26, m.reviewScroll, "pane body excludes borders, chrome, header and fix panel")
	m, _ = pressKey(m, 'G')
	assert.Contains(t, m.viewContent(), "LASTLINE")
	m, _ = pressKey(m, 'g')
	assert.Zero(t, m.reviewScroll)
}

func TestVimNavigationWrappedCommitBottomAfterResize(t *testing.T) {
	t.Parallel()
	m := initTestModel(withCurrentView(viewCommitMsg), withDimensions(40, 12))
	m.commitMsgContent = strings.Repeat("a long commit message with wrapped words ", 60) + "LASTLINE"
	m, _ = pressKey(m, 'G')
	assert.Contains(t, m.renderCommitMsgView(), "LASTLINE")
	old := m.commitMsgScroll
	m.width, m.height = 100, 20
	m, _ = pressKey(m, 'u')
	assert.Less(t, m.commitMsgScroll, old)
	m, _ = pressKey(m, 'G')
	assert.Contains(t, m.renderCommitMsgView(), "LASTLINE")
}

func TestVimNavigationPromptPagesRespectExpandedCommand(t *testing.T) {
	t.Parallel()
	m := initTestModel(withCurrentView(viewKindPrompt), withDimensions(12, 30))
	job := makeJob(1, withAgent("test"))
	m.currentReview = &storage.Review{Job: &job, Prompt: strings.Repeat("line\n\n", 100)}
	m.promptCmdExpanded = false
	collapsed, _ := pressKey(m, 'd')
	m.promptCmdExpanded = true
	expanded, _ := pressKey(m, 'd')
	assert.Greater(t, collapsed.promptScroll, expanded.promptScroll,
		"wrapped command headers reduce the visible page")
	assert.Positive(t, expanded.promptScroll)
}

func TestVimNavigationReleaseNotesWrappedFooter(t *testing.T) {
	t.Parallel()
	m := initTestModel(withCurrentView(viewReleaseNotes), withDimensions(40, 20))
	m.releaseNotes = []storage.ReleaseNote{{Name: "Release", Body: strings.Repeat("line\n\n", 100) + "LASTLINE"}}
	assert.Len(t, strings.Split(m.renderReleaseNotesView(), "\n"), 20,
		"wrapped footer must fit within the terminal height")
	m, _ = pressKey(m, 'd')
	assert.Equal(t, 16, m.releaseNotesScroll, "one page excludes both footer rows")
	m, _ = pressKey(m, 'G')
	assert.Contains(t, m.renderReleaseNotesView(), "LASTLINE")
	old := m.releaseNotesScroll
	m.width = 200
	m, _ = pressKey(m, 'G')
	assert.Equal(t, old-1, m.releaseNotesScroll, "wider footer frees a content row")
	assert.Contains(t, m.renderReleaseNotesView(), "LASTLINE")
}
