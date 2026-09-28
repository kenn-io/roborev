package searchindex

import (
	"fmt"
	"slices"

	"go.kenn.io/kit/search/rrf"
)

const reciprocalRankConstant = 60

func rankLegCandidatesPreservingGroups(
	candidates []rankedCandidate, compare func(rankedCandidate, rankedCandidate) int,
) []rankedCandidate {
	best := make(map[string]rankedCandidate, len(candidates))
	for _, candidate := range candidates {
		current, found := best[candidate.GroupKey]
		if !found || compare(candidate, current) < 0 {
			best[candidate.GroupKey] = candidate
		}
	}
	groups := make([]rankedCandidate, 0, len(best))
	for _, candidate := range best {
		groups = append(groups, candidate)
	}
	slices.SortFunc(groups, compare)
	ranks := make(map[string]int, len(groups))
	for i, candidate := range groups {
		ranks[candidate.GroupKey] = i + 1
	}
	for i := range candidates {
		candidates[i].Rank = ranks[candidates[i].GroupKey]
	}
	slices.SortFunc(candidates, func(left, right rankedCandidate) int {
		if left.Rank != right.Rank {
			return left.Rank - right.Rank
		}
		return compare(left, right)
	})
	return candidates
}

func groupLegCandidates(candidates []rankedCandidate) []rankedCandidate {
	if len(candidates) == 0 {
		return nil
	}
	best := make(map[string]rankedCandidate, len(candidates))
	for _, candidate := range candidates {
		current, found := best[candidate.GroupKey]
		if !found || betterWithinLeg(candidate, current) {
			best[candidate.GroupKey] = candidate
		}
	}
	grouped := make([]rankedCandidate, 0, len(best))
	for _, candidate := range best {
		grouped = append(grouped, candidate)
	}
	slices.SortFunc(grouped, compareLegCandidates)
	for i := range grouped {
		grouped[i].Rank = i + 1
	}
	return grouped
}

func betterWithinLeg(candidate, current rankedCandidate) bool {
	if candidate.Rank > 0 && current.Rank > 0 && candidate.Rank != current.Rank {
		return candidate.Rank < current.Rank
	}
	if candidate.identifier != current.identifier {
		return candidate.identifier
	}
	if candidate.Score != current.Score {
		return candidate.Score > current.Score
	}
	return compareCandidateTie(candidate, current) < 0
}

func compareLegCandidates(left, right rankedCandidate) int {
	if left.Rank > 0 && right.Rank > 0 && left.Rank != right.Rank {
		if left.Rank < right.Rank {
			return -1
		}
		return 1
	}
	if left.Score != right.Score {
		if left.Score > right.Score {
			return -1
		}
		return 1
	}
	return compareCandidateTie(left, right)
}

const (
	legLexical  = "lexical"
	legSemantic = "semantic"
)

// mergeGroupRRF fuses already-hydrated legs. Each leg ranks a panel group
// once, the group's representative is the member from its best-ranked leg
// (lexical on a tie), and equal scores keep the canonical tie order.
func mergeGroupRRF(lexical, semantic []rankedCandidate, limit int) ([]rankedCandidate, error) {
	lexical = groupLegCandidates(lexical)
	semantic = groupLegCandidates(semantic)
	members := map[string]map[string]rankedCandidate{
		legLexical:  make(map[string]rankedCandidate, len(lexical)),
		legSemantic: make(map[string]rankedCandidate, len(semantic)),
	}
	leg := func(name string, candidates []rankedCandidate) rrf.GroupLeg[string, string] {
		groups := make([]rrf.Group[string, string], len(candidates))
		for i, candidate := range candidates {
			members[name][candidate.DocKey] = candidate
			groups[i] = rrf.Group[string, string]{Key: candidate.GroupKey, Members: []string{candidate.DocKey}}
		}
		return rrf.GroupLeg[string, string]{Name: name, Weight: 1, Groups: groups}
	}
	fused, err := rrf.FuseGroups(reciprocalRankConstant, []rrf.GroupLeg[string, string]{
		leg(legLexical, lexical), leg(legSemantic, semantic),
	})
	if err != nil {
		return nil, fmt.Errorf("fuse search legs: %w", err)
	}

	merged := make([]rankedCandidate, 0, len(fused))
	for _, hit := range fused {
		var representative rankedCandidate
		var matches []string
		for _, alternate := range hit.Alternates {
			candidate := members[alternate.Leg][alternate.Member]
			matches = append(matches, candidate.MatchedIn...)
			if representative.DocKey == "" || candidate.Rank < representative.Rank {
				representative = candidate
			}
		}
		representative.Score = hit.Score
		representative.MatchedIn = stableMatches(matches)
		merged = append(merged, representative)
	}
	slices.SortStableFunc(merged, func(left, right rankedCandidate) int {
		if left.Score != right.Score {
			if left.Score > right.Score {
				return -1
			}
			return 1
		}
		return compareCandidateTie(left, right)
	})
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	for i := range merged {
		merged[i].Rank = i + 1
	}
	return merged, nil
}

func stableMatches(matches []string) []string {
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		seen[match] = struct{}{}
	}
	ordered := make([]string, 0, len(seen))
	for _, match := range []string{MatchIdentifier, MatchLexical, MatchSemantic} {
		if _, found := seen[match]; found {
			ordered = append(ordered, match)
		}
	}
	return ordered
}
