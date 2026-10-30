package main

import (
	"context"
	"encoding/json/v2"
	"strings"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/review/autotype"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// refineTrivialReview checks the entire original review, before severity filtering.
// Synthesis may include security members regardless of the parent's review type.
func refineTrivialReview(review *storage.Review) bool {
	if review == nil || review.Job == nil || review.Job.ReviewType == config.ReviewTypeSecurity || review.Job.JobType == storage.JobTypeSynthesis {
		return false
	}
	raw, err := json.Marshal(review.StructuredOutput)
	if err != nil {
		return false
	}
	doc, err := structuredreview.Decode(raw)
	if err != nil || doc.Legacy != nil || doc.Verdict == structuredreview.VerdictUnableToReview || len(doc.Findings) == 0 {
		return false
	}
	for _, finding := range doc.Findings {
		if (finding.Severity != "medium" && finding.Severity != "low") || finding.Topic == structuredreview.TopicSecurity {
			return false
		}
	}
	return true
}

// refineTrivialDiff consumes raw -z output with rename detection disabled. Both
// sides of a rename are therefore checked, including deletions of source files.
func refineTrivialDiff(raw []byte) bool {
	records := strings.Split(string(raw), "\x00")
	if len(records) < 3 || len(records)%2 != 1 || records[len(records)-1] != "" {
		return false
	}
	paths := make([]string, 0, len(records)/2)
	for i := 0; i < len(records)-1; i += 2 {
		header := strings.Fields(records[i])
		if len(header) != 5 || !strings.HasPrefix(header[0], ":") || records[i+1] == "" {
			return false
		}
		oldMode, newMode := strings.TrimPrefix(header[0], ":"), header[1]
		switch header[4] {
		case "A":
			if oldMode != "000000" || newMode != "100644" {
				return false
			}
		case "D":
			if oldMode != "100644" || newMode != "000000" {
				return false
			}
		case "M":
			if oldMode != "100644" || newMode != "100644" {
				return false
			}
		default:
			return false
		}
		paths = append(paths, records[i+1])
	}
	ok, err := autotype.AllMatch(autotype.DefaultHeuristics().SkipPaths, paths)
	return err == nil && ok
}

// refineTrivialFixCommits returns every commit in the checked fix range when
// the complete diff is eligible for skipping individual re-reviews.
func refineTrivialFixCommits(
	ctx context.Context, repo, before, after string, review *storage.Review,
) ([]string, bool) {
	if !refineTrivialReview(review) {
		return nil, false
	}
	// Include changes made by commit-hook retries or additional hook commits.
	raw, err := refineGitRunner().Output(
		ctx, repo, "diff-tree", "--no-commit-id", "--raw", "-z", "-r",
		"--no-renames", "--no-ext-diff", before, after, "--",
	)
	if err != nil || !refineTrivialDiff(raw) {
		return nil, false
	}
	commits, err := git.GetRangeCommitsCtx(
		ctx, repo, before+".."+after,
	)
	if err != nil || len(commits) == 0 {
		return nil, false
	}
	return commits, true
}
