package main

import (
	"context"
	"strings"

	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/review/autotype"
)

// refineTrivialFixCommits returns the commits in before..after when every path
// the range changes is documentation or tests. The range includes commits that
// post-commit hooks add, and --no-renames makes both sides of a rename count.
func refineTrivialFixCommits(
	ctx context.Context, repo, before, after string,
) ([]string, bool) {
	raw, err := refineGitRunner().Output(
		ctx, repo, "diff-tree", "--name-only", "-z", "-r", "--no-renames",
		before, after, "--",
	)
	if err != nil {
		return nil, false
	}
	paths := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if paths[0] == "" {
		return nil, false
	}
	ok, err := autotype.AllMatch(autotype.DefaultHeuristics().SkipPaths, paths)
	if err != nil || !ok {
		return nil, false
	}
	commits, err := git.GetRangeCommitsCtx(ctx, repo, before+".."+after)
	if err != nil || len(commits) == 0 {
		return nil, false
	}
	return commits, true
}

const refineSkipNote = "Skipped individual re-review: documentation/test-only fix."
