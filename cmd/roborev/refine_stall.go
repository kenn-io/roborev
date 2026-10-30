package main

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"unicode"

	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// refineStallTracker counts completed failing fix rounds, not agent retries.
// Three rounds is an advisory heuristic from the measured case/proposal in
// https://github.com/kenn-io/roborev/issues/1002#issuecomment-6489157587;
// repeated files alone do not establish that a fix made no progress.
type refineStallTracker struct {
	previous              map[string]struct{}
	rounds                int
	pendingFixSHA         string
	reviewID              int64
	previousSeverityCount int
	severityRounds        int
}

func (s *refineStallTracker) reset() { *s = refineStallTracker{} }

// resetSignals keeps the pending fix and review identity for asynchronous recovery.
func (s *refineStallTracker) resetSignals() {
	s.previous = nil
	s.rounds = 0
	s.previousSeverityCount = 0
	s.severityRounds = 0
}

// observeSeverity counts non-decreasing positive High plus Medium totals.
// Critical is deliberately excluded from this metric, not treated as High.
func (s *refineStallTracker) observeSeverity(count int) bool {
	if count > 0 && s.previousSeverityCount > 0 && count >= s.previousSeverityCount {
		s.severityRounds++
	} else {
		s.severityRounds = 0
	}
	s.previousSeverityCount = count
	return s.severityRounds == 3
}

func (s *refineStallTracker) observe(files map[string]struct{}) bool {
	if len(files) == 0 {
		s.previous = nil
		s.rounds = 0
		return false
	}
	subset := len(s.previous) > 0
	for file := range files {
		if _, ok := s.previous[file]; !ok {
			subset = false
			break
		}
	}
	if subset {
		s.rounds++
	} else {
		s.rounds = 0
	}
	s.previous = maps.Clone(files)
	return s.rounds == 3
}

// observeReview is shared by immediate and recovered re-reviews. Consume the
// pending SHA before extraction so re-selecting the same review cannot count it
// again. Without a pending fix, a different review starts a new baseline.
func (s *refineStallTracker) observeReview(ctx context.Context, repoPath string, review *storage.Review) bool {
	if review == nil || review.Closed || review.Verdict() != "F" || review.Job == nil || review.Job.Status != storage.JobStatusDone {
		s.reset()
		return false
	}
	if s.pendingFixSHA != "" {
		sha := s.pendingFixSHA
		s.pendingFixSHA = ""
		if !refineReviewIncludes(ctx, repoPath, review, sha) {
			s.reset()
		}
	} else if s.reviewID == review.JobID {
		return false
	} else {
		s.reset()
	}
	signal := s.observe(refineFindingFiles(ctx, repoPath, review))
	severitySignal := s.observeSeverity(refineHighMediumCount(review))
	s.reviewID = review.JobID
	return signal || severitySignal
}

// refineFindingPath recognizes one anchored location, only when its exact path
// belongs to the reviewed tree. Membership disambiguates spaces from prose.
func refineFindingPath(location string, tracked map[string]struct{}) (string, bool) {
	location = strings.TrimSpace(location)
	if strings.HasPrefix(location, "`") && strings.HasSuffix(location, "`") && len(location) > 1 {
		location = location[1 : len(location)-1]
	}
	if strings.ContainsAny(location, "`\\") || strings.ContainsFunc(location, unicode.IsControl) {
		return "", false
	}
	path, line, hasLine := strings.Cut(location, ":")
	if hasLine {
		first, last, hasRange := strings.Cut(line, "-")
		positive := func(n string) string {
			for _, digit := range n {
				if digit < '0' || digit > '9' {
					return ""
				}
			}
			return strings.TrimLeft(n, "0")
		}
		start := positive(first)
		if start == "" {
			return "", false
		}
		if hasRange {
			end := positive(last)
			if end == "" || len(start) > len(end) || len(start) == len(end) && start > end {
				return "", false
			}
		}
	}
	for strings.HasPrefix(path, "./") {
		path = strings.TrimPrefix(path, "./")
	}
	if path == "" || strings.HasPrefix(path, "/") {
		return "", false
	}
	for part := range strings.SplitSeq(path, "/") {
		if part == ".." || part == "." || part == "" {
			return "", false
		}
	}
	if _, ok := tracked[path]; !ok {
		return "", false
	}
	return path, true
}

func refineFindingFiles(ctx context.Context, repoPath string, review *storage.Review) map[string]struct{} {
	if review == nil || review.Job == nil || review.StructuredOutput == nil {
		return nil
	}
	raw, err := json.Marshal(review.StructuredOutput)
	if err != nil {
		return nil
	}
	document, err := structuredreview.Decode(raw)
	if err != nil || document.Legacy != nil || document.UnableToReview() {
		return nil
	}
	endpoint, ok := refinePlanReviewEndpoint(review.Job.GitRef)
	if !ok {
		return nil
	}
	resolved, err := gitrepo.Resolve(ctx, repoPath, endpoint)
	if err != nil {
		return nil
	}
	tree, _, err := refineGitRunner().Run(ctx, repoPath, nil, "ls-tree", "-r", "--name-only", "-z", resolved)
	if err != nil {
		return nil
	}
	tracked := make(map[string]struct{})
	for path := range strings.SplitSeq(string(tree), "\x00") {
		if path != "" {
			tracked[path] = struct{}{}
		}
	}
	var files map[string]struct{}
	for _, finding := range document.Findings {
		if path, ok := refineFindingPath(finding.Location, tracked); ok {
			if files == nil {
				files = make(map[string]struct{})
			}
			files[path] = struct{}{}
		}
	}
	return files
}

// refineHighMediumCount uses the stored structured severities, never Markdown
// or location parsing. Unusable documents cannot establish a severity baseline.
func refineHighMediumCount(review *storage.Review) int {
	if review == nil || review.StructuredOutput == nil {
		return 0
	}
	raw, err := json.Marshal(review.StructuredOutput)
	if err != nil {
		return 0
	}
	document, err := structuredreview.Decode(raw)
	if err != nil || document.Legacy != nil || document.UnableToReview() {
		return 0
	}
	count := 0
	for _, finding := range document.Findings {
		if finding.Severity == "high" || finding.Severity == "medium" {
			count++
		}
	}
	return count
}
