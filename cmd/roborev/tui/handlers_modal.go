package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"go.kenn.io/roborev/internal/storage"
)

// handleCommentKey handles key input in the comment modal.
func (m model) handleCommentKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.currentView = m.commentFromView
		m.commentText = ""
		m.commentJobID = 0
		return m, nil
	case "enter":
		if strings.TrimSpace(m.commentText) != "" {
			text := m.commentText
			jobID := m.commentJobID
			m.currentView = m.commentFromView
			return m, m.submitComment(jobID, text)
		}
		return m, nil
	case "backspace":
		if len(m.commentText) > 0 {
			runes := []rune(m.commentText)
			m.commentText = string(runes[:len(runes)-1])
		}
		return m, nil
	default:
		if msg.String() == "shift+enter" || msg.String() == "ctrl+j" {
			m.commentText += "\n"
		} else if len(keyRunes(msg)) > 0 {
			for _, r := range keyRunes(msg) {
				if unicode.IsPrint(r) || r == '\n' || r == '\t' {
					m.commentText += string(r)
				}
			}
		}
		return m, nil
	}
}

// handleFilterKey handles key input in the unified tree filter modal.
func (m model) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.currentView = viewQueue
		m.filterSearch = ""
		m.filterBranchMode = false
		return m, nil
	case "up":
		m.filterNavigateUp()
		return m, nil
	case "down":
		m.filterNavigateDown()
		return m, nil
	case "right":
		// Expand a collapsed repo node, or retry a failed load
		entry := m.getSelectedFilterEntry()
		if entry != nil && entry.repoIdx >= 0 && entry.branchIdx == -1 {
			node := &m.filterTree[entry.repoIdx]
			needsFetch := node.children == nil && !node.loading
			if !node.expanded || needsFetch {
				node.userCollapsed = false
				if len(node.children) > 0 {
					node.expanded = true
					m.rebuildFilterFlatList()
				} else if !node.loading {
					node.expanded = true
					node.loading = true
					node.fetchFailed = false
					return m, m.fetchBranchesForRepo(
						node.rootPaths, entry.repoIdx, true, m.filterSearchSeq,
					)
				} else {
					// Load in-flight (search-triggered); mark
					// expanded so children show on arrival.
					node.expanded = true
				}
			}
		}
		return m, nil
	case "left":
		// Collapse an expanded repo, or if on a branch, collapse parent
		entry := m.getSelectedFilterEntry()
		if entry != nil {
			collapse := func(idx int) {
				m.filterTree[idx].expanded = false
				if m.filterSearch != "" {
					m.filterTree[idx].userCollapsed = true
				}
				m.rebuildFilterFlatList()
			}
			if entry.branchIdx >= 0 {
				// On a branch: collapse parent and move selection to parent
				collapse(entry.repoIdx)
				for i, e := range m.filterFlatList {
					if e.repoIdx == entry.repoIdx && e.branchIdx == -1 {
						m.filterSelectedIdx = i
						break
					}
				}
			} else if entry.repoIdx >= 0 {
				node := &m.filterTree[entry.repoIdx]
				if node.expanded ||
					(m.filterSearch != "" && !node.userCollapsed) {
					collapse(entry.repoIdx)
				}
			}
		}
		return m, nil
	case "enter":
		entry := m.getSelectedFilterEntry()
		if entry == nil {
			return m, nil
		}
		if entry.repoIdx == -1 {
			// "All" -- clear unlocked filters only
			if !m.lockedRepoFilter {
				m.activeRepoFilter = nil
				m.autoRepoFilter = false
				m.removeFilterFromStack(filterTypeRepo)
			}
			if !m.lockedBranchFilter {
				m.activeBranchFilter = ""
				m.removeFilterFromStack(filterTypeBranch)
			}
		} else if entry.branchIdx == -1 {
			// Repo node -- filter by repo only
			node := m.filterTree[entry.repoIdx]
			if !m.lockedRepoFilter {
				m.activeRepoFilter = node.rootPaths
				m.autoRepoFilter = false
				m.pushFilter(filterTypeRepo)
			}
			if !m.lockedBranchFilter {
				m.activeBranchFilter = ""
				m.removeFilterFromStack(filterTypeBranch)
			}
		} else {
			// Branch node -- filter by repo + branch
			node := m.filterTree[entry.repoIdx]
			branch := node.children[entry.branchIdx]
			if !m.lockedRepoFilter {
				m.activeRepoFilter = node.rootPaths
				m.autoRepoFilter = false
				m.pushFilter(filterTypeRepo)
			}
			if !m.lockedBranchFilter {
				m.activeBranchFilter = branch.name
				m.pushFilter(filterTypeBranch)
			}
		}
		m.currentView = viewQueue
		m.filterSearch = ""
		m.filterBranchMode = false
		m.resetQueueForFilterChange()
		// activeRepoFilter may have changed; refresh the cached
		// classify visibility so the next render/fetch sees the
		// repo-scoped value without hitting disk on the hot path.
		m.recomputeClassifyEffective()
		return m, m.fetchJobs()
	case "backspace":
		if len(m.filterSearch) > 0 {
			runes := []rune(m.filterSearch)
			m.filterSearch = string(runes[:len(runes)-1])
			m.filterSearchSeq++
			m.clearFetchFailed()
			m.filterSelectedIdx = 0
			m.rebuildFilterFlatList()
		}
		return m, m.fetchUnloadedBranches()
	default:
		if len(keyRunes(msg)) > 0 {
			for _, r := range keyRunes(msg) {
				if unicode.IsPrint(r) && !unicode.IsControl(r) {
					m.filterSearch += string(r)
					m.filterSelectedIdx = 0
				}
			}
			m.filterSearchSeq++
			m.clearFetchFailed()
			m.rebuildFilterFlatList()
		}
		return m, m.fetchUnloadedBranches()
	}
}

// clearFetchFailed resets fetchFailed on all tree nodes so that
// changed search text retries previously failed repos.
func (m *model) clearFetchFailed() {
	for i := range m.filterTree {
		m.filterTree[i].fetchFailed = false
	}
}

// maxSearchBranchFetches is the maximum number of concurrent
// search-triggered branch fetches. Counts both already in-flight
// and newly started requests.
const maxSearchBranchFetches = 5

// fetchUnloadedBranches triggers branch fetches for repos that
// haven't loaded branches yet while search text is active. Without
// this, searching for a branch name only matches already-expanded
// repos. At most maxSearchBranchFetches total requests are allowed
// in-flight at once; completions trigger top-up fetches via the
// repoBranchesMsg handler.
func (m *model) fetchUnloadedBranches() tea.Cmd {
	if m.filterSearch == "" {
		return nil
	}
	inFlight := 0
	for i := range m.filterTree {
		if m.filterTree[i].loading {
			inFlight++
		}
	}
	slots := maxSearchBranchFetches - inFlight
	if slots <= 0 {
		return nil
	}
	var cmds []tea.Cmd
	for i := range m.filterTree {
		node := &m.filterTree[i]
		if node.children == nil && !node.loading && !node.fetchFailed {
			node.loading = true
			cmds = append(cmds, m.fetchBranchesForRepo(
				node.rootPaths, i, false, m.filterSearchSeq,
			))
			if len(cmds) >= slots {
				break
			}
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// handleLogKey handles key input in the log view.
func (m model) handleLogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "ctrl+d", "esc", "q":
		m.currentView = m.logFromView
		m.logStreaming = false
		return m, nil
	case "x":
		if m.logJobID > 0 && m.logStreaming {
			if job := m.logViewLookupJob(); job != nil &&
				job.Status == storage.JobStatusRunning {
				oldStatus := job.Status
				oldFinishedAt := job.FinishedAt
				job.Status = storage.JobStatusCanceled
				now := time.Now()
				job.FinishedAt = &now
				m.logStreaming = false
				return m, m.cancelJob(
					job.ID, oldStatus, oldFinishedAt,
					false,
				)
			}
		}
		return m, nil
	case "up", "k":
		m.logFollow = false
		if m.logScroll > 0 {
			m.logScroll--
		}
		return m, nil
	case "down", "j":
		m.logScroll++
		return m, nil
	case "pgup":
		m.logFollow = false
		m.logScroll -= m.logVisibleLines()
		if m.logScroll < 0 {
			m.logScroll = 0
		}
		return m, tea.ClearScreen
	case "pgdown":
		m.logScroll += m.logVisibleLines()
		return m, tea.ClearScreen
	case "home":
		m.logFollow = false
		m.logScroll = 0
		return m, nil
	case "end":
		m.logFollow = true
		maxScroll := max(len(m.logLines)-m.logVisibleLines(), 0)
		m.logScroll = maxScroll
		return m, nil
	case "g", "G":
		maxScroll := max(len(m.logLines)-m.logVisibleLines(), 0)
		if m.logScroll == 0 {
			m.logFollow = true
			m.logScroll = maxScroll
		} else {
			m.logFollow = false
			m.logScroll = 0
		}
		return m, tea.ClearScreen
	case "left":
		return m.handlePrevKey()
	case "right":
		return m.handleNextKey()
	case "i":
		m.cmdExpanded = !m.cmdExpanded
		return m, tea.ClearScreen
	case "?":
		m.helpFromView = m.currentView
		m.currentView = viewHelp
		m.helpScroll = 0
		return m, nil
	}
	return m, nil
}

// handleWorktreeConfirmKey handles key input in the worktree creation confirmation modal.
func (m model) handleWorktreeConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "ctrl+d", "esc", "n":
		m.currentView = viewTasks
		m.worktreeConfirmJobID = 0
		m.worktreeConfirmBranch = ""
		return m, nil
	case "enter", "y":
		jobID := m.worktreeConfirmJobID
		m.currentView = viewTasks
		m.worktreeConfirmJobID = 0
		m.worktreeConfirmBranch = ""
		return m, m.applyFixPatchInWorktree(jobID)
	}
	return m, nil
}

// handleTasksKey handles key input in the tasks view.
func (m model) handleTasksKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if isSubmitKey(msg) {
		// View task: prompt for running, review for done/applied, log for failed
		if len(m.fixJobs) > 0 && m.fixSelectedIdx < len(m.fixJobs) {
			job := m.fixJobs[m.fixSelectedIdx]
			switch {
			case job.Status == storage.JobStatusRunning:
				if job.Prompt != "" {
					m.currentReview = &storage.Review{
						Agent:  job.Agent,
						Prompt: job.Prompt,
						Job:    &job,
					}
					m.reviewFromView = viewTasks
					m.currentView = viewKindPrompt
					m.promptScroll = 0
					m.promptFromQueue = false
					return m, nil
				}
				// No prompt yet, go straight to log view
				return m.openLogView(job.ID, job.Status, viewTasks)
			case job.HasViewableOutput():
				m.selectedJobID = job.ID
				m.reviewFromView = viewTasks
				return m, m.fetchReview(job.ID)
			case job.Status == storage.JobStatusFailed:
				return m.openLogView(job.ID, job.Status, viewTasks)
			}
		}
		return m, nil
	}

	switch msg.String() {
	case "ctrl+c", "ctrl+d":
		return m, tea.Quit
	case "q":
		if m.noQuit {
			return m, nil
		}
		return m, tea.Quit
	case "esc", "T":
		m.currentView = viewQueue
		return m, nil
	case "o":
		return m.handleColumnOptionsKey()
	case "up", "k":
		if m.fixSelectedIdx > 0 {
			m.fixSelectedIdx--
		}
		return m, nil
	case "down", "j":
		if m.fixSelectedIdx < len(m.fixJobs)-1 {
			m.fixSelectedIdx++
		}
		return m, nil
	case "l", "t":
		// View agent log for any non-queued job
		if len(m.fixJobs) > 0 && m.fixSelectedIdx < len(m.fixJobs) {
			job := m.fixJobs[m.fixSelectedIdx]
			if job.Status == storage.JobStatusQueued {
				m.setFlash("Job is queued - not yet running", 2*time.Second, viewTasks)
				return m, nil
			}
			return m.openLogView(job.ID, job.Status, viewTasks)
		}
		return m, nil
	case "A":
		// Apply patch (handled in Phase 5)
		if len(m.fixJobs) > 0 && m.fixSelectedIdx < len(m.fixJobs) {
			job := m.fixJobs[m.fixSelectedIdx]
			if job.Status == storage.JobStatusDone {
				return m, m.applyFixPatch(job.ID)
			}
		}
		return m, nil
	case "R":
		// Manually trigger rebase for a completed or rebased fix job
		if len(m.fixJobs) > 0 && m.fixSelectedIdx < len(m.fixJobs) {
			job := m.fixJobs[m.fixSelectedIdx]
			if job.Status == storage.JobStatusDone || job.Status == storage.JobStatusRebased {
				return m, m.triggerRebase(job.ID)
			}
		}
		return m, nil
	case "x":
		// Cancel fix job
		if len(m.fixJobs) > 0 && m.fixSelectedIdx < len(m.fixJobs) {
			job := &m.fixJobs[m.fixSelectedIdx]
			if job.Status == storage.JobStatusRunning || job.Status == storage.JobStatusQueued {
				oldStatus := job.Status
				oldFinishedAt := job.FinishedAt
				job.Status = storage.JobStatusCanceled
				now := time.Now()
				job.FinishedAt = &now
				return m, m.cancelJob(
					job.ID, oldStatus, oldFinishedAt, false,
				)
			}
		}
		return m, nil
	case "p":
		// View patch for completed fix jobs
		if len(m.fixJobs) > 0 && m.fixSelectedIdx < len(m.fixJobs) {
			job := m.fixJobs[m.fixSelectedIdx]
			if job.HasViewableOutput() {
				return m, m.fetchPatch(job.ID)
			}
			m.setFlash("Patch not yet available", 2*time.Second, m.currentView)
		}
		return m, nil
	case "P":
		// Open parent review for this fix task
		if len(m.fixJobs) > 0 && m.fixSelectedIdx < len(m.fixJobs) {
			job := m.fixJobs[m.fixSelectedIdx]
			if job.ParentJobID == nil || *job.ParentJobID == 0 {
				m.setFlash("No parent review for this task", 2*time.Second, viewTasks)
				return m, nil
			}
			m.selectedJobID = *job.ParentJobID
			m.reviewFromView = viewTasks
			return m, m.fetchReview(*job.ParentJobID)
		}
		return m, nil
	case "?":
		m.fixShowHelp = !m.fixShowHelp
		return m, nil
	}
	return m, nil
}

// handlePatchKey handles key input in the patch viewer.
func (m model) handlePatchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// When the save-filename input is active, route keys there.
	if m.savePatchInputActive {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.savePatchInputActive = false
			m.savePatchInput = ""
			return m, nil
		case "enter":
			path := strings.TrimSpace(m.savePatchInput)
			if path == "" {
				return m, nil
			}
			m.savePatchInputActive = false
			m.savePatchInput = ""
			return m, m.savePatchToFile(path)
		case "backspace":
			if len(m.savePatchInput) > 0 {
				runes := []rune(m.savePatchInput)
				m.savePatchInput = string(runes[:len(runes)-1])
			}
			return m, nil
		default:
			for _, r := range keyRunes(msg) {
				if unicode.IsPrint(r) {
					m.savePatchInput += string(r)
				}
			}
			return m, nil
		}
	}

	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "ctrl+d", "esc", "q":
		m.currentView = viewTasks
		m.patchText = ""
		m.patchScroll = 0
		m.patchJobID = 0
		return m, nil
	case "s":
		m.savePatchInputActive = true
		m.savePatchInput = filepath.Join(os.TempDir(), fmt.Sprintf("roborev-%d.patch", m.patchJobID))
		return m, nil
	case "up", "k":
		if m.patchScroll > 0 {
			m.patchScroll--
		}
		return m, nil
	case "down", "j":
		m.patchScroll++
		return m, nil
	case "pgup":
		visibleLines := max(m.height-4, 1)
		m.patchScroll = max(0, m.patchScroll-visibleLines)
		return m, tea.ClearScreen
	case "pgdown":
		visibleLines := max(m.height-4, 1)
		m.patchScroll += visibleLines
		return m, tea.ClearScreen
	case "home", "g":
		m.patchScroll = 0
		return m, nil
	case "end", "G":
		lines := strings.Split(m.patchText, "\n")
		visibleRows := max(m.height-4, 1)
		m.patchScroll = max(len(lines)-visibleRows, 0)
		return m, nil
	}
	return m, nil
}

// savePatchToFile writes the current patch text to path.
func (m model) savePatchToFile(path string) tea.Cmd {
	patch := m.patchText
	return func() tea.Msg {
		if err := os.WriteFile(path, []byte(patch), 0o644); err != nil {
			return savePatchResultMsg{err: err}
		}
		return savePatchResultMsg{path: path}
	}
}
