package tui

func (m model) showFirstReview() bool {
	if m.loadingJobs || m.loadingMore || m.jobsErr != nil || len(m.jobs) > 0 || m.lockedBranchFilter {
		return false
	}
	if (len(m.activeRepoFilter) > 0 && !m.autoRepoFilter) || (m.hideClosed && m.jobStats.Closed+m.jobStats.Failed+m.jobStats.Canceled > 0) {
		return false
	}
	return m.activeBranchFilter == "" || m.activeBranchFilter == m.cwdBranch
}
