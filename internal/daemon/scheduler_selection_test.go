package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestScheduledSelectionUsesOldestBaselineAndDistinctFileBudget(t *testing.T) {
	now := time.Now()
	candidates := []scheduledCandidate{
		{path: "b.go", typ: "complexity", when: now.Add(-time.Hour)},
		{path: "a.go", typ: "refactor", when: now.Add(-time.Hour)},
		{path: "a.go", typ: "complexity", when: now.Add(-2 * time.Hour)},
	}
	sortScheduledCandidates(candidates)
	got := selectScheduledCandidates(candidates, 2)
	assert.Equal(t, []string{"a.go", "a.go", "b.go"}, []string{got[0].path, got[1].path, got[2].path})
	assert.Equal(t, []string{"complexity", "refactor", "complexity"}, []string{got[0].typ, got[1].typ, got[2].typ})
}
