package searchindex

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeGroupRRFFusesDifferentPanelMembersOnce(t *testing.T) {
	lexicalMember := rrfCandidate("lexical-member", "panel-1", 10, 1)
	lexicalMember.MatchedIn = []string{MatchLexical}
	lexicalMember.Excerpt = "lexical excerpt"
	semanticMember := rrfCandidate("semantic-member", "panel-1", 20, 1)
	semanticMember.MatchedIn = []string{MatchSemantic}
	semanticMember.Excerpt = "semantic excerpt"

	merged := mergeGroupRRF([]rankedCandidate{lexicalMember}, []rankedCandidate{semanticMember}, 10)
	require.Len(t, merged, 1)
	assert.Equal(t, lexicalMember.DocKey, merged[0].DocKey, "equal ranks prefer the lexical member")
	assert.Equal(t, lexicalMember.JobID, merged[0].JobID)
	assert.Equal(t, "lexical excerpt", merged[0].Excerpt)
	assert.Equal(t, []string{MatchLexical, MatchSemantic}, merged[0].MatchedIn)
	assert.InDelta(t, 2.0/61.0, merged[0].Score, 1e-12)
}

func TestMergeGroupRRFKeepsOneContributionPerLegAndUsesBetterLegRank(t *testing.T) {
	lexicalFirst := rrfCandidate("other-lexical", "other-lexical-group", 1, 1)
	lexicalPanel := rrfCandidate("lexical-panel", "panel", 2, 2)
	duplicatePanel := rrfCandidate("lexical-panel-duplicate", "panel", 3, 3)
	semanticPanel := rrfCandidate("semantic-panel", "panel", 4, 1)
	semanticPanel.Excerpt = "semantic winner"

	merged := mergeGroupRRF(
		[]rankedCandidate{lexicalFirst, lexicalPanel, duplicatePanel},
		[]rankedCandidate{semanticPanel},
		10,
	)
	require.Len(t, merged, 2)
	panel := candidateForGroup(t, merged, "panel")
	assert.Equal(t, semanticPanel.DocKey, panel.DocKey)
	assert.Equal(t, "semantic winner", panel.Excerpt)
	assert.InDelta(t, 1.0/62.0+1.0/61.0, panel.Score, 1e-12,
		"the duplicate lexical panel member must not add a second lexical contribution")
}

func TestMergeGroupRRFBreaksFinalTiesByRecencyJobAndDocumentKey(t *testing.T) {
	when := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	older := rrfCandidate("older", "older-group", 10, 1)
	older.FinishedAt = when.Add(-time.Hour)
	newerLowJob := rrfCandidate("newer-low-job", "newer-low-job-group", 10, 1)
	newerLowJob.FinishedAt = when
	newerLowJob.JobID = 40
	newerHighJobZ := rrfCandidate("z-doc", "newer-high-z-group", 10, 1)
	newerHighJobZ.FinishedAt = when
	newerHighJobZ.JobID = 50
	newerHighJobA := rrfCandidate("a-doc", "newer-high-a-group", 10, 1)
	newerHighJobA.FinishedAt = when
	newerHighJobA.JobID = 50

	merged := mergeGroupRRF([]rankedCandidate{older, newerLowJob, newerHighJobZ, newerHighJobA}, nil, 10)
	require.Len(t, merged, 4)
	assert.Equal(t, []string{"a-doc", "z-doc", "newer-low-job", "older"}, candidateDocKeys(merged))
}

func TestGroupLegCandidatesUsesRecencyJobAndKeyForEqualScores(t *testing.T) {
	when := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	candidates := []rankedCandidate{
		rrfCandidate("old", "panel", 1, 1),
		rrfCandidate("z-key", "panel", 2, 1),
		rrfCandidate("a-key", "panel", 2, 1),
	}
	candidates[0].FinishedAt = when.Add(-time.Hour)
	candidates[1].FinishedAt = when
	candidates[2].FinishedAt = when
	candidates[1].JobID = 100
	candidates[2].JobID = 100

	grouped := groupLegCandidates(candidates)
	require.Len(t, grouped, 1)
	assert.Equal(t, "a-key", grouped[0].DocKey)
}

func TestMergeGroupRRFUsesStableMatchedInOrderIncludingIdentifiers(t *testing.T) {
	lexical := rrfCandidate("lexical", "panel", 1, 1)
	lexical.MatchedIn = []string{MatchLexical, MatchIdentifier, MatchLexical}
	semantic := rrfCandidate("semantic", "panel", 2, 1)
	semantic.MatchedIn = []string{MatchSemantic}

	merged := mergeGroupRRF([]rankedCandidate{lexical}, []rankedCandidate{semantic}, 1)
	require.Len(t, merged, 1)
	assert.Equal(t, []string{MatchIdentifier, MatchLexical, MatchSemantic}, merged[0].MatchedIn)
}

func rrfCandidate(docKey, groupKey string, id int64, rank int) rankedCandidate {
	return rankedCandidate{
		DocKey: docKey, GroupKey: groupKey, ReviewID: id, JobID: id,
		FinishedAt: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC),
		Rank:       rank, Score: 1, MatchedIn: []string{MatchLexical},
	}
}

func candidateForGroup(t *testing.T, candidates []rankedCandidate, group string) rankedCandidate {
	t.Helper()
	for _, candidate := range candidates {
		if candidate.GroupKey == group {
			return candidate
		}
	}
	require.NotEmpty(t, candidates)
	return rankedCandidate{}
}

func candidateDocKeys(candidates []rankedCandidate) []string {
	keys := make([]string, len(candidates))
	for i, candidate := range candidates {
		keys[i] = candidate.DocKey
	}
	return keys
}
