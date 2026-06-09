package tui

import (
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
	"go.kenn.io/roborev/internal/version"
)

// handleJobsMsg processes job list updates from the server.
func (m model) handleJobsMsg(msg jobsMsg) (tea.Model, tea.Cmd) {
	// Discard stale responses from before a filter change.
	if msg.seq < m.fetchSeq {
		m.paginateNav = 0
		m.loadingMore = false
		return m, nil
	}

	if msg.append || m.paginateNav == 0 {
		m.loadingMore = false
	}
	if !msg.append {
		m.loadingJobs = false
	}
	m.consecutiveErrors = 0

	m.hasMore = msg.hasMore

	m.updateDisplayNameCache(msg.jobs)

	if msg.append {
		m.jobs = append(m.jobs, msg.jobs...)
	} else {
		m.jobs = msg.jobs
	}
	m.queueColGen++

	// Clear pending closed states that server has confirmed
	for jobID, pending := range m.pendingClosed {
		found := false
		for i := range m.jobs {
			if m.jobs[i].ID == jobID {
				found = true
				serverState := m.jobs[i].Closed != nil && *m.jobs[i].Closed
				if serverState == pending.newState {
					delete(m.pendingClosed, jobID)
				}
				break
			}
		}
		// When hideClosed is active, closed jobs are filtered
		// out of the response. If a pending "mark closed" job is
		// absent from the response, that confirms the server absorbed
		// the change — clear it to prevent delta double-counting.
		if !found && m.hideClosed && pending.newState {
			delete(m.pendingClosed, jobID)
		}
	}

	if !msg.append {
		m.jobStats = msg.stats
		// Re-apply only unconfirmed pending deltas so that
		// rollback math stays correct without double-counting
		// entries the server has already absorbed.
		for _, pending := range m.pendingClosed {
			m.applyStatsDelta(pending.newState)
		}
	}

	// Apply any remaining pending closed changes to prevent flash
	for i := range m.jobs {
		if pending, ok := m.pendingClosed[m.jobs[i].ID]; ok {
			newState := pending.newState
			m.jobs[i].Closed = &newState
		}
	}

	// Selection management
	if m.selectedIsMember() {
		// A panel member is selected. Members are side-fetched and absent
		// from the parents-only m.jobs, so keep the id authoritative and
		// leave selectedIdx as a no-op hint; moveQueueSelection re-derives
		// the cursor from the flattened rows on the next nav key.
		m.selectedIdx = -1
	} else if len(m.jobs) == 0 {
		m.selectedIdx = -1
		if !m.isReviewAnchored() {
			m.selectedJobID = 0
		}
	} else if m.selectedJobID > 0 {
		found := false
		for i, job := range m.jobs {
			if job.ID == m.selectedJobID {
				m.selectedIdx = i
				found = true
				break
			}
		}

		if !found && m.isReviewAnchored() {
			// Review-rooted views: leave selectedIdx/selectedJobID
			// as-is so ←/→ navigation stays anchored to the displayed
			// review's position. normalizeSelectionIfHidden adjusts
			// on return to queue.
		} else if !found {
			m.selectedIdx = max(0, min(len(m.jobs)-1, m.selectedIdx))
			if len(m.activeRepoFilter) > 0 || m.hideClosed {
				idx := m.findNearestVisibleJob(m.selectedIdx)
				if idx >= 0 {
					m.selectedIdx = idx
					m.selectedJobID = m.jobs[idx].ID
				} else {
					m.selectedIdx = -1
					m.selectedJobID = 0
				}
			} else {
				m.selectedJobID = m.jobs[m.selectedIdx].ID
			}
		} else if !m.isJobVisible(m.jobs[m.selectedIdx]) && m.currentView == viewQueue {
			// Only adjust selection in queue view. In review/prompt/log
			// views, ←/→ navigation is relative to the viewed job's
			// position; normalizeSelectionIfHidden handles it on return
			// to queue.
			idx := m.findNearestVisibleJob(m.selectedIdx)
			if idx >= 0 {
				m.selectedIdx = idx
				m.selectedJobID = m.jobs[idx].ID
			} else {
				m.selectedIdx = -1
				m.selectedJobID = 0
			}
		}
	} else if m.currentView == viewReview && m.currentReview != nil && m.currentReview.Job != nil {
		targetID := m.currentReview.Job.ID
		for i, job := range m.jobs {
			if job.ID == targetID {
				m.selectedIdx = i
				m.selectedJobID = targetID
				break
			}
		}
		if m.selectedJobID == 0 {
			m.selectedIdx = 0
			m.selectedJobID = m.jobs[0].ID
		}
	} else {
		firstVisible := m.findFirstVisibleJob()
		if firstVisible >= 0 {
			m.selectedIdx = firstVisible
			m.selectedJobID = m.jobs[firstVisible].ID
		} else if len(m.activeRepoFilter) == 0 && len(m.jobs) > 0 {
			m.selectedIdx = 0
			m.selectedJobID = m.jobs[0].ID
		} else {
			m.selectedIdx = -1
			m.selectedJobID = 0
		}
	}

	// Auto-paginate when hide-closed hides too many jobs
	if m.currentView == viewQueue &&
		m.hideClosed &&
		m.canPaginate() &&
		len(m.getVisibleJobs()) < m.queueVisibleRows() {
		m.loadingMore = true
		return m, m.fetchMoreJobs()
	}

	// Auto-navigate after pagination triggered from review/prompt view
	if msg.append && m.paginateNav != 0 && m.currentView == m.paginateNav {
		nav := m.paginateNav
		m.paginateNav = 0
		switch nav {
		case viewReview:
			nextIdx := m.stepVisibleJobIndex(1, eligibleReviewRow)
			if nextIdx >= 0 {
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
			}
		case viewKindPrompt:
			nextIdx := m.stepVisibleJobIndex(1, eligiblePromptRow)
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
			}
		case viewLog:
			nextIdx := m.stepVisibleJobIndex(1, eligibleLogRow)
			if nextIdx >= 0 {
				m.selectedIdx = nextIdx
				m.updateSelectedJobID()
				job := m.jobs[nextIdx]
				return m.openLogView(
					job.ID, job.Status, m.logFromView,
				)
			}
		}
	} else if !msg.append && !m.loadingMore {
		m.paginateNav = 0
	}

	cmds := []tea.Cmd{m.consumeSSEPendingRefresh()}
	for _, uuid := range m.staleExpandedPanelRuns() {
		cmds = append(cmds, m.fetchPanelMembers(uuid))
	}
	return m, tea.Batch(cmds...)
}

// staleExpandedPanelRuns returns the panel_run_uuids of expanded panels whose
// synthesis parent is still in m.jobs and whose cached members include at least
// one non-terminal (queued/running) row, so the cache should be refreshed.
// Collapsed and all-terminal panels are skipped; nil when nothing is expanded.
func (m model) staleExpandedPanelRuns() []string {
	if len(m.expandedPanels) == 0 {
		return nil
	}
	visible := make(map[string]bool, len(m.jobs))
	for i := range m.jobs {
		if u := m.jobs[i].PanelRunUUID; u != "" && m.jobs[i].IsSynthesisJob() {
			visible[u] = true
		}
	}
	var runs []string
	for uuid := range m.expandedPanels {
		if !visible[uuid] {
			continue
		}
		for _, mem := range m.panelMembers[uuid] {
			if mem.Status == storage.JobStatusQueued || mem.Status == storage.JobStatusRunning {
				runs = append(runs, uuid)
				break
			}
		}
	}
	sort.Strings(runs)
	return runs
}

// handlePanelMembersMsg caches side-fetched panel members on success. On error
// it flashes and leaves the panel uncached, so a later expand retries.
func (m model) handlePanelMembersMsg(msg panelMembersMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.setFlash("Couldn't load panel members — collapse and expand to retry",
			3*time.Second, viewQueue)
		return m, nil
	}
	m.panelMembers[msg.runUUID] = msg.members
	m.queueColGen++
	return m, nil
}

// handleStatusMsg processes daemon status updates.
func (m model) handleStatusMsg(msg statusMsg) (tea.Model, tea.Cmd) {
	if msg.gen < m.fetchGen {
		return m, nil // discard pre-reconnect response
	}
	m.loadingStatus = false
	m.status = msg.status
	m.consecutiveErrors = 0
	if m.status.Version != "" {
		m.daemonVersion = m.status.Version
		m.versionMismatch = m.daemonVersion != version.Version
	}
	if m.statusFetchedOnce && m.status.ConfigReloadCounter != m.lastConfigReloadCounter {
		m.setFlash("Config reloaded", 5*time.Second, m.currentView)
	}
	m.lastConfigReloadCounter = m.status.ConfigReloadCounter
	m.statusFetchedOnce = true
	if m.statusStale {
		m.statusStale = false
		m.loadingStatus = true
		return m, m.fetchStatus()
	}
	return m, nil
}

// handleRepoNamesMsg stores the display-name-to-root-paths mapping
// fetched from /api/repos at init, used by control socket set-filter.
func (m model) handleRepoNamesMsg(
	msg repoNamesMsg,
) (tea.Model, tea.Cmd) {
	if msg.names != nil {
		m.repoNames = msg.names
		m.repoIdentities = msg.identities
		if m.reconcileAutoRepoFilter() {
			return m, m.fetchJobs()
		}
	}
	return m, nil
}

// handleReposMsg processes repo list results for the filter modal.
func (m model) handleReposMsg(
	msg reposMsg,
) (tea.Model, tea.Cmd) {
	m.consecutiveErrors = 0
	refetchJobs := false

	// Refresh repoNames when the modal fetch was unfiltered (no
	// branch constraint), so newly registered repos are picked up.
	// Skip branch-filtered responses — they are a subset and would
	// clobber the authoritative mapping.
	if !msg.branchFiltered {
		names := make(map[string][]string, len(msg.repos))
		for _, r := range msg.repos {
			names[r.name] = r.rootPaths
		}
		m.repoNames = names
		m.repoIdentities = msg.identities
		refetchJobs = m.reconcileAutoRepoFilter()
	}

	// Build filterTree from repos (all collapsed, no children)
	m.filterTree = make([]treeFilterNode, len(msg.repos))
	for i, r := range msg.repos {
		m.filterTree[i] = treeFilterNode{
			name:      r.name,
			rootPaths: r.rootPaths,
			count:     r.count,
		}
	}
	// Move cwd repo to first position for quick access
	if m.cwdRepoRoot != "" && len(m.filterTree) > 1 {
		moveToFront(m.filterTree, func(n treeFilterNode) bool {
			return slices.Contains(n.rootPaths, m.cwdRepoRoot)
		})
	}
	m.rebuildFilterFlatList()
	// Pre-select active filter if any
	if len(m.activeRepoFilter) > 0 {
		for i, entry := range m.filterFlatList {
			if entry.repoIdx >= 0 && entry.branchIdx == -1 &&
				rootPathsMatch(
					m.filterTree[entry.repoIdx].rootPaths,
					m.activeRepoFilter,
				) {
				m.filterSelectedIdx = i
				break
			}
		}
	}
	// Auto-expand repo to branches when opened via 'b' key
	if m.filterBranchMode && len(m.filterTree) > 0 {
		targetIdx := 0
		if len(m.activeRepoFilter) > 0 {
			for i, node := range m.filterTree {
				if rootPathsMatch(
					node.rootPaths, m.activeRepoFilter,
				) {
					targetIdx = i
					goto foundTarget
				}
			}
		}
		if m.cwdRepoRoot != "" {
			for i, node := range m.filterTree {
				for _, p := range node.rootPaths {
					if p == m.cwdRepoRoot {
						targetIdx = i
						goto foundTarget
					}
				}
			}
		}
	foundTarget:
		m.filterTree[targetIdx].loading = true
		for i, entry := range m.filterFlatList {
			if entry.repoIdx == targetIdx &&
				entry.branchIdx == -1 {
				m.filterSelectedIdx = i
				break
			}
		}
		branchCmd := m.fetchBranchesForRepo(
			m.filterTree[targetIdx].rootPaths,
			targetIdx, true, m.filterSearchSeq,
		)
		if refetchJobs {
			return m, tea.Batch(m.fetchJobs(), branchCmd)
		}
		return m, branchCmd
	}
	// If user typed search before repos loaded, kick off fetches
	if cmd := m.fetchUnloadedBranches(); cmd != nil {
		if refetchJobs {
			return m, tea.Batch(m.fetchJobs(), cmd)
		}
		return m, cmd
	}
	if refetchJobs {
		return m, m.fetchJobs()
	}
	return m, nil
}

// handleRepoBranchesMsg processes branch list results for a repo.
func (m model) handleRepoBranchesMsg(
	msg repoBranchesMsg,
) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
		m.filterBranchMode = false
		if msg.repoIdx >= 0 &&
			msg.repoIdx < len(m.filterTree) &&
			rootPathsMatch(
				m.filterTree[msg.repoIdx].rootPaths,
				msg.rootPaths,
			) {
			m.filterTree[msg.repoIdx].loading = false
			if !msg.expandOnLoad && m.filterSearch != "" &&
				msg.searchSeq == m.filterSearchSeq {
				m.filterTree[msg.repoIdx].fetchFailed = true
			}
		}
		if cmd := m.handleConnectionError(msg.err); cmd != nil {
			return m, cmd
		}
		return m, m.fetchUnloadedBranches()
	}
	// Verify filter view, repoIdx valid, and identity matches
	if m.currentView == viewFilter &&
		msg.repoIdx >= 0 &&
		msg.repoIdx < len(m.filterTree) &&
		rootPathsMatch(
			m.filterTree[msg.repoIdx].rootPaths,
			msg.rootPaths,
		) {
		m.consecutiveErrors = 0
		m.filterTree[msg.repoIdx].loading = false
		m.filterTree[msg.repoIdx].children = msg.branches
		if msg.expandOnLoad {
			m.filterTree[msg.repoIdx].expanded = true
		}
		// Move cwd branch to first position if this is cwd repo
		if m.cwdBranch != "" && len(msg.branches) > 1 {
			isCwdRepo := slices.Contains(
				m.filterTree[msg.repoIdx].rootPaths,
				m.cwdRepoRoot,
			)
			if isCwdRepo {
				moveToFront(
					m.filterTree[msg.repoIdx].children,
					func(b branchFilterItem) bool {
						return b.name == m.cwdBranch
					},
				)
			}
		}
		m.rebuildFilterFlatList()
		// Auto-position on first branch when opened via 'b'
		if m.filterBranchMode {
			m.filterBranchMode = false
			for i, entry := range m.filterFlatList {
				if entry.repoIdx == msg.repoIdx &&
					entry.branchIdx >= 0 {
					m.filterSelectedIdx = i
					break
				}
			}
		}
		if cmd := m.fetchUnloadedBranches(); cmd != nil {
			return m, cmd
		}
	}
	return m, nil
}

// handleBranchesMsg processes branch backfill completion.
func (m model) handleBranchesMsg(
	msg branchesMsg,
) (tea.Model, tea.Cmd) {
	m.consecutiveErrors = 0
	m.branchBackfillDone = true
	if msg.backfillCount > 0 {
		m.setFlash(fmt.Sprintf(
			"Backfilled branch info for %d jobs",
			msg.backfillCount,
		), 5*time.Second, viewFilter)
	}
	return m, nil
}

// handleFixJobsMsg processes fix job list results.
func (m model) handleFixJobsMsg(
	msg fixJobsMsg,
) (tea.Model, tea.Cmd) {
	if msg.gen < m.fetchGen {
		return m, nil // discard pre-reconnect response
	}
	m.loadingFixJobs = false
	if msg.err != nil {
		m.err = msg.err
	} else {
		m.fixJobs = msg.jobs
		m.taskColGen++
		if m.fixSelectedIdx >= len(m.fixJobs) &&
			len(m.fixJobs) > 0 {
			m.fixSelectedIdx = len(m.fixJobs) - 1
		}
	}
	// A state-mutating handler requested a refresh while this fetch was
	// in flight. The data we just received predates that mutation, so
	// dispatch a follow-up fetch to pick up the latest state.
	if m.fixJobsStale {
		m.fixJobsStale = false
		m.loadingFixJobs = true
		return m, m.fetchFixJobs()
	}
	return m, nil
}

// handleReviewMsg processes review fetch results.
func (m model) handleReviewMsg(
	msg reviewMsg,
) (tea.Model, tea.Cmd) {
	if msg.jobID != m.selectedJobID {
		// Stale fetch -- clear pending fix panel if it was
		// for this (now-discarded) review.
		if m.reviewFixPanelPending &&
			m.fixPromptJobID == msg.jobID {
			m.reviewFixPanelPending = false
			m.fixPromptJobID = 0
		}
		return m, nil
	}
	m.consecutiveErrors = 0
	m.currentReview = msg.review
	m.currentResponses = msg.responses
	m.currentBranch = msg.branchName
	m.currentView = viewReview
	m.reviewScroll = 0
	if m.reviewFixPanelPending &&
		m.fixPromptJobID == msg.review.JobID {
		m.reviewFixPanelPending = false
		m.reviewFixPanelOpen = true
		m.reviewFixPanelFocused = true
	}
	return m, nil
}

// handlePromptMsg processes prompt fetch results.
func (m model) handlePromptMsg(
	msg promptMsg,
) (tea.Model, tea.Cmd) {
	if msg.jobID != m.selectedJobID {
		return m, nil
	}
	m.consecutiveErrors = 0
	m.currentReview = msg.review
	m.currentView = viewKindPrompt
	m.promptScroll = 0
	return m, nil
}

// handleLogOutputMsg processes log output from the daemon.
func (m model) handleLogOutputMsg(
	msg logOutputMsg,
) (tea.Model, tea.Cmd) {
	// Drop stale responses from previous log sessions.
	if msg.seq != m.logFetchSeq {
		return m, nil
	}
	m.logLoading = false
	m.consecutiveErrors = 0
	// If the user navigated away while a fetch was in-flight, drop it.
	if m.currentView != viewLog {
		return m, nil
	}
	if msg.err != nil {
		if errors.Is(msg.err, errNoLog) {
			// Auto-design-router rows (running classify, terminal
			// skipped, failed/canceled classify) never produce a
			// streamed agent log because the classifier is a
			// one-shot SchemaAgent.Decide call. Stay in the log
			// view so renderLogView's classifyReasoningLines
			// header can show the verdict, skip reason, and any
			// classifier error — bouncing back to the queue
			// would hide that information behind a flash.
			job := m.logViewLookupJob()
			if job != nil && len(classifyReasoningLines(job, m.width)) > 0 {
				m.logLines = []logLine{}
				m.logStreaming = false
				return m, nil
			}
			flash := "No log available for this job"
			if job != nil &&
				job.Status == storage.JobStatusFailed &&
				job.Error != "" {
				flash = fmt.Sprintf(
					"Job #%d failed: %s",
					m.logJobID, job.Error,
				)
			}
			m.setFlash(flash, 5*time.Second, m.logFromView)
			m.currentView = m.logFromView
			m.logStreaming = false
			return m, nil
		}
		m.err = msg.err
		return m, nil
	}
	if m.currentView == viewLog {
		// Persist formatter state for incremental polls
		if msg.fmtr != nil {
			m.logFmtr = msg.fmtr
		}

		if msg.append {
			if len(msg.lines) > 0 {
				m.logLines = append(
					m.logLines, msg.lines...,
				)
			}
		} else if len(msg.lines) > 0 {
			m.logLines = msg.lines
		} else if m.logLines == nil {
			if !msg.hasMore {
				m.logLines = []logLine{}
			}
		} else if msg.newOffset == 0 {
			m.logLines = []logLine{}
		}
		m.logOffset = msg.newOffset
		m.logStreaming = msg.hasMore
		if m.logFollow && len(m.logLines) > 0 {
			visibleLines := m.logVisibleLines()
			maxScroll := max(len(m.logLines)-visibleLines, 0)
			m.logScroll = maxScroll
		}
		if m.logStreaming {
			return m, tea.Tick(
				500*time.Millisecond,
				func(t time.Time) tea.Msg {
					return logTickMsg{}
				},
			)
		}
	}
	return m, nil
}

// handleCommitMsgMsg processes commit message fetch results.
func (m model) handleCommitMsgMsg(
	msg commitMsgMsg,
) (tea.Model, tea.Cmd) {
	if msg.jobID != m.commitMsgJobID {
		return m, nil
	}
	if msg.err != nil {
		m.setFlash(msg.err.Error(), 2*time.Second, m.currentView)
		return m, nil
	}
	m.commitMsgContent = msg.content
	m.commitMsgScroll = 0
	m.currentView = viewCommitMsg
	return m, nil
}

// handleClosedResultMsg processes the result of a closed toggle API call.
func (m model) handleClosedResultMsg(msg closedResultMsg) (tea.Model, tea.Cmd) {
	isCurrentRequest := false
	if msg.jobID > 0 {
		if pending, ok := m.pendingClosed[msg.jobID]; ok && pending.seq == msg.seq {
			isCurrentRequest = true
		}
	} else if msg.reviewView && msg.reviewID > 0 {
		if pending, ok := m.pendingReviewClosed[msg.reviewID]; ok && pending.seq == msg.seq {
			isCurrentRequest = true
		}
	}

	if msg.err != nil {
		if isCurrentRequest {
			if msg.reviewView {
				if m.currentReview != nil && m.currentReview.ID == msg.reviewID {
					m.currentReview.Closed = msg.oldState
				}
			}
			if msg.jobID > 0 {
				m.setJobClosed(msg.jobID, msg.oldState)
				delete(m.pendingClosed, msg.jobID)
				if msg.restoreSelection {
					m.selectJobByID(msg.jobID)
				}
				// Reverse the optimistic stats delta
				m.applyStatsDelta(msg.oldState)
			} else if msg.reviewID > 0 {
				delete(m.pendingReviewClosed, msg.reviewID)
			}
			flashView := viewQueue
			if msg.reviewView {
				flashView = viewReview
			}
			m.setWarningFlash(msg.err.Error(), 3*time.Second, flashView)
			m.err = msg.err
		}
	} else {
		if isCurrentRequest && msg.jobID == 0 && msg.reviewID > 0 {
			delete(m.pendingReviewClosed, msg.reviewID)
		}
	}
	return m, nil
}

// handleClosedToggleMsg processes closed state toggle messages.
func (m model) handleClosedToggleMsg(
	msg closedMsg,
) (tea.Model, tea.Cmd) {
	if m.currentReview != nil {
		m.currentReview.Closed = bool(msg)
	}
	return m, nil
}

// handleCancelResultMsg processes job cancellation results.
func (m model) handleCancelResultMsg(
	msg cancelResultMsg,
) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.setJobStatus(msg.jobID, msg.oldState)
		m.setJobFinishedAt(msg.jobID, msg.oldFinishedAt)
		if msg.restoreSelection {
			m.selectJobByID(msg.jobID)
		}
		m.err = msg.err
	}
	return m, nil
}

// handleRerunResultMsg processes job re-run results.
func (m model) handleRerunResultMsg(
	msg rerunResultMsg,
) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.setJobStatus(msg.jobID, msg.oldState)
		m.setJobStartedAt(msg.jobID, msg.oldStartedAt)
		m.setJobFinishedAt(msg.jobID, msg.oldFinishedAt)
		m.setJobError(msg.jobID, msg.oldError)
		m.mutateJob(msg.jobID, func(job *storage.ReviewJob) {
			job.Closed = msg.oldClosed
			job.Verdict = msg.oldVerdict
		})
		m.err = msg.err
	}
	return m, nil
}

// handleCommentResultMsg processes comment submission results.
func (m model) handleCommentResultMsg(
	msg commentResultMsg,
) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
	} else {
		if m.commentJobID == msg.jobID {
			m.commentText = ""
			m.commentJobID = 0
		}
		if m.currentView == viewReview &&
			m.currentReview != nil &&
			m.currentReview.JobID == msg.jobID {
			return m, m.fetchReview(msg.jobID)
		}
	}
	return m, nil
}

// handleClipboardResultMsg processes clipboard copy results.
func (m model) handleClipboardResultMsg(
	msg clipboardResultMsg,
) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = fmt.Errorf("copy failed: %w", msg.err)
		m.setWarningFlash(clipboardErrorMessage(msg.err), 4*time.Second, msg.view)
	} else {
		m.setFlash("Copied to clipboard", 2*time.Second, msg.view)
	}
	return m, nil
}

// clipboardErrorMessage returns a user-friendly flash message for a
// clipboard write failure. The atotto/clipboard library returns a verbose
// "No clipboard utilities available..." error when no clipboard tool is
// installed; we substitute a shorter, actionable hint in that case.
func clipboardErrorMessage(err error) string {
	if strings.Contains(err.Error(), "No clipboard utilities available") {
		return "Copy failed: install xclip, wl-clipboard, or xsel"
	}
	return fmt.Sprintf("Copy failed: %v", err)
}

// handleSavePatchResultMsg processes save-patch-to-file results.
func (m model) handleSavePatchResultMsg(msg savePatchResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = fmt.Errorf("save patch failed: %w", msg.err)
	} else {
		m.setFlash("Saved to "+msg.path, 3*time.Second, viewPatch)
	}
	return m, nil
}

// handleSSEEventMsg processes real-time events from the daemon's NDJSON stream.
// Triggers an immediate data refresh and re-subscribes for the next event.
// If a fetch is already in flight, sets a pending flag so the refresh runs
// after the current load completes (avoiding stale data).
func (m model) handleSSEEventMsg() (tea.Model, tea.Cmd) {
	if m.loadingMore || m.loadingJobs {
		m.ssePendingRefresh = true
		return m, waitForSSE(m.sseCh, m.sseStop)
	}
	m.loadingJobs = true
	cmds := []tea.Cmd{
		m.fetchJobs(),
		waitForSSE(m.sseCh, m.sseStop),
	}
	// SSE events signal daemon state changes — use the stale-aware
	// helpers so a skipped fetch gets retried after the in-flight one.
	if cmd := m.requestFetchStatus(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.tasksWorkflowEnabled() && (m.currentView == viewTasks || m.hasActiveFixJobs()) {
		if cmd := m.requestFetchFixJobs(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

// consumeSSEPendingRefresh returns the full SSE refresh command set if
// an event arrived while a fetch was in flight, then clears the flag.
// Returns nil if no refresh is pending.
func (m *model) consumeSSEPendingRefresh() tea.Cmd {
	if !m.ssePendingRefresh {
		return nil
	}
	m.ssePendingRefresh = false
	m.loadingJobs = true
	cmds := []tea.Cmd{m.fetchJobs()}
	if cmd := m.requestFetchStatus(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.tasksWorkflowEnabled() && (m.currentView == viewTasks || m.hasActiveFixJobs()) {
		if cmd := m.requestFetchFixJobs(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// handleReconnectMsg processes daemon reconnection attempts.
func (m model) handleReconnectMsg(msg reconnectMsg) (tea.Model, tea.Cmd) {
	m.reconnecting = false
	if msg.err != nil {
		return m, nil
	}

	if msg.endpoint != m.endpoint {
		m.endpoint = msg.endpoint
		m.client = msg.endpoint.HTTPClient(10 * time.Second)
		m.api = newDaemonAPI(msg.endpoint, m.client)
		// Update runtime metadata so external tools see the
		// new daemon address after reconnect.
		if m.controlSocket != "" {
			rtInfo := buildTUIRuntimeInfo(
				m.controlSocket, m.endpoint.ConfigAddr(),
			)
			if err := WriteTUIRuntime(rtInfo); err != nil {
				log.Printf(
					"warning: failed to update runtime info: %v",
					err,
				)
			}
		}
	}

	// Restart SSE subscription on any successful reconnect, not just
	// endpoint changes. The old goroutine may be stuck in backoff
	// after a same-address daemon restart.
	if m.sseStop != nil {
		close(m.sseStop)
		m.sseCh = make(chan struct{}, 1)
		m.sseStop = make(chan struct{})
		go startSSESubscription(m.endpoint, m.sseCh, m.sseStop)
	}

	m.consecutiveErrors = 0
	m.err = nil
	if msg.version != "" {
		m.daemonVersion = msg.version
	}
	m.clearFetchFailed()
	m.fetchGen++ // invalidate pre-reconnect status/fix-jobs responses
	m.fetchSeq++ // invalidate pre-reconnect jobs responses
	m.loadingJobs = true
	m.loadingMore = false
	cmds := []tea.Cmd{m.fetchJobs(), m.fetchRepoNames()}
	// Force fetches on reconnect — previous in-flight requests
	// were against the old connection and will fail or be stale.
	m.loadingStatus = true
	m.statusStale = false
	cmds = append(cmds, m.fetchStatus())
	m.loadingFixJobs = false
	m.fixJobsStale = false
	if m.tasksWorkflowEnabled() {
		m.loadingFixJobs = true
		cmds = append(cmds, m.fetchFixJobs())
	}
	if cmd := m.fetchUnloadedBranches(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.sseCh != nil {
		cmds = append(cmds, waitForSSE(m.sseCh, m.sseStop))
	}
	return m, tea.Batch(cmds...)
}

// handleWindowSizeMsg processes terminal resize events.
func (m model) handleWindowSizeMsg(
	msg tea.WindowSizeMsg,
) (tea.Model, tea.Cmd) {
	m.width = msg.Width
	m.height = msg.Height
	m.heightDetected = true

	// If terminal can show more jobs than we have, re-fetch to fill
	if !m.loadingMore && !m.loadingJobs &&
		len(m.jobs) > 0 && m.hasMore &&
		m.activeBranchFilter != branchNone {
		newVisibleRows := m.queueVisibleRows() + queuePrefetchBuffer
		if newVisibleRows > len(m.jobs) {
			m.loadingJobs = true
			return m, m.fetchJobs()
		}
	}

	// Width change in log view requires full re-render
	if m.currentView == viewLog && m.logLines != nil {
		m.logOffset = 0
		m.logLines = nil
		m.logFmtr = streamfmt.NewWithWidth(
			io.Discard, msg.Width, m.glamourStyle,
		)
		m.logFetchSeq++
		m.logLoading = true
		return m, m.fetchJobLog(m.logJobID)
	}

	return m, nil
}

// handleTickMsg processes periodic tick events for adaptive polling.
func (m model) handleTickMsg(
	_ tickMsg,
) (tea.Model, tea.Cmd) {
	// Skip job refresh while pagination or another refresh is in flight
	if m.loadingMore || m.loadingJobs {
		cmds := []tea.Cmd{m.tick()}
		if cmd := m.startFetchStatus(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	}
	cmds := []tea.Cmd{m.tick(), m.fetchJobs()}
	if cmd := m.startFetchStatus(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.tasksWorkflowEnabled() && (m.currentView == viewTasks || m.hasActiveFixJobs()) {
		if cmd := m.startFetchFixJobs(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

// handleLogTickMsg processes log stream polling ticks.
func (m model) handleLogTickMsg(
	_ logTickMsg,
) (tea.Model, tea.Cmd) {
	if m.currentView == viewLog && m.logStreaming &&
		m.logJobID > 0 && !m.logLoading {
		m.logLoading = true
		return m, m.fetchJobLog(m.logJobID)
	}
	return m, nil
}

// handleUpdateCheckMsg processes version update check results.
func (m model) handleUpdateCheckMsg(
	msg updateCheckMsg,
) (tea.Model, tea.Cmd) {
	m.updateAvailable = msg.version
	m.updateIsDevBuild = msg.isDevBuild
	return m, nil
}

// handleJobsErrMsg processes job fetch errors.
func (m model) handleJobsErrMsg(
	msg jobsErrMsg,
) (tea.Model, tea.Cmd) {
	if msg.seq < m.fetchSeq {
		return m, nil
	}
	m.err = msg.err
	m.loadingJobs = false
	if cmd := m.handleConnectionError(msg.err); cmd != nil {
		return m, cmd
	}
	return m, m.consumeSSEPendingRefresh()
}

// handlePaginationErrMsg processes pagination fetch errors.
func (m model) handlePaginationErrMsg(
	msg paginationErrMsg,
) (tea.Model, tea.Cmd) {
	if msg.seq < m.fetchSeq {
		m.loadingMore = false
		m.paginateNav = 0
		return m, nil
	}
	m.err = msg.err
	m.loadingMore = false
	m.paginateNav = 0
	if cmd := m.handleConnectionError(msg.err); cmd != nil {
		return m, cmd
	}
	return m, m.consumeSSEPendingRefresh()
}

// handleErrMsg processes generic error messages.
func (m model) handleStatusErrMsg(
	msg statusErrMsg,
) (tea.Model, tea.Cmd) {
	if msg.gen < m.fetchGen {
		return m, nil // discard pre-reconnect error
	}
	m.loadingStatus = false
	m.err = msg.err
	if m.statusStale {
		m.statusStale = false
		m.loadingStatus = true
		return m, m.fetchStatus()
	}
	if cmd := m.handleConnectionError(msg.err); cmd != nil {
		return m, cmd
	}
	return m, nil
}

func (m model) handleErrMsg(
	msg errMsg,
) (tea.Model, tea.Cmd) {
	m.err = msg
	if cmd := m.handleConnectionError(msg); cmd != nil {
		return m, cmd
	}
	return m, nil
}

// handleFixTriggerResultMsg processes fix job trigger results.
func (m model) handleFixTriggerResultMsg(
	msg fixTriggerResultMsg,
) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
		m.setFlash(fmt.Sprintf(
			"Fix failed: %v", msg.err,
		), 3*time.Second, viewTasks)
	} else if msg.warning != "" {
		m.setFlash(msg.warning, 5*time.Second, viewTasks)
		return m, m.requestFetchFixJobs()
	} else {
		m.setFlash(fmt.Sprintf(
			"Fix job #%d enqueued", msg.job.ID,
		), 3*time.Second, viewTasks)
		return m, m.requestFetchFixJobs()
	}
	return m, nil
}

// handlePatchResultMsg processes patch fetch results.
func (m model) handlePatchResultMsg(
	msg patchMsg,
) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.setFlash(fmt.Sprintf(
			"Patch fetch failed: %v", msg.err,
		), 3*time.Second, viewTasks)
	} else {
		m.patchText = msg.patch
		m.patchJobID = msg.jobID
		m.patchScroll = 0
		m.currentView = viewPatch
	}
	return m, nil
}

// handleApplyPatchResultMsg processes patch application results.
func (m model) handleApplyPatchResultMsg(
	msg applyPatchResultMsg,
) (tea.Model, tea.Cmd) {
	if msg.needWorktree {
		m.worktreeConfirmJobID = msg.jobID
		m.worktreeConfirmBranch = msg.branch
		m.currentView = viewKindWorktreeConfirm
		return m, nil
	}
	if msg.rebase {
		m.setFlash(fmt.Sprintf(
			"Patch for job #%d doesn't apply cleanly"+
				" - triggering rebase", msg.jobID,
		), 5*time.Second, viewTasks)
		cmds := []tea.Cmd{m.triggerRebase(msg.jobID)}
		if cmd := m.requestFetchFixJobs(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	} else if msg.commitFailed {
		detail := fmt.Sprintf(
			"Job #%d: %v", msg.jobID, msg.err,
		)
		if msg.worktreeDir != "" {
			detail += fmt.Sprintf(
				" (worktree kept at %s)", msg.worktreeDir,
			)
		}
		m.setFlash(detail, 8*time.Second, viewTasks)
	} else if msg.err != nil {
		m.setFlash(fmt.Sprintf(
			"Apply failed: %v", msg.err,
		), 3*time.Second, viewTasks)
	} else {
		m.setFlash(fmt.Sprintf(
			"Patch from job #%d applied and committed",
			msg.jobID,
		), 3*time.Second, viewTasks)
		cmds := []tea.Cmd{}
		if cmd := m.requestFetchFixJobs(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if msg.parentJobID > 0 {
			cmds = append(
				cmds,
				m.markParentClosed(msg.parentJobID),
			)
		}
		return m, tea.Batch(cmds...)
	}
	return m, nil
}
