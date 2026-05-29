package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"go.kenn.io/roborev/internal/storage"
)

// updateSelectedJobID updates the tracked job ID after navigation
func (m *model) updateSelectedJobID() {
	if m.selectedIdx >= 0 && m.selectedIdx < len(m.jobs) {
		m.selectedJobID = m.jobs[m.selectedIdx].ID
	}
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

// findPrevViewableJob finds the previous (older, lower ID) viewable job.
// Respects active filters. Returns the index or -1 if none found.
func (m *model) findPrevViewableJob() int {
	for i := m.selectedIdx + 1; i < len(m.jobs); i++ {
		job := m.jobs[i]
		if (job.Status == storage.JobStatusDone || job.Status == storage.JobStatusFailed) &&
			m.isJobVisible(job) {
			return i
		}
	}
	return -1
}

// findNextViewableJob finds the next (newer, higher ID) viewable job.
// Respects active filters. Returns the index or -1 if none found.
func (m *model) findNextViewableJob() int {
	for i := m.selectedIdx - 1; i >= 0; i-- {
		job := m.jobs[i]
		if (job.Status == storage.JobStatusDone || job.Status == storage.JobStatusFailed) &&
			m.isJobVisible(job) {
			return i
		}
	}
	return -1
}

// findPrevPromptableJob finds the previous (older) job with a viewable prompt.
// Respects active filters. Returns the index or -1 if none found.
func (m *model) findPrevPromptableJob() int {
	for i := m.selectedIdx + 1; i < len(m.jobs); i++ {
		job := m.jobs[i]
		if m.isJobVisible(job) &&
			(job.Status == storage.JobStatusDone || (job.Prompt != "" && (job.Status == storage.JobStatusRunning || job.Status == storage.JobStatusQueued))) {
			return i
		}
	}
	return -1
}

// findNextPromptableJob finds the next (newer) job with a viewable prompt.
// Respects active filters. Returns the index or -1 if none found.
func (m *model) findNextPromptableJob() int {
	for i := m.selectedIdx - 1; i >= 0; i-- {
		job := m.jobs[i]
		if m.isJobVisible(job) &&
			(job.Status == storage.JobStatusDone || (job.Prompt != "" && (job.Status == storage.JobStatusRunning || job.Status == storage.JobStatusQueued))) {
			return i
		}
	}
	return -1
}

// findPrevLoggableJob finds the previous (older) job with a log.
// Respects active filters.
func (m *model) findPrevLoggableJob() int {
	for i := m.selectedIdx + 1; i < len(m.jobs); i++ {
		job := m.jobs[i]
		if job.Status != storage.JobStatusQueued &&
			m.isJobVisible(job) {
			return i
		}
	}
	return -1
}

// findNextLoggableJob finds the next (newer) job with a log.
// Respects active filters.
func (m *model) findNextLoggableJob() int {
	for i := m.selectedIdx - 1; i >= 0; i-- {
		job := m.jobs[i]
		if job.Status != storage.JobStatusQueued &&
			m.isJobVisible(job) {
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
		{"↑/↓", "scroll"}, {"←/→", "prev/next"}, {"g", "toggle top/bottom"},
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
