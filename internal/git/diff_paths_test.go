package git

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiffPathsCtxMatchesReviewDiffProducers(t *testing.T) {
	repo := NewTestRepo(t)
	repo.WriteFile("delete.txt", "delete")
	repo.WriteFile("old name.txt", "old")
	repo.WriteFile("space name.txt", "space")
	repo.WriteFile("unicøde.txt", "unicode")
	repo.WriteFile("binary.dat", "\x00\x01\x02")
	repo.WriteFile("excluded.txt", "excluded")
	repo.WriteFile("root-only.txt", "root")
	repo.CommitAll("root")
	root := repo.HeadSHA()
	repo.Run("mv", "old name.txt", "new name.txt")
	repo.Run("rm", "delete.txt")
	repo.WriteFile("space name.txt", "changed")
	repo.WriteFile("unicøde.txt", "changed unicode")
	repo.WriteFile("binary.dat", "\x00\x03\x04")
	repo.WriteFile("excluded.txt", "changed excluded")
	repo.CommitAll("change paths")
	commit := repo.HeadSHA()

	tests := []struct {
		name     string
		ref      string
		producer func() string
	}{
		{name: "root", ref: root, producer: func() string {
			diff, err := GetDiffCtx(context.Background(), repo.Dir, root, "excluded.txt")
			require.NoError(t, err)
			return diff
		}},
		{name: "commit with configured exclude", ref: commit, producer: func() string {
			diff, err := GetDiffCtx(context.Background(), repo.Dir, commit, "excluded.txt")
			require.NoError(t, err)
			return diff
		}},
		{name: "range with configured exclude", ref: root + ".." + commit, producer: func() string {
			diff, err := GetRangeDiffCtx(context.Background(), repo.Dir, root+".."+commit, "excluded.txt")
			require.NoError(t, err)
			return diff
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := DiffPathsCtx(context.Background(), repo.Dir, test.ref, ReviewPathspecArgs("excluded.txt"))
			require.NoError(t, err)
			want := producerHeaderPaths(test.producer())
			assert.Equal(t, want, got)
		})
	}
}

func TestDiffPathsCtxRootAndMergeDivergeFromGetFilesChanged(t *testing.T) {
	repo := NewTestRepo(t)
	repo.CommitFile("root.txt", "root", "root")
	root := repo.HeadSHA()
	rootPaths, err := DiffPathsCtx(context.Background(), repo.Dir, root, ReviewPathspecArgs())
	require.NoError(t, err)
	changed, err := GetFilesChanged(repo.Dir, root)
	require.NoError(t, err)
	assert.NotEmpty(t, rootPaths)
	assert.Empty(t, changed)

	mainBranch := repo.Run("branch", "--show-current")
	repo.Run("checkout", "-b", "coverage-side")
	repo.CommitFile("side.txt", "side", "side")
	repo.Run("checkout", mainBranch)
	repo.CommitFile("main.txt", "main", "main")
	repo.Run("merge", "--no-ff", "coverage-side", "-m", "merge")
	merge := repo.HeadSHA()

	mergePaths, err := DiffPathsCtx(context.Background(), repo.Dir, merge, ReviewPathspecArgs())
	require.NoError(t, err)
	mergeDiff, err := GetDiffCtx(context.Background(), repo.Dir, merge)
	require.NoError(t, err)
	assert.Empty(t, mergePaths)
	assert.Empty(t, mergeDiff)
}

func producerHeaderPaths(diff string) []string {
	seen := make(map[string]struct{})
	for line := range strings.SplitSeq(diff, "\n") {
		if !strings.HasPrefix(line, "diff --git ") {
			continue
		}
		header := strings.TrimPrefix(line, "diff --git ")
		left, right, ok := parseTestDiffHeader(header)
		if !ok {
			continue
		}
		seen[strings.TrimPrefix(left, "a/")] = struct{}{}
		seen[strings.TrimPrefix(right, "b/")] = struct{}{}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func parseTestDiffHeader(header string) (string, string, bool) {
	if strings.HasPrefix(header, `"`) {
		separator := strings.Index(header, `" "b/`)
		if separator < 0 {
			return "", "", false
		}
		left, err := strconv.Unquote(header[:separator+1])
		if err != nil {
			return "", "", false
		}
		right, err := strconv.Unquote(header[separator+2:])
		if err != nil {
			return "", "", false
		}
		return left, right, true
	}
	separator := strings.Index(header, " b/")
	if separator < 0 {
		return "", "", false
	}
	return header[:separator], header[separator+1:], true
}
