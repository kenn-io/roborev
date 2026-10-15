package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Reuse the large-prompt history: queue counts must not walk payload overflow
// pages just to read status, panel role, source, and closed state.
func BenchmarkJobQueue(b *testing.B) {
	db, _ := openAnalyticsBenchmarkDB(b)
	opts := []ListJobsOption{WithoutPrompt(), WithHideClassifyJobs(), WithExcludePanelRole(PanelRoleMember)}
	b.Run("ListFirstPage", func(b *testing.B) {
		for b.Loop() {
			jobs, err := db.ListJobs("", "", 51, 0, opts...)
			require.NoError(b, err)
			require.Len(b, jobs, 51)
		}
	})
	b.Run("CountStats", func(b *testing.B) {
		for b.Loop() {
			stats, err := db.CountJobStats("", "", opts...)
			require.NoError(b, err)
			require.Equal(b, 3000, stats.Done)
		}
	})
}
