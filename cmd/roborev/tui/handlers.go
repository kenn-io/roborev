package tui

import (
	"io"
	"time"

	tea "charm.land/bubbletea/v2"

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
	mouse := msg.Mouse()
	switch msg.(type) {
	case tea.MouseWheelMsg:
		switch mouse.Button {
		case tea.MouseWheelUp:
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
		case tea.MouseWheelDown:
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
		default:
			return m, nil
		}
	case tea.MouseClickMsg:
		if mouse.Button != tea.MouseLeft {
			return m, nil
		}
		switch m.currentView {
		case viewQueue:
			m.handleQueueMouseClick(mouse.X, mouse.Y)
		case viewTasks:
			m.handleTasksMouseClick(mouse.Y)
		case viewColumnOptions:
			return m.handleColumnOptionsMouseClick(mouse.Y)
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
	case "j":
		return m.handlePrevKey()
	case "left":
		return m.handleLeftKey()
	case "down":
		return m.handleDownKey()
	case "k":
		return m.handleNextKey()
	case "right":
		return m.handleRightKey()
	case "pgup":
		return m.handlePageUpKey()
	case "pgdown":
		return m.handlePageDownKey()
	case "space":
		return m.handleToggleExpand()
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
		rows := m.visibleQueueRows()
		if len(rows) > 0 {
			m = m.moveSelectionToJobID(rows[0].job.ID)
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

// handleToggleExpand toggles the selected panel parent. No-op off the queue or
// on a non-parent row. Dispatches a member fetch the first time an uncached
// panel is expanded; a failed fetch leaves the panel uncached so a later expand
// retries.
func (m model) handleToggleExpand() (tea.Model, tea.Cmd) {
	if m.currentView != viewQueue {
		return m, nil
	}
	rows := m.visibleQueueRows()
	idx := m.selectedRowIndex(rows)
	if idx < 0 || !rows[idx].hasChildren {
		return m, nil
	}
	uuid := rows[idx].job.PanelRunUUID
	m.queueColGen++
	if m.expandedPanels[uuid] {
		delete(m.expandedPanels, uuid)
		return m, nil
	}
	m.expandedPanels[uuid] = true
	if _, ok := m.panelMembers[uuid]; !ok {
		return m, m.fetchPanelMembers(uuid)
	}
	return m, nil
}

func (m model) handleRightKey() (tea.Model, tea.Cmd) {
	if m.currentView != viewQueue {
		return m.handleNextKey()
	}
	rows := m.visibleQueueRows()
	idx := m.selectedRowIndex(rows)
	if idx < 0 || !rows[idx].hasChildren {
		return m.handleNextKey()
	}
	uuid := rows[idx].job.PanelRunUUID
	if m.expandedPanels[uuid] {
		return m, nil
	}
	m.queueColGen++
	m.expandedPanels[uuid] = true
	if _, ok := m.panelMembers[uuid]; !ok {
		return m, m.fetchPanelMembers(uuid)
	}
	return m, nil
}

func (m model) handleLeftKey() (tea.Model, tea.Cmd) {
	if m.currentView != viewQueue {
		return m.handlePrevKey()
	}
	rows := m.visibleQueueRows()
	idx := m.selectedRowIndex(rows)
	if idx < 0 {
		return m.handlePrevKey()
	}
	row := rows[idx]
	if row.hasChildren {
		if m.expandedPanels[row.job.PanelRunUUID] {
			m.queueColGen++
			delete(m.expandedPanels, row.job.PanelRunUUID)
		}
		return m, nil
	}
	if row.depth == 1 && row.job.PanelRunUUID != "" {
		for i := idx - 1; i >= 0; i-- {
			parent := rows[i]
			if parent.depth == 0 && parent.job.PanelRunUUID == row.job.PanelRunUUID {
				m.queueColGen++
				delete(m.expandedPanels, row.job.PanelRunUUID)
				m = m.moveSelectionToJobID(parent.job.ID)
				return m, nil
			}
		}
	}
	return m.handlePrevKey()
}

func (m model) handleUpKey() (tea.Model, tea.Cmd) {
	switch m.currentView {
	case viewQueue:
		prevID := m.selectedJobID
		m = m.moveQueueSelection(-1)
		// id unchanged after a clamped move ⇒ already at the top; re-flash the boundary hint.
		if m.selectedJobID == prevID {
			m.setFlash("No newer review", 2*time.Second, viewQueue)
		}
		return m, nil
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

// contentNavBoundary applies the direction-specific behavior when no eligible
// row exists. dir is the nav direction (-1 = newer, +1 = older), the single
// source of truth from which older is derived. The "newer" direction flashes;
// the "older" direction resumes pagination (parent-only — gated on
// selectedIdx >= 0 so a member never sets paginateNav, since the handlers_msg
// resume is parent-only) or flashes. The returned cmd is non-nil only when
// pagination was started.
//
// Intentional v1 limitation: older-nav from a selected member (selectedIdx ==
// -1) never paginates — it flashes the boundary instead. Resuming pagination
// from a member would require new parent-anchored resume state to re-find the
// member's panel after the page lands, which v1 does not implement. The
// selectedIdx >= 0 gate enforces this no-op (see TestMemberAtBoundaryDoesNotPaginate).
func (m *model) contentNavBoundary(view viewKind, dir int) tea.Cmd {
	older := dir > 0
	verb := "newer"
	if older {
		verb = "older"
	}
	label := "review"
	if view == viewLog {
		label = "log"
	}
	if older && m.canPaginate() && m.selectedIdx >= 0 {
		m.loadingMore = true
		m.paginateNav = view
		return m.fetchMoreJobs()
	}
	m.setFlash("No "+verb+" "+label, 2*time.Second, view)
	return nil
}

// resolveAbsentSelection handles a content-nav step whose selection left the
// flattened rows (present=false). An absent member flashes and stops (ok=false,
// nil cmd → flash-and-stay). An absent parent/normal job hidden by hide-closed
// or a filter falls back to the adjacent visible job (ok=true), or stops at the
// boundary via contentNavBoundary (ok=false, cmd set). The returned job is
// meaningful only when ok is true.
func (m *model) resolveAbsentSelection(
	view viewKind, dir int, eligible func(storage.ReviewJob) bool,
) (storage.ReviewJob, tea.Cmd, bool) {
	if m.selectedIsMember() {
		m.setFlash("Selection no longer visible", 2*time.Second, view)
		return storage.ReviewJob{}, nil, false
	}
	idx := m.stepVisibleJobIndex(dir, eligible)
	if idx < 0 {
		return storage.ReviewJob{}, m.contentNavBoundary(view, dir), false
	}
	return m.jobs[idx], nil, true
}

// stepReviewNav walks the flattened rows for the queue-origin review view in
// direction dir (-1 = newer, +1 = older) and opens the target review, or
// flashes/paginates at the boundary. Never indexes m.jobs by a stale index.
func (m model) stepReviewNav(dir int) (tea.Model, tea.Cmd) {
	job, found, present := m.contentNavStep(dir, eligibleReviewRow)
	if !present {
		fallbackJob, cmd, ok := m.resolveAbsentSelection(viewReview, dir, eligibleReviewRow)
		if !ok {
			return m, cmd
		}
		job = fallbackJob
	} else if !found {
		return m, m.contentNavBoundary(viewReview, dir)
	}
	m.closeFixPanel()
	m = m.moveSelectionToJobID(job.ID)
	m.reviewScroll = 0
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
	return m, nil
}

// stepPromptNav walks the flattened rows for the queue-origin prompt view in
// direction dir (-1 = newer, +1 = older) and opens the target prompt, or
// flashes/paginates at the boundary.
func (m model) stepPromptNav(dir int) (tea.Model, tea.Cmd) {
	job, found, present := m.contentNavStep(dir, eligiblePromptRow)
	if !present {
		fallbackJob, cmd, ok := m.resolveAbsentSelection(viewKindPrompt, dir, eligiblePromptRow)
		if !ok {
			return m, cmd
		}
		job = fallbackJob
	} else if !found {
		return m, m.contentNavBoundary(viewKindPrompt, dir)
	}
	m = m.moveSelectionToJobID(job.ID)
	m.promptScroll = 0
	if job.Status == storage.JobStatusDone {
		return m, m.fetchReviewForPrompt(job.ID)
	}
	if (job.Status == storage.JobStatusRunning || job.Status == storage.JobStatusQueued) && job.Prompt != "" {
		m.currentReview = &storage.Review{
			Agent:  job.Agent,
			Prompt: job.Prompt,
			Job:    &job,
		}
	}
	return m, nil
}

// stepLogNav walks the flattened rows for the queue-origin log view in direction
// dir (-1 = newer, +1 = older) and opens the target log, or flashes/paginates
// at the boundary.
func (m model) stepLogNav(dir int) (tea.Model, tea.Cmd) {
	job, found, present := m.contentNavStep(dir, eligibleLogRow)
	if !present {
		fallbackJob, cmd, ok := m.resolveAbsentSelection(viewLog, dir, eligibleLogRow)
		if !ok {
			return m, cmd
		}
		job = fallbackJob
	} else if !found {
		return m, m.contentNavBoundary(viewLog, dir)
	}
	m = m.moveSelectionToJobID(job.ID)
	m.logStreaming = false
	return m.openLogView(job.ID, job.Status, m.logFromView)
}

func (m model) handleNextKey() (tea.Model, tea.Cmd) {
	switch m.currentView {
	case viewQueue:
		return m.moveQueueSelection(-1), nil
	case viewReview:
		return m.stepReviewNav(-1)
	case viewKindPrompt:
		return m.stepPromptNav(-1)
	case viewLog:
		if m.logFromView == viewTasks {
			return m.nextFixLog()
		}
		return m.stepLogNav(-1)
	}
	return m, nil
}

// nextFixLog steps to the next (newer) tasks-origin fix-job log, or flashes the
// boundary. Tasks-origin log nav is index-based over m.fixJobs and unchanged.
func (m model) nextFixLog() (tea.Model, tea.Cmd) {
	idx := m.findNextLoggableFixJob()
	if idx >= 0 {
		m.fixSelectedIdx = idx
		job := m.fixJobs[idx]
		m.logStreaming = false
		return m.openLogView(job.ID, job.Status, viewTasks)
	}
	m.setFlash("No newer log", 2*time.Second, viewLog)
	return m, nil
}

func (m model) handleDownKey() (tea.Model, tea.Cmd) {
	switch m.currentView {
	case viewQueue:
		prevID := m.selectedJobID
		m = m.moveQueueSelection(+1)
		if m.selectedJobID != prevID {
			if cmd := m.maybePrefetch(m.selectedIdx); cmd != nil {
				return m, cmd
			}
		} else if m.canPaginate() {
			m.loadingMore = true
			return m, m.fetchMoreJobs()
		} else if !m.hasMore || m.activeBranchFilter == branchNone {
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
		return m.prevQueueSelection()
	case viewReview:
		return m.stepReviewNav(+1)
	case viewKindPrompt:
		return m.stepPromptNav(+1)
	case viewLog:
		if m.logFromView == viewTasks {
			return m.prevFixLog()
		}
		return m.stepLogNav(+1)
	}
	return m, nil
}

// prevQueueSelection moves the queue cursor to the older (higher-index) row,
// prefetching when near the end or resuming pagination at the bottom boundary.
func (m model) prevQueueSelection() (tea.Model, tea.Cmd) {
	prevID := m.selectedJobID
	m = m.moveQueueSelection(+1)
	if m.selectedJobID != prevID {
		if cmd := m.maybePrefetch(m.selectedIdx); cmd != nil {
			return m, cmd
		}
	} else if m.canPaginate() {
		m.loadingMore = true
		return m, m.fetchMoreJobs()
	}
	return m, nil
}

// prevFixLog steps to the previous (older) tasks-origin fix-job log, or flashes
// the boundary. Tasks-origin log nav is index-based over m.fixJobs and unchanged.
func (m model) prevFixLog() (tea.Model, tea.Cmd) {
	idx := m.findPrevLoggableFixJob()
	if idx >= 0 {
		m.fixSelectedIdx = idx
		job := m.fixJobs[idx]
		m.logStreaming = false
		return m.openLogView(job.ID, job.Status, viewTasks)
	}
	m.setFlash("No older log", 2*time.Second, viewLog)
	return m, nil
}

func (m model) handlePageUpKey() (tea.Model, tea.Cmd) {
	pageSize := max(1, m.height-10)
	switch m.currentView {
	case viewQueue:
		return m.moveQueueSelection(-pageSize), nil
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
		rows := m.visibleQueueRows()
		idx := m.selectedRowIndex(rows)
		reachedEnd := idx >= 0 && idx+pageSize >= len(rows)
		m = m.moveQueueSelection(+pageSize)
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
			m.resetQueueForFilterChange()
			return m, m.fetchJobs()
		}
		return m, nil
	} else if m.currentView == viewQueue && m.hideClosed {
		m.hideClosed = false
		m.resetQueueForFilterChange()
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
