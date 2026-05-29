package tui

import (
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
)

// handleKeyMsg dispatches key events to view-specific handlers.
func (m model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Fix panel captures input when focused in review view
	if m.currentView == viewReview && m.reviewFixPanelOpen && m.reviewFixPanelFocused {
		return m.handleReviewFixPanelKey(msg)
	}

	// Modal views that capture most keys for typing
	switch m.currentView {
	case viewKindComment:
		return m.handleCommentKey(msg)
	case viewFilter:
		return m.handleFilterKey(msg)
	case viewLog:
		return m.handleLogKey(msg)
	case viewKindWorktreeConfirm:
		return m.handleWorktreeConfirmKey(msg)
	case viewTasks:
		return m.handleTasksKey(msg)
	case viewPatch:
		return m.handlePatchKey(msg)
	case viewColumnOptions:
		return m.handleColumnOptionsInput(msg)
	}

	// Global keys shared across queue/review/prompt/commitMsg/help views
	return m.handleGlobalKey(msg)
}

func (m model) handleMouseMsg(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress {
		return m, nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if m.currentView == viewColumnOptions {
			if m.colOptionsIdx > 0 {
				m.colOptionsIdx--
			}
			return m, nil
		}
		if m.currentView == viewTasks {
			if m.fixSelectedIdx > 0 {
				m.fixSelectedIdx--
			}
			return m, nil
		}
		return m.handleUpKey()
	case tea.MouseButtonWheelDown:
		if m.currentView == viewColumnOptions {
			if m.colOptionsIdx < len(m.colOptionsList)-1 {
				m.colOptionsIdx++
			}
			return m, nil
		}
		if m.currentView == viewTasks {
			if m.fixSelectedIdx < len(m.fixJobs)-1 {
				m.fixSelectedIdx++
			}
			return m, nil
		}
		return m.handleDownKey()
	case tea.MouseButtonLeft:
		switch m.currentView {
		case viewQueue:
			m.handleQueueMouseClick(msg.X, msg.Y)
		case viewTasks:
			m.handleTasksMouseClick(msg.Y)
		case viewColumnOptions:
			return m.handleColumnOptionsMouseClick(msg.Y)
		}
		return m, nil
	default:
		return m, nil
	}
}

// handleGlobalKey handles keys shared across queue, review, prompt, commit msg, and help views.
func (m model) handleGlobalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if isSubmitKey(msg) {
		return m.handleEnterKey()
	}
	switch msg.String() {
	case "ctrl+c", "ctrl+d":
		return m.handleQuitKey()
	case "q":
		if m.noQuit && m.currentView == viewQueue {
			return m, nil
		}
		return m.handleQuitKey()
	case "home", "g":
		return m.handleHomeKey()
	case "up":
		return m.handleUpKey()
	case "j", "left":
		return m.handlePrevKey()
	case "down":
		return m.handleDownKey()
	case "k", "right":
		return m.handleNextKey()
	case "pgup":
		return m.handlePageUpKey()
	case "pgdown":
		return m.handlePageDownKey()
	case "p":
		return m.handlePromptKey()
	case "i":
		if m.currentView == viewKindPrompt {
			m.cmdExpanded = !m.cmdExpanded
			return m, tea.ClearScreen
		}
		return m, nil
	case "a":
		return m.handleCloseKey()
	case "x":
		return m.handleCancelKey()
	case "r":
		return m.handleRerunKey()
	case "l", "t":
		return m.handleLogKey2()
	case "f":
		return m.handleFilterOpenKey()
	case "b":
		return m.handleBranchFilterOpenKey()
	case "h":
		return m.handleHideClosedKey()
	case "s":
		return m.handleToggleClassifyKey()
	case "c":
		return m.handleCommentOpenKey()
	case "y":
		return m.handleCopyKey()
	case "m":
		return m.handleCommitMsgKey()
	case "?":
		return m.handleHelpKey()
	case "esc":
		return m.handleEscKey()
	case "F":
		return m.handleFixKey()
	case "T":
		return m.handleToggleTasksKey()
	case "o":
		return m.handleColumnOptionsKey()
	case "D":
		return m.handleDistractionFreeKey()
	case "tab":
		return m.handleTabKey()
	}
	return m, nil
}

func (m model) handleQuitKey() (tea.Model, tea.Cmd) {
	if m.currentView == viewReview {
		returnTo := m.reviewFromView
		if returnTo == 0 {
			returnTo = viewQueue
		}
		m.closeFixPanel()
		m.currentView = returnTo
		m.currentReview = nil
		m.reviewScroll = 0
		m.paginateNav = 0
		if returnTo == viewQueue {
			m.normalizeSelectionIfHidden()
		}
		return m, nil
	}
	if m.currentView == viewKindPrompt {
		m.paginateNav = 0
		if m.promptFromQueue {
			m.currentView = viewQueue
			m.currentReview = nil
			m.promptScroll = 0
		} else {
			m.currentView = viewReview
			m.promptScroll = 0
		}
		return m, nil
	}
	if m.currentView == viewCommitMsg {
		m.currentView = m.commitMsgFromView
		m.commitMsgContent = ""
		m.commitMsgScroll = 0
		return m, nil
	}
	if m.currentView == viewHelp {
		m.currentView = m.helpFromView
		return m, nil
	}
	return m, tea.Quit
}

func (m model) handleHomeKey() (tea.Model, tea.Cmd) {
	switch m.currentView {
	case viewQueue:
		firstVisible := m.findFirstVisibleJob()
		if firstVisible >= 0 {
			m.selectedIdx = firstVisible
			m.updateSelectedJobID()
		}
	case viewReview:
		m.reviewScroll = 0
	case viewKindPrompt:
		m.promptScroll = 0
	case viewCommitMsg:
		m.commitMsgScroll = 0
	case viewHelp:
		m.helpScroll = 0
	}
	return m, nil
}

func (m model) handleUpKey() (tea.Model, tea.Cmd) {
	switch m.currentView {
	case viewQueue:
		nextIdx := m.findNextVisibleJob(m.selectedIdx)
		if nextIdx >= 0 {
			m.selectedIdx = nextIdx
			m.updateSelectedJobID()
		} else {
			m.setFlash("No newer review", 2*time.Second, viewQueue)
		}
	case viewReview:
		if m.reviewScroll > 0 {
			m.reviewScroll--
		}
	case viewKindPrompt:
		if m.promptScroll > 0 {
			m.promptScroll--
		}
	case viewCommitMsg:
		if m.commitMsgScroll > 0 {
			m.commitMsgScroll--
		}
	case viewHelp:
		if m.helpScroll > 0 {
			m.helpScroll--
		}
	}
	return m, nil
}

func (m model) handleNextKey() (tea.Model, tea.Cmd) {
	switch m.currentView {
	case viewQueue:
		nextIdx := m.findNextVisibleJob(m.selectedIdx)
		if nextIdx >= 0 {
			m.selectedIdx = nextIdx
			m.updateSelectedJobID()
		}
	case viewReview:
		nextIdx := m.findNextViewableJob()
		if nextIdx >= 0 {
			m.closeFixPanel()
			m.selectedIdx = nextIdx
			m.updateSelectedJobID()
			m.reviewScroll = 0
			job := m.jobs[nextIdx]
			switch job.Status {
			case storage.JobStatusDone:
				return m, m.fetchReview(job.ID)
			case storage.JobStatusFailed:
				m.currentBranch = ""
				m.currentReview = &storage.Review{
					Agent:  job.Agent,
					Output: "Job failed:\n\n" + job.Error,
					Job:    &job,
				}
			}
		} else {
			m.setFlash("No newer review", 2*time.Second, viewReview)
		}
	case viewKindPrompt:
		nextIdx := m.findNextPromptableJob()
		if nextIdx >= 0 {
			m.selectedIdx = nextIdx
			m.updateSelectedJobID()
			m.promptScroll = 0
			job := m.jobs[nextIdx]
			if job.Status == storage.JobStatusDone {
				return m, m.fetchReviewForPrompt(job.ID)
			} else if (job.Status == storage.JobStatusRunning || job.Status == storage.JobStatusQueued) && job.Prompt != "" {
				m.currentReview = &storage.Review{
					Agent:  job.Agent,
					Prompt: job.Prompt,
					Job:    &job,
				}
			}
		} else {
			m.setFlash("No newer review", 2*time.Second, viewKindPrompt)
		}
	case viewLog:
		if m.logFromView == viewTasks {
			idx := m.findNextLoggableFixJob()
			if idx >= 0 {
				m.fixSelectedIdx = idx
				job := m.fixJobs[idx]
				m.logStreaming = false
				return m.openLogView(
					job.ID, job.Status, viewTasks,
				)
			}
		} else {
			nextIdx := m.findNextLoggableJob()
			if nextIdx >= 0 {
				m.selectedIdx = nextIdx
				m.updateSelectedJobID()
				job := m.jobs[nextIdx]
				m.logStreaming = false
				return m.openLogView(
					job.ID, job.Status, m.logFromView,
				)
			}
		}
		m.setFlash("No newer log", 2*time.Second, viewLog)
	}
	return m, nil
}

func (m model) handleDownKey() (tea.Model, tea.Cmd) {
	switch m.currentView {
	case viewQueue:
		prevIdx := m.findPrevVisibleJob(m.selectedIdx)
		if prevIdx >= 0 {
			m.selectedIdx = prevIdx
			m.updateSelectedJobID()
			if cmd := m.maybePrefetch(prevIdx); cmd != nil {
				return m, cmd
			}
		} else if m.canPaginate() {
			m.loadingMore = true
			return m, m.fetchMoreJobs()
		} else if !m.hasMore || len(m.activeRepoFilter) > 1 {
			m.setFlash("No older review", 2*time.Second, viewQueue)
		}
	case viewReview:
		m.reviewScroll++
		if m.mdCache != nil && m.reviewScroll > m.mdCache.lastReviewMaxScroll {
			m.reviewScroll = m.mdCache.lastReviewMaxScroll
		}
	case viewKindPrompt:
		m.promptScroll++
		if m.mdCache != nil && m.promptScroll > m.mdCache.lastPromptMaxScroll {
			m.promptScroll = m.mdCache.lastPromptMaxScroll
		}
	case viewCommitMsg:
		m.commitMsgScroll++
	case viewHelp:
		m.helpScroll = min(m.helpScroll+1, m.helpMaxScroll())
	}
	return m, nil
}

func (m model) handlePrevKey() (tea.Model, tea.Cmd) {
	switch m.currentView {
	case viewQueue:
		prevIdx := m.findPrevVisibleJob(m.selectedIdx)
		if prevIdx >= 0 {
			m.selectedIdx = prevIdx
			m.updateSelectedJobID()
			if cmd := m.maybePrefetch(prevIdx); cmd != nil {
				return m, cmd
			}
		} else if m.canPaginate() {
			m.loadingMore = true
			return m, m.fetchMoreJobs()
		}
	case viewReview:
		prevIdx := m.findPrevViewableJob()
		if prevIdx >= 0 {
			m.closeFixPanel()
			m.selectedIdx = prevIdx
			m.updateSelectedJobID()
			m.reviewScroll = 0
			job := m.jobs[prevIdx]
			switch job.Status {
			case storage.JobStatusDone:
				return m, m.fetchReview(job.ID)
			case storage.JobStatusFailed:
				m.currentBranch = ""
				m.currentReview = &storage.Review{
					Agent:  job.Agent,
					Output: "Job failed:\n\n" + job.Error,
					Job:    &job,
				}
			}
		} else if m.canPaginate() {
			m.loadingMore = true
			m.paginateNav = viewReview
			return m, m.fetchMoreJobs()
		} else {
			m.setFlash("No older review", 2*time.Second, viewReview)
		}
	case viewKindPrompt:
		prevIdx := m.findPrevPromptableJob()
		if prevIdx >= 0 {
			m.selectedIdx = prevIdx
			m.updateSelectedJobID()
			m.promptScroll = 0
			job := m.jobs[prevIdx]
			if job.Status == storage.JobStatusDone {
				return m, m.fetchReviewForPrompt(job.ID)
			} else if (job.Status == storage.JobStatusRunning || job.Status == storage.JobStatusQueued) && job.Prompt != "" {
				m.currentReview = &storage.Review{
					Agent:  job.Agent,
					Prompt: job.Prompt,
					Job:    &job,
				}
			}
		} else if m.canPaginate() {
			m.loadingMore = true
			m.paginateNav = viewKindPrompt
			return m, m.fetchMoreJobs()
		} else {
			m.setFlash("No older review", 2*time.Second, viewKindPrompt)
		}
	case viewLog:
		if m.logFromView == viewTasks {
			idx := m.findPrevLoggableFixJob()
			if idx >= 0 {
				m.fixSelectedIdx = idx
				job := m.fixJobs[idx]
				m.logStreaming = false
				return m.openLogView(
					job.ID, job.Status, viewTasks,
				)
			}
		} else {
			prevIdx := m.findPrevLoggableJob()
			if prevIdx >= 0 {
				m.selectedIdx = prevIdx
				m.updateSelectedJobID()
				job := m.jobs[prevIdx]
				m.logStreaming = false
				return m.openLogView(
					job.ID, job.Status, m.logFromView,
				)
			} else if m.canPaginate() {
				m.loadingMore = true
				m.paginateNav = viewLog
				return m, m.fetchMoreJobs()
			}
		}
		m.setFlash("No older log", 2*time.Second, viewLog)
	}
	return m, nil
}

func (m model) handlePageUpKey() (tea.Model, tea.Cmd) {
	pageSize := max(1, m.height-10)
	switch m.currentView {
	case viewQueue:
		for range pageSize {
			nextIdx := m.findNextVisibleJob(m.selectedIdx)
			if nextIdx < 0 {
				break
			}
			m.selectedIdx = nextIdx
		}
		m.updateSelectedJobID()
	case viewReview:
		if m.mdCache != nil && m.reviewScroll > m.mdCache.lastReviewMaxScroll {
			m.reviewScroll = m.mdCache.lastReviewMaxScroll
		}
		m.reviewScroll = max(0, m.reviewScroll-pageSize)
		return m, tea.ClearScreen
	case viewKindPrompt:
		if m.mdCache != nil && m.promptScroll > m.mdCache.lastPromptMaxScroll {
			m.promptScroll = m.mdCache.lastPromptMaxScroll
		}
		m.promptScroll = max(0, m.promptScroll-pageSize)
		return m, tea.ClearScreen
	case viewHelp:
		m.helpScroll = max(0, m.helpScroll-pageSize)
	}
	return m, nil
}

func (m model) handlePageDownKey() (tea.Model, tea.Cmd) {
	pageSize := max(1, m.height-10)
	switch m.currentView {
	case viewQueue:
		reachedEnd := false
		for range pageSize {
			prevIdx := m.findPrevVisibleJob(m.selectedIdx)
			if prevIdx < 0 {
				reachedEnd = true
				break
			}
			m.selectedIdx = prevIdx
		}
		m.updateSelectedJobID()
		if reachedEnd && m.canPaginate() {
			m.loadingMore = true
			return m, m.fetchMoreJobs()
		}
		if cmd := m.maybePrefetch(m.selectedIdx); cmd != nil {
			return m, cmd
		}
	case viewReview:
		m.reviewScroll += pageSize
		if m.mdCache != nil && m.reviewScroll > m.mdCache.lastReviewMaxScroll {
			m.reviewScroll = m.mdCache.lastReviewMaxScroll
		}
		return m, tea.ClearScreen
	case viewKindPrompt:
		m.promptScroll += pageSize
		if m.mdCache != nil && m.promptScroll > m.mdCache.lastPromptMaxScroll {
			m.promptScroll = m.mdCache.lastPromptMaxScroll
		}
		return m, tea.ClearScreen
	case viewHelp:
		m.helpScroll = min(m.helpScroll+pageSize, m.helpMaxScroll())
	}
	return m, nil
}

func (m model) handleHelpKey() (tea.Model, tea.Cmd) {
	if m.currentView == viewHelp {
		m.currentView = m.helpFromView
		return m, nil
	}
	if m.currentView == viewQueue || m.currentView == viewReview || m.currentView == viewKindPrompt || m.currentView == viewLog {
		m.helpFromView = m.currentView
		m.currentView = viewHelp
		m.helpScroll = 0
		return m, nil
	}
	return m, nil
}

func (m model) handleEscKey() (tea.Model, tea.Cmd) {
	if m.currentView == viewQueue && len(m.filterStack) > 0 {
		popped := m.popFilter()
		if popped == filterTypeRepo || popped == filterTypeBranch {
			m.hasMore = false
			m.selectedIdx = -1
			m.selectedJobID = 0
			m.fetchSeq++
			m.loadingJobs = true
			return m, m.fetchJobs()
		}
		return m, nil
	} else if m.currentView == viewQueue && m.hideClosed {
		m.hideClosed = false
		m.queueColGen++
		m.hasMore = false
		m.selectedIdx = -1
		m.selectedJobID = 0
		m.fetchSeq++
		m.loadingJobs = true
		return m, m.fetchJobs()
	} else if m.currentView == viewReview {
		// If fix panel is open (unfocused), esc closes it rather than leaving the review
		if m.reviewFixPanelOpen {
			m.closeFixPanel()
			return m, nil
		}
		m.closeFixPanel()
		returnTo := m.reviewFromView
		if returnTo == 0 {
			returnTo = viewQueue
		}
		m.currentView = returnTo
		m.currentReview = nil
		m.reviewScroll = 0
		m.paginateNav = 0
		if returnTo == viewQueue {
			m.normalizeSelectionIfHidden()
			if m.hideClosed && !m.loadingJobs {
				m.loadingJobs = true
				return m, m.fetchJobs()
			}
		}
	} else if m.currentView == viewKindPrompt {
		m.paginateNav = 0
		if m.promptFromQueue {
			m.currentView = viewQueue
			m.currentReview = nil
			m.promptScroll = 0
		} else {
			m.currentView = viewReview
			m.promptScroll = 0
		}
	} else if m.currentView == viewCommitMsg {
		m.currentView = m.commitMsgFromView
		m.commitMsgContent = ""
		m.commitMsgScroll = 0
	} else if m.currentView == viewHelp {
		m.currentView = m.helpFromView
	}
	return m, nil
}

// openLogView opens the log view for a job of any status.
// Running jobs stream with follow; completed jobs show a static view.
func (m model) openLogView(
	jobID int64, status storage.JobStatus, fromView viewKind,
) (tea.Model, tea.Cmd) {
	m.logReviewAnchored = m.isReviewAnchored()
	m.logJobID = jobID
	m.logLines = nil
	m.logScroll = 0
	m.logFromView = fromView
	m.currentView = viewLog
	m.logOffset = 0
	m.logFmtr = streamfmt.NewWithWidth(
		io.Discard, m.width, m.glamourStyle,
	)
	m.logFetchSeq++
	m.logLoading = true

	if status == storage.JobStatusRunning {
		m.logStreaming = true
		m.logFollow = true
	} else {
		m.logStreaming = false
		m.logFollow = false
	}

	return m, tea.Batch(tea.ClearScreen, m.fetchJobLog(jobID))
}

// handleConnectionError tracks consecutive connection errors and triggers reconnection.
func (m *model) handleConnectionError(err error) tea.Cmd {
	if isConnectionError(err) {
		m.consecutiveErrors++
		if m.consecutiveErrors >= 3 && !m.reconnecting {
			m.reconnecting = true
			return m.tryReconnect()
		}
	}
	return nil
}
