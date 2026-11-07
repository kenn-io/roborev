package tui

func (m model) showFirstReview() bool {
	if m.loadingJobs || m.loadingMore || m.err != nil || len(m.jobs) > 0 || m.hasMore {
		return false
	}
	if m.lockedRepoFilter || m.lockedBranchFilter || (m.hideClosed && m.jobStats.Closed > 0) {
		return false
	}
	if len(m.activeRepoFilter) > 0 && (!m.autoRepoFilter || len(m.activeRepoFilter) != 1 || m.activeRepoFilter[0] != m.cwdRepoRoot) {
		return false
	}
	return m.activeBranchFilter == "" || m.activeBranchFilter == m.cwdBranch
}
