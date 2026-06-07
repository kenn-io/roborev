package tui

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func moveToFront[T any](items []T, match func(T) bool) {
	for i := 1; i < len(items); i++ {
		if match(items[i]) {
			m := items[i]
			copy(items[1:i+1], items[0:i])
			items[0] = m
			return
		}
	}
}

// rebuildFilterFlatList rebuilds the flat list of visible filter entries from the tree.
// When search is active, auto-expands repos with matching branches and hides non-matching items.
// Clamps filterSelectedIdx after rebuild.
func (m *model) rebuildFilterFlatList() {
	var flat []flatFilterEntry
	search := strings.ToLower(m.filterSearch)

	// Reset search state when not searching
	if search == "" {
		for i := range m.filterTree {
			m.filterTree[i].userCollapsed = false
			m.filterTree[i].fetchFailed = false
		}
	}

	// Always include "All" as first entry
	if search == "" || strings.Contains("all", search) {
		flat = append(flat, flatFilterEntry{repoIdx: -1, branchIdx: -1})
	}

	for i, node := range m.filterTree {
		repoNameMatch := search == "" ||
			strings.Contains(strings.ToLower(node.name), search)
		// Also check repo path basenames
		if !repoNameMatch {
			for _, p := range node.rootPaths {
				if strings.Contains(strings.ToLower(filepath.Base(p)), search) {
					repoNameMatch = true
					break
				}
			}
		}

		// Check if any children match the search
		childMatches := false
		if search != "" && !repoNameMatch {
			for _, child := range node.children {
				if strings.Contains(strings.ToLower(child.name), search) {
					childMatches = true
					break
				}
			}
		}

		if !repoNameMatch && !childMatches {
			continue
		}

		// Add repo node
		flat = append(flat, flatFilterEntry{repoIdx: i, branchIdx: -1})

		// Add children if expanded or if search matched children
		// (user can override search auto-expansion via left-arrow)
		showChildren := node.expanded ||
			(search != "" && childMatches && !node.userCollapsed)
		if showChildren && len(node.children) > 0 {
			for j, child := range node.children {
				if search != "" && !repoNameMatch {
					// Only show matching children when repo didn't match
					if !strings.Contains(strings.ToLower(child.name), search) {
						continue
					}
				}
				flat = append(flat, flatFilterEntry{repoIdx: i, branchIdx: j})
			}
		}
	}

	m.filterFlatList = flat

	// Clamp selection
	if len(flat) == 0 {
		m.filterSelectedIdx = 0
	} else if m.filterSelectedIdx >= len(flat) {
		m.filterSelectedIdx = len(flat) - 1
	}
}

// filterNavigateUp moves selection up in the tree filter
func (m *model) filterNavigateUp() {
	if m.filterSelectedIdx > 0 {
		m.filterSelectedIdx--
	}
}

// filterNavigateDown moves selection down in the tree filter
func (m *model) filterNavigateDown() {
	if m.filterSelectedIdx < len(m.filterFlatList)-1 {
		m.filterSelectedIdx++
	}
}

// getSelectedFilterEntry returns the currently selected flat entry, or nil
func (m *model) getSelectedFilterEntry() *flatFilterEntry {
	if m.filterSelectedIdx >= 0 && m.filterSelectedIdx < len(m.filterFlatList) {
		return &m.filterFlatList[m.filterSelectedIdx]
	}
	return nil
}

// filterTreeTotalCount returns the total job count across all repos in the tree
func (m *model) filterTreeTotalCount() int {
	total := 0
	for _, node := range m.filterTree {
		total += node.count
	}
	return total
}

// rootPathsMatch returns true if two rootPaths slices contain the
// same paths (order-independent). This handles the case where the
// tree is rebuilt with a different path ordering while a branch
// fetch is in-flight.
func rootPathsMatch(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) <= 1 {
		return len(a) == 0 || a[0] == b[0]
	}
	as := make([]string, len(a))
	bs := make([]string, len(b))
	copy(as, a)
	copy(bs, b)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// repoMatchesFilter checks if a repo path matches the active filter.
func (m model) repoMatchesFilter(repoPath string) bool {
	return slices.Contains(m.activeRepoFilter, repoPath)
}

func (m model) displayNameForRootPaths(rootPaths []string) string {
	names := make([]string, 0, len(m.repoNames))
	for name := range m.repoNames {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if rootPathsMatch(m.repoNames[name], rootPaths) {
			return name
		}
	}
	for _, node := range m.filterTree {
		if rootPathsMatch(node.rootPaths, rootPaths) {
			return node.name
		}
	}
	if len(rootPaths) == 1 {
		for _, job := range m.jobs {
			if job.RepoPath == rootPaths[0] && job.RepoName != "" {
				return job.RepoName
			}
		}
	}
	return ""
}

func (m model) repoFilterDisplayName() string {
	if len(m.activeRepoFilter) == 0 {
		return ""
	}
	if name := m.displayNameForRootPaths(m.activeRepoFilter); name != "" {
		return name
	}
	if len(m.activeRepoFilter) == 1 {
		return m.getDisplayName(
			m.activeRepoFilter[0],
			filepath.Base(m.activeRepoFilter[0]),
		)
	}
	return strings.Join(m.activeRepoFilter, ", ")
}

func (m model) repoRootTracked(rootPath string) bool {
	if rootPath == "" {
		return false
	}
	for _, rootPaths := range m.repoNames {
		if slices.Contains(rootPaths, rootPath) {
			return true
		}
	}
	for _, rootPaths := range m.repoIdentities {
		if slices.Contains(rootPaths, rootPath) {
			return true
		}
	}
	for _, node := range m.filterTree {
		if slices.Contains(node.rootPaths, rootPath) {
			return true
		}
	}
	return false
}

func (m *model) reconcileAutoRepoFilter() bool {
	if !m.autoRepoFilter || len(m.activeRepoFilter) != 1 ||
		m.cwdRepoRoot == "" || m.activeRepoFilter[0] != m.cwdRepoRoot {
		return false
	}
	if m.repoRootTracked(m.cwdRepoRoot) {
		return false
	}

	var rootPaths []string
	if m.cwdRepoIdentity != "" {
		rootPaths = m.repoIdentities[m.cwdRepoIdentity]
	}
	if len(rootPaths) == 0 {
		displayName := config.GetDisplayName(m.cwdRepoRoot)
		if displayName == "" {
			displayName = filepath.Base(m.cwdRepoRoot)
		}
		rootPaths = m.repoNames[displayName]
	}
	if len(rootPaths) == 0 || rootPathsMatch(rootPaths, m.activeRepoFilter) {
		return false
	}

	m.activeRepoFilter = copyStrings(rootPaths)
	m.resetQueueForFilterChange()
	m.recomputeClassifyEffective()
	return true
}

func (m *model) resetQueueForFilterChange() {
	m.jobs = nil
	m.hasMore = false
	m.loadingMore = false
	m.paginateNav = 0
	m.selectedIdx = -1
	m.selectedJobID = 0
	m.fetchSeq++
	m.queueColGen++
	m.loadingJobs = true
}

// isJobVisible checks if a job passes all active filters
func (m model) isJobVisible(job storage.ReviewJob) bool {
	if len(m.activeRepoFilter) > 0 && !m.repoMatchesFilter(job.RepoPath) {
		return false
	}
	if m.activeBranchFilter != "" && !m.branchMatchesFilter(job) {
		return false
	}
	if m.hideClosed {
		// Hide closed reviews, failed jobs, and canceled jobs
		// Check pendingClosed first for optimistic updates (avoids flash on filter)
		if pending, ok := m.pendingClosed[job.ID]; ok {
			if pending.newState {
				return false
			}
		} else if job.Closed != nil && *job.Closed {
			return false
		}
		if job.Status == storage.JobStatusFailed || job.Status == storage.JobStatusCanceled {
			return false
		}
	}
	return true
}

// branchMatchesFilter checks if a job's branch matches the active branch filter
func (m model) branchMatchesFilter(job storage.ReviewJob) bool {
	branch := m.getBranchForJob(job)
	if branch == "" {
		branch = branchNone
	}
	return branch == m.activeBranchFilter
}

// pushFilter adds a filter type to the stack (or moves it to the end if already present)
func (m *model) pushFilter(filterType string) {
	// Remove if already present
	m.removeFilterFromStack(filterType)
	// Add to end
	m.filterStack = append(m.filterStack, filterType)
}

// popFilter walks the stack from end to start, removes the first
// unlocked filter, clears its value, and returns the filter type.
// Returns empty string if no unlocked filter exists.
func (m *model) popFilter() string {
	for i, v := range slices.Backward(m.filterStack) {
		ft := v
		if ft == filterTypeRepo && m.lockedRepoFilter {
			continue
		}
		if ft == filterTypeBranch && m.lockedBranchFilter {
			continue
		}
		m.filterStack = append(
			m.filterStack[:i], m.filterStack[i+1:]...,
		)
		switch ft {
		case filterTypeRepo:
			m.activeRepoFilter = nil
			m.autoRepoFilter = false
			m.recomputeClassifyEffective()
		case filterTypeBranch:
			m.activeBranchFilter = ""
		}
		m.queueColGen++
		return ft
	}
	return ""
}

// removeFilterFromStack removes a filter type from the stack without clearing its value
func (m *model) removeFilterFromStack(filterType string) {
	var newStack []string
	for _, f := range m.filterStack {
		if f != filterType {
			newStack = append(newStack, f)
		}
	}
	m.filterStack = newStack
}
