package searchindex

import "slices"

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

func mergeGroupRRF(lexical, semantic []rankedCandidate, limit int) []rankedCandidate {
	lexical = groupLegCandidates(lexical)
	semantic = groupLegCandidates(semantic)
	type fusedGroup struct {
		candidate rankedCandidate
		score     float64
		matches   []string
		bestRank  int
		lexical   bool
	}
	groups := make(map[string]fusedGroup, len(lexical)+len(semantic))
	add := func(candidates []rankedCandidate, lexicalLeg bool) {
		for _, candidate := range candidates {
			group := groups[candidate.GroupKey]
			group.score += 1 / float64(reciprocalRankConstant+candidate.Rank)
			group.matches = append(group.matches, candidate.MatchedIn...)
			if group.bestRank == 0 || candidate.Rank < group.bestRank ||
				(candidate.Rank == group.bestRank && lexicalLeg && !group.lexical) {
				group.candidate = candidate
				group.bestRank = candidate.Rank
				group.lexical = lexicalLeg
			}
			groups[candidate.GroupKey] = group
		}
	}
	add(lexical, true)
	add(semantic, false)

	merged := make([]rankedCandidate, 0, len(groups))
	for _, group := range groups {
		candidate := group.candidate
		candidate.Score = group.score
		candidate.MatchedIn = stableMatches(group.matches)
		merged = append(merged, candidate)
	}
	slices.SortFunc(merged, func(left, right rankedCandidate) int {
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
	return merged
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
