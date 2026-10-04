package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// navigationBounds returns the visible page size and maximum scroll position
// together, refreshing markdown bounds from the active renderer when needed.
func (m *model) navigationBounds() (pageSize, maxScroll int) {
	switch m.currentView {
	case viewQueue:
		if m.splitActive() {
			return m.queuePaneRowCapacity(), 0
		}
		return m.queueVisibleRows(), 0
	case viewReview, viewKindPrompt:
		if m.currentReview == nil {
			return max(m.height-10, 1), 0
		}
		if m.mdCache == nil {
			m.mdCache = newMarkdownCache(2)
		}
		if m.currentView == viewKindPrompt {
			_ = m.renderPromptView()
			return m.mdCache.lastPromptVisibleLines, m.mdCache.lastPromptMaxScroll
		}
		if m.splitActive() {
			g := splitLayoutConfig.Geometry(m.width, m.height, len(convertAndReflowHelpRows(m.splitFooterRows(), m.width)))
			_ = m.renderReviewPaneBody(g.DetailInnerW, g.DetailInnerH)
		} else {
			_ = m.renderReviewView()
		}
		return m.mdCache.lastReviewVisibleLines, m.mdCache.lastReviewMaxScroll
	case viewCommitMsg:
		pageSize = max(m.height-4, 1)
		return pageSize, max(len(m.commitMsgLines())-pageSize, 0)
	case viewPatch:
		pageSize = max(m.height-4, 1)
		return pageSize, max(len(strings.Split(m.patchText, "\n"))-pageSize, 0)
	case viewHelp:
		return max(m.height-3, 5), m.helpMaxScroll()
	case viewTasks:
		visible, _, _ := m.tasksVisibleWindow(len(m.fixJobs))
		return visible, 0
	case viewLog:
		pageSize = m.logVisibleLines()
		return pageSize, max(len(m.logLines)-pageSize, 0)
	}
	return max(m.height-10, 1), 0
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
	_, bottom := m.navigationBounds()
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
