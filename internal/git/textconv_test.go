package git

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTextconvTestRepo commits report.pdf and notes.txt, routes PDFs through a
// textconv driver that always fails and text files through one that prefixes
// each line with "CONV:", and returns the repo with the base commit SHA.
func newTextconvTestRepo(t *testing.T) (*TestRepo, string) {
	t.Helper()
	repo := NewTestRepo(t)
	repo.Run("config", "diff.fail.textconv", "false")
	repo.Run("config", "diff.conv.textconv", "sed s/^/CONV:/")
	repo.WriteFile(".gitattributes", "*.pdf diff=fail\n*.txt diff=conv\n")
	repo.WriteFile("report.pdf", "%PDF\x00v1")
	repo.WriteFile("notes.txt", "old note\n")
	repo.Run("add", "-A")
	repo.Run("commit", "-m", "base")
	return repo, repo.HeadSHA()
}

func TestReviewDiffsFallBackWhenTextconvFails(t *testing.T) {
	ctx := context.Background()
	repo, base := newTextconvTestRepo(t)
	repo.WriteFile("report.pdf", "%PDF\x00v2")
	repo.WriteFile("notes.txt", "new note\n")

	type result struct {
		diff string
		err  error
	}
	results := map[string]result{}
	diff, err := GetDirtyDiff(repo.Dir)
	results["dirty"] = result{diff, err}

	repo.Run("add", "-A")
	repo.Run("commit", "-m", "change report")
	head := repo.HeadSHA()
	rangeRef := base + ".." + head

	diff, err = GetDiffCtx(ctx, repo.Dir, head)
	results["commit"] = result{diff, err}
	diff, _, err = GetDiffLimitedCtx(ctx, repo.Dir, head, 1<<20)
	results["commitLimited"] = result{diff, err}
	diff, err = GetRangeDiffCtx(ctx, repo.Dir, rangeRef)
	results["range"] = result{diff, err}
	diff, _, err = GetRangeDiffLimitedCtx(ctx, repo.Dir, rangeRef, 1<<20)
	results["rangeLimited"] = result{diff, err}

	for name, r := range results {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, r.err)
			assert.Contains(t, r.diff, "Binary files a/report.pdf and b/report.pdf differ")
			assert.Contains(t, r.diff, "+new note")
		})
	}
}

func TestReviewDiffsKeepWorkingTextconvOutput(t *testing.T) {
	ctx := context.Background()
	repo, base := newTextconvTestRepo(t)
	repo.WriteFile("notes.txt", "new note\n")

	dirty, err := GetDirtyDiff(repo.Dir)
	require.NoError(t, err)

	repo.Run("add", "-A")
	repo.Run("commit", "-m", "change notes")
	head := repo.HeadSHA()

	commit, err := GetDiffCtx(ctx, repo.Dir, head)
	require.NoError(t, err)
	rangeDiff, err := GetRangeDiffCtx(ctx, repo.Dir, base+".."+head)
	require.NoError(t, err)

	for name, diff := range map[string]string{
		"dirty":  dirty,
		"commit": commit,
		"range":  rangeDiff,
	} {
		assert.Contains(t, diff, "+CONV:new note", name)
		assert.Contains(t, diff, "-CONV:old note", name)
	}
}
