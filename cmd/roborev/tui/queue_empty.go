package tui

func (m model) showFirstReview() bool {
	if m.loadingJobs || m.loadingMore || m.err != nil || len(m.jobs) > 0 || m.lockedBranchFilter {
		return false
	}
	if (len(m.activeRepoFilter) > 0 && !m.autoRepoFilter) || (m.hideClosed && m.jobStats.Closed > 0) {
		return false
	}
	return m.activeBranchFilter == "" || m.activeBranchFilter == m.cwdBranch
}
