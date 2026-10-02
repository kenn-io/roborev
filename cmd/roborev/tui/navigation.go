package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// navigationPageSize uses the same content area as the active renderer.
func (m *model) navigationPageSize() int {
	switch m.currentView {
	case viewQueue:
		if m.splitActive() {
			return m.queuePaneRowCapacity()
		}
		return m.queueVisibleRows()
	case viewReview, viewKindPrompt:
		if m.currentReview == nil {
			return max(m.height-10, 1)
		}
		if m.mdCache == nil {
			m.mdCache = newMarkdownCache(2)
		}
		if m.currentView == viewKindPrompt {
			_ = m.renderPromptView()
			return m.mdCache.lastPromptVisibleLines
		}
		if m.splitActive() {
			g := splitLayoutConfig.Geometry(m.width, m.height, len(convertAndReflowHelpRows(m.splitFooterRows(), m.width)))
			_ = m.renderReviewPaneBody(g.DetailInnerW, g.DetailInnerH)
		} else {
			_ = m.renderReviewView()
		}
		return m.mdCache.lastReviewVisibleLines
	case viewCommitMsg, viewPatch:
		return max(m.height-4, 1)
	case viewHelp:
		return max(m.height-3, 5)
	case viewTasks:
		visible, _, _ := m.tasksVisibleWindow(len(m.fixJobs))
		return visible
	case viewLog:
		return m.logVisibleLines()
	case viewReleaseNotes:
		return m.releaseNotesVisibleLines()
	}
	return max(m.height-10, 1)
}

// navigationMaxScroll is called after navigationPageSize refreshed markdown bounds.
func (m model) navigationMaxScroll(pageSize int) int {
	switch m.currentView {
	case viewReview:
		if m.currentReview != nil && m.mdCache != nil {
			return m.mdCache.lastReviewMaxScroll
		}
	case viewKindPrompt:
		if m.currentReview != nil && m.mdCache != nil {
			return m.mdCache.lastPromptMaxScroll
		}
	case viewCommitMsg:
		return max(len(wrapText(m.commitMsgContent, max(20, min(m.width-4, 100))))-pageSize, 0)
	case viewHelp:
		return m.helpMaxScroll()
	case viewLog:
		return max(len(m.logLines)-pageSize, 0)
	case viewPatch:
		return max(len(strings.Split(m.patchText, "\n"))-pageSize, 0)
	}
	return 0
}

func (m model) handleEndKey() (tea.Model, tea.Cmd) {
	if m.currentView == viewQueue {
		rows := m.visibleQueueRows()
		if len(rows) == 0 {
			return m, nil
		}
		m = m.moveSelectionToJobID(rows[len(rows)-1].job.ID)
		if m.canPaginate() {
			m.loadingMore = true
			return m, m.fetchMoreJobs()
		}
		return m, nil
	}
	page := m.navigationPageSize()
	bottom := m.navigationMaxScroll(page)
	switch m.currentView {
	case viewReview:
		m.reviewScroll = bottom
	case viewKindPrompt:
		m.promptScroll = bottom
	case viewCommitMsg:
		m.commitMsgScroll = bottom
	case viewHelp:
		m.helpScroll = bottom
	}
	return m, tea.ClearScreen
}
