package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// splitPaneStyle wraps pane content in a border: accent when focused,
// gray otherwise. lipgloss v2 sizes Width/Height border-box.
func splitPaneStyle(focused bool, outerW, outerH int) lipgloss.Style {
	border := adaptiveColor("242", "246")
	if focused {
		border = adaptiveColor("125", "205")
	}
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(border).
		Width(outerW).
		Height(outerH)
}

// splitFooterRows returns the help table for the focused pane.
func (m model) splitFooterRows() [][]helpItem {
	if m.focus == focusDetail {
		rows := [][]helpItem{
			{{"p", "prompt"}, {"c", "comment"}, {"m", "commit"}, {"a", "close"}, {"y", "copy"}},
			{{"↑/↓", "scroll"}, {"←/→", "prev/next"}, {"L", "layout"}, {"esc", "back to list"}, {"?", "commands"}},
		}
		if m.tasksWorkflowEnabled() {
			rows[0] = append(rows[0], helpItem{"F", "fix"})
		}
		return rows
	}
	rows := filterHelpItem(m.queueHelpRows(), "↵")
	last := len(rows) - 1
	rows[last] = append(rows[last], helpItem{"L", "layout"}, helpItem{"tab", "focus detail"})
	return rows
}

// filterHelpItem returns a copy of rows with any entry whose key matches key
// removed. Enter is a no-op with the queue focused in split layout (the
// detail pane already follows the cursor), so the split footer omits the
// "↵ review" hint that queueHelpRows carries for the stacked layout.
func filterHelpItem(rows [][]helpItem, key string) [][]helpItem {
	out := make([][]helpItem, len(rows))
	for i, row := range rows {
		filtered := make([]helpItem, 0, len(row))
		for _, item := range row {
			if item.key == key {
				continue
			}
			filtered = append(filtered, item)
		}
		out[i] = filtered
	}
	return out
}

// renderSplit composes the split screen: full-width title, two bordered
// panes, full-width info line and help footer.
func (m model) renderSplit() string {
	footerRows := m.splitFooterRows()
	footerLines := len(reflowHelpRows(footerRows, m.width))
	g := splitGeometry(m.width, m.height, footerLines)

	title := m.renderQueueTitle()

	listPane := splitPaneStyle(m.focus == focusList, g.listOuterW, g.bodyH).
		Render(strings.Join(m.renderQueuePaneBody(g.listInnerW, g.listInnerH), "\n"))
	detailPane := splitPaneStyle(m.focus == focusDetail, g.detailOuterW, g.bodyH).
		Render(strings.Join(m.renderDetailPane(g.detailInnerW, g.detailInnerH), "\n"))
	body := lipgloss.JoinHorizontal(lipgloss.Top, listPane, detailPane)

	info := m.splitInfoLine(g)
	footer := renderHelpTable(footerRows, m.width)

	return title + "\x1b[K\n" + body + "\n" + info + "\x1b[K\n" + footer + "\x1b[K\x1b[J"
}

// splitInfoLine shows the flash (if any) or the focused pane's position.
func (m model) splitInfoLine(g splitGeom) string {
	if flash := m.renderFlash(m.currentView); flash != "" {
		return flash
	}
	if m.focus == focusDetail && m.currentReview != nil {
		// Exactly the condition renderDetailPane renders the review body
		// under: anything looser describes content that is not on screen
		// (a status card for a queued/running job, or the loading/error
		// card while a stale attempt's review is being refetched).
		if m.selectedReviewLoaded() {
			start, end, total := m.reviewPaneScrollInfo(g.detailInnerW, g.detailInnerH)
			if total > end-start {
				return statusStyle.Render(fmt.Sprintf("[%d-%d of %d lines]", start+1, end, total))
			}
		}
		return ""
	}
	rows := m.visibleQueueRows()
	idx := visibleSelectedRowIndex(rows, m.selectedJobID)
	if idx >= 0 {
		return statusStyle.Render(fmt.Sprintf("%d/%d jobs", idx+1, len(rows)))
	}
	return ""
}

// handleSplitMouse handles mouse events while the split layout is active:
// click focuses/selects by x-coordinate (list side selects a row and keeps
// list focus; detail side focuses the detail pane), wheel scrolls whichever
// pane the cursor is over regardless of current focus. A list-side selection
// change (click or wheel) schedules the debounced detail follow so the
// detail pane catches up, mirroring the keyboard nav wrapper in
// handleKeyMsg -- but only when the selection actually moved: a click on
// the already-selected row, or a wheel at a boundary where the move
// clamps in place, must not reset reviewScroll via scheduleDetailFollow.
// List-side wheel routes through the shared queueNavUp/queueNavDown
// helpers (handlers.go), so it prefetches near the end of loaded data and
// resumes pagination at the bottom boundary exactly like keyboard
// navigation; the nav cmd is batched with the detail-follow cmd.
func (m model) handleSplitMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	mouse := msg.Mouse()
	footerRows := m.splitFooterRows()
	g := splitGeometry(m.width, m.height, len(reflowHelpRows(footerRows, m.width)))

	switch msg.(type) {
	case tea.MouseWheelMsg:
		if mouse.X < g.listOuterW {
			prevSelected := m.selectedJobID
			var navCmd tea.Cmd
			switch mouse.Button {
			case tea.MouseWheelUp:
				m, navCmd = m.queueNavUp()
			case tea.MouseWheelDown:
				m, navCmd = m.queueNavDown()
			default:
				return m, nil
			}
			mm, followCmd := m.followSelectionChange(prevSelected)
			return mm, tea.Batch(navCmd, followCmd)
		}
		var delta int
		switch mouse.Button {
		case tea.MouseWheelUp:
			delta = -3
		case tea.MouseWheelDown:
			delta = 3
		default:
			return m, nil
		}
		m.reviewScroll += delta
		if m.reviewScroll < 0 {
			m.reviewScroll = 0
		}
		if m.mdCache != nil && m.reviewScroll > m.mdCache.lastReviewMaxScroll {
			m.reviewScroll = m.mdCache.lastReviewMaxScroll
		}
		return m, nil
	case tea.MouseClickMsg:
		if mouse.Button != tea.MouseLeft {
			return m, nil
		}
		if mouse.X < g.listOuterW {
			rows := m.visibleQueueRows()
			idx := m.splitListRowAt(rows, mouse.Y, g)
			if idx < 0 || idx >= len(rows) {
				return m, nil
			}
			prevSelected := m.selectedJobID
			m = m.moveSelectionToJobID(rows[idx].job.ID)
			m.focus = focusList
			m.currentView = viewQueue
			return m.followSelectionChange(prevSelected)
		}
		if m.selectedReviewLoaded() {
			// Same reasoning as handleTabKey's split branch: this click
			// enters viewReview from a review the follow-fetch already
			// loaded, not a fresh dispatch, so stamp the origin explicitly
			// rather than leave a stale reviewFromView in place. The
			// selectedReviewLoaded guard (rather than a bare
			// currentReview != nil check) additionally rules out a stale
			// review left over from a previously selected job -- see
			// handleTabKey's identical guard for the full rationale.
			m.reviewFromView = viewQueue
			m.focus = focusDetail
			m.currentView = viewReview
		}
		return m, nil
	default:
		return m, nil
	}
}

// queuePaneRowCapacity returns the number of queue data rows the split list
// pane can currently display. Mirrors renderQueuePaneBody's row BUDGET
// exactly: `max(innerH-2, 1)`, passed to renderQueueTable unconditionally --
// even in compact mode, where the 2-line header/separator isn't actually
// drawn on screen but the freed rows are padded with blank lines rather
// than repurposed for more data rows (see renderQueuePaneBody). There is
// deliberately no compact-mode special case here: that would overestimate
// capacity by 2 relative to what's actually rendered, one root cause behind
// the pagination-refill bug this helper was introduced to fix. Contrast
// with splitListRowAt's OWN (unrelated) compact adjustment below, which is
// about the on-screen y-offset of the header that IS or ISN'T drawn, not
// about the row budget.
func (m model) queuePaneRowCapacity() int {
	footerRows := m.splitFooterRows()
	g := splitGeometry(m.width, m.height, len(reflowHelpRows(footerRows, m.width)))
	return max(g.listInnerH-2, 1)
}

// splitListRowAt maps a screen row y to the index of the visible queue row
// drawn there (into rows, the caller's m.visibleQueueRows()), or -1 when y
// misses the data rows. Mirrors handleQueueMouseClick's window math
// (handlers_queue.go) with split pane offsets: title band (1) + pane top
// border (1) + the table header/separator lines renderQueueTable actually
// draws on screen before the data rows (2 normally, 0 in compact mode --
// see renderQueueTable's compact stripping). This tableHeaderLines is
// distinct from queuePaneRowCapacity's row budget (which stays innerH-2
// regardless of compact mode, matching what renderQueuePaneBody actually
// passes to renderQueueTable) -- it's purely about where the data rows
// start on screen.
func (m model) splitListRowAt(rows []queueRow, y int, g splitGeom) int {
	if len(rows) == 0 {
		return -1
	}
	tableHeaderLines := 2
	if m.queueCompact() {
		tableHeaderLines = 0
	}
	visibleRows := m.queuePaneRowCapacity()
	start, end := queueWindowStart(len(rows), visibleSelectedRowIndex(rows, m.selectedJobID), visibleRows)

	offset := 1 + 1 + tableHeaderLines // title + pane border + table header/separator
	row := y - offset
	if row < 0 || row >= visibleRows {
		return -1
	}
	idx := start + row
	if idx < start || idx >= end {
		return -1
	}
	return idx
}
