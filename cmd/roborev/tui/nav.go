package tui

import (
	tea "charm.land/bubbletea/v2"

	"go.kenn.io/roborev/internal/storage"
)

// updateSelectedJobID updates the tracked job ID after navigation
func (m *model) updateSelectedJobID() {
	if m.selectedIdx >= 0 && m.selectedIdx < len(m.jobs) {
		m.selectedJobID = m.jobs[m.selectedIdx].ID
	}
}

// selectedRowIndex returns the index of selectedJobID within the flattened
// visible rows, or -1 if absent.
func (m model) selectedRowIndex(rows []queueRow) int {
	return visibleSelectedRowIndex(rows, m.selectedJobID)
}

// moveQueueSelection moves the cursor by delta over the flattened visible rows,
// clamping at the ends, and sets selectedJobID (authoritative). selectedIdx is
// kept best-effort (index in m.jobs, or -1 for a member) for legacy callers.
func (m model) moveQueueSelection(delta int) model {
	rows := m.visibleQueueRows()
	if len(rows) == 0 {
		return m
	}
	idx := m.selectedRowIndex(rows)
	if idx < 0 {
		idx = 0
	} else {
		idx += delta
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= len(rows) {
		idx = len(rows) - 1
	}
	return m.moveSelectionToJobID(rows[idx].job.ID)
}

// eligibleReviewRow reports whether a job can be opened in the review view.
func eligibleReviewRow(j storage.ReviewJob) bool {
	return j.Status == storage.JobStatusDone || j.Status == storage.JobStatusFailed
}

// eligiblePromptRow reports whether a job has a prompt viewable in prompt view.
func eligiblePromptRow(j storage.ReviewJob) bool {
	return j.Status == storage.JobStatusDone ||
		(j.Prompt != "" && (j.Status == storage.JobStatusRunning || j.Status == storage.JobStatusQueued))
}

// eligibleLogRow reports whether a job has a log viewable in log view.
func eligibleLogRow(j storage.ReviewJob) bool {
	return j.Status != storage.JobStatusQueued
}

// contentNavStep walks the flattened visible rows from the current selection
// (selectedJobID) in direction dir (-1 = newer/toward row 0, +1 = older) and
// returns the next row whose job is eligible for the current content view.
// present=false means selectedJobID is no longer in the flattened rows (e.g. its
// panel collapsed) — the caller flashes and leaves the view stable instead of
// indexing m.jobs. found=false with present=true means no eligible job in that
// direction — the caller keeps the existing "No newer/older" flash.
func (m model) contentNavStep(
	dir int, eligible func(storage.ReviewJob) bool,
) (job storage.ReviewJob, found, present bool) {
	rows := m.visibleQueueRows()
	idx := visibleSelectedRowIndex(rows, m.selectedJobID)
	if idx < 0 {
		return storage.ReviewJob{}, false, false
	}
	for i := idx + dir; i >= 0 && i < len(rows); i += dir {
		if eligible(*rows[i].job) {
			return *rows[i].job, true, true
		}
	}
	return storage.ReviewJob{}, false, true
}

// isReviewAnchored reports whether the current view is part of a
// review-rooted view chain (review, prompt-from-review, log-from-review)
// where selectedIdx must stay anchored to the displayed review's position.
func (m model) isReviewAnchored() bool {
	switch m.currentView {
	case viewReview:
		return true
	case viewKindPrompt:
		return !m.promptFromQueue
	case viewLog:
		return m.logReviewAnchored
	default:
		return false
	}
}

// selectionStartIndex returns the m.jobs index at which a positional fallback
// scan should begin in direction dir (-1 = newer, +1 = older). When the selected
// job still occupies m.jobs[selectedIdx] (present, or hidden in place by a
// filter), the scan starts just past it (selectedIdx+dir) so it is not
// re-selected. When the selection was omitted from m.jobs entirely (e.g. a
// server-side hide-closed refresh dropped the closed parent), the list shifted
// up under the preserved selectedIdx, so for the older direction the successor
// now sits at selectedIdx itself; the newer direction is unaffected.
func (m model) selectionStartIndex(dir int) int {
	inPlace := m.selectedIdx >= 0 && m.selectedIdx < len(m.jobs) &&
		m.jobs[m.selectedIdx].ID == m.selectedJobID
	if inPlace || dir < 0 {
		return m.selectedIdx + dir
	}
	return m.selectedIdx
}

// stepVisibleJobIndex walks m.jobs from selectionStartIndex in direction dir
// (-1 = newer toward index 0, +1 = older) and returns the index of the next job
// that is eligible for the view AND currently visible, or -1. This is the
// positional fallback used when an absent PARENT selection (hidden or omitted by
// hide-closed/filters) is no longer in the flattened rows; an absent member is
// handled separately by the caller (flash-and-stay).
func (m model) stepVisibleJobIndex(dir int, eligible func(storage.ReviewJob) bool) int {
	for i := m.selectionStartIndex(dir); i >= 0 && i < len(m.jobs); i += dir {
		if eligible(m.jobs[i]) && m.isJobVisible(m.jobs[i]) {
			return i
		}
	}
	return -1
}

// findPrevLoggableFixJob finds the previous (older) fix job with a log.
func (m *model) findPrevLoggableFixJob() int {
	for i := m.fixSelectedIdx + 1; i < len(m.fixJobs); i++ {
		if m.fixJobs[i].Status != storage.JobStatusQueued {
			return i
		}
	}
	return -1
}

// findNextLoggableFixJob finds the next (newer) fix job with a log.
func (m *model) findNextLoggableFixJob() int {
	for i := m.fixSelectedIdx - 1; i >= 0; i-- {
		if m.fixJobs[i].Status != storage.JobStatusQueued {
			return i
		}
	}
	return -1
}

// logViewLookupJob finds the job being viewed in the log view.
// Searches m.jobs first, then m.fixJobs for jobs opened from
// the tasks view.
func (m *model) logViewLookupJob() *storage.ReviewJob {
	for i := range m.jobs {
		if m.jobs[i].ID == m.logJobID {
			return &m.jobs[i]
		}
	}
	for i := range m.fixJobs {
		if m.fixJobs[i].ID == m.logJobID {
			return &m.fixJobs[i]
		}
	}
	return nil
}

// logVisibleLines returns the number of content lines visible in the
// log view, accounting for title, optional command line, optional
// classify-reasoning lines, separator, status, and help bar.
func (m *model) logVisibleLines() int {
	// title + separator + status + help(N)
	helpRows := m.logHelpRows()
	reserved := 3 + len(reflowHelpRows(helpRows, m.width))
	job := m.logViewLookupJob()
	// Command header may span multiple lines when expanded; classify rows
	// add their own reasoning header lines.
	reserved += len(m.commandHeaderLines(job))
	reserved += len(classifyReasoningLines(job, m.width))
	return max(m.height-reserved, 1)
}

// logHelpRows returns the help row items for the log view.
func (m *model) logHelpRows() [][]helpItem {
	helpRow := []helpItem{
		{"↑/↓", "scroll"},
		{"←/→", "prev/next"},
		{"g", "toggle top/bottom"},
		{"i", "expand cmd"},
	}
	if m.logStreaming {
		helpRow = append(helpRow, helpItem{"x", "cancel"})
	}
	helpRow = append(helpRow, helpItem{"esc/q", "back"})
	return [][]helpItem{helpRow}
}

// normalizeSelectionIfHidden adjusts selectedIdx/selectedJobID if the current
// selection is hidden or out of bounds (e.g., job removed while in review
// view, or marked closed with hideClosed filter active).
// Call this when returning to queue view from review view.
func (m *model) normalizeSelectionIfHidden() {
	if len(m.jobs) == 0 {
		m.selectedIdx = -1
		m.selectedJobID = 0
		return
	}
	if m.selectedIdx < 0 || m.selectedIdx >= len(m.jobs) {
		clamped := max(0, min(len(m.jobs)-1, m.selectedIdx))
		idx := m.findNearestVisibleJob(clamped)
		if idx >= 0 {
			m.selectedIdx = idx
			m.updateSelectedJobID()
		} else {
			m.selectedIdx = -1
			m.selectedJobID = 0
		}
		return
	}
	if !m.isJobVisible(m.jobs[m.selectedIdx]) {
		idx := m.findNearestVisibleJob(m.selectedIdx)
		if idx >= 0 {
			m.selectedIdx = idx
			m.updateSelectedJobID()
		}
	} else if m.selectedJobID != m.jobs[m.selectedIdx].ID {
		// Resync stale selectedJobID (e.g., a job was removed from
		// the middle while in a review-anchored view).
		m.updateSelectedJobID()
	}
}

// findPrevVisibleJob returns the first visible job at a higher index
// (older, lower ID) than currentIdx.
func (m model) findPrevVisibleJob(currentIdx int) int {
	for i := currentIdx + 1; i < len(m.jobs); i++ {
		if m.isJobVisible(m.jobs[i]) {
			return i
		}
	}
	return -1
}

// findNextVisibleJob returns the first visible job at a lower index
// (newer, higher ID) than currentIdx.
func (m model) findNextVisibleJob(currentIdx int) int {
	for i := currentIdx - 1; i >= 0; i-- {
		if m.isJobVisible(m.jobs[i]) {
			return i
		}
	}
	return -1
}

// countVisibleJobsAfter returns the number of visible jobs after currentIdx,
// short-circuiting once the count reaches queuePrefetchBuffer since callers
// only need to know whether the count is below that threshold.
func (m model) countVisibleJobsAfter(currentIdx int) int {
	count := 0
	for i := currentIdx + 1; i < len(m.jobs); i++ {
		if m.isJobVisible(m.jobs[i]) {
			count++
			if count >= queuePrefetchBuffer {
				return count
			}
		}
	}
	return count
}

// maybePrefetch triggers a page fetch if the cursor is near the end of loaded
// data. Returns a tea.Cmd if a fetch was started, nil otherwise.
func (m *model) maybePrefetch(idx int) tea.Cmd {
	if m.canPaginate() && m.countVisibleJobsAfter(idx) < queuePrefetchBuffer {
		m.loadingMore = true
		return m.fetchMoreJobs()
	}
	return nil
}

// findNearestVisibleJob returns the nearest visible job to fromIdx.
// It checks fromIdx itself first, then searches older jobs (higher
// indices), then newer jobs (lower indices), then falls back to the
// first visible job in the list.
func (m model) findNearestVisibleJob(fromIdx int) int {
	if fromIdx >= 0 && fromIdx < len(m.jobs) &&
		m.isJobVisible(m.jobs[fromIdx]) {
		return fromIdx
	}
	idx := m.findPrevVisibleJob(fromIdx)
	if idx < 0 {
		idx = m.findNextVisibleJob(fromIdx)
	}
	if idx < 0 {
		idx = m.findFirstVisibleJob()
	}
	return idx
}

// findFirstVisibleJob returns the index of the first visible job.
func (m model) findFirstVisibleJob() int {
	for i := range m.jobs {
		if m.isJobVisible(m.jobs[i]) {
			return i
		}
	}
	return -1
}

// hasActiveFixJobs returns true if any fix jobs are queued or running.
func (m model) hasActiveFixJobs() bool {
	for _, j := range m.fixJobs {
		if j.Status == storage.JobStatusQueued || j.Status == storage.JobStatusRunning {
			return true
		}
	}
	return false
}
