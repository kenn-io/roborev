package prompt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExhaustiveReviewExtendsTheDefaultPrompt(t *testing.T) {
	r := newGoldenTestRepo(t)
	sha := r.commitFile("hello.txt", strings.Repeat("line\n", 250), "add lines")
	b := NewBuilder(nil).ForRepo(r.dir, 0)

	base, err := b.Build(sha, 0, "codex", "", "")
	require.NoError(t, err)
	exhaustive, err := b.Build(sha, 0, "codex", "exhaustive", "")
	require.NoError(t, err)

	assert := assert.New(t)
	assert.True(strings.HasPrefix(exhaustive, base), "exhaustive review must keep the default prompt")
	extra := strings.TrimPrefix(exhaustive, base)
	assert.Contains(extra, "## Exhaustive review")
	assert.Contains(extra, "This change has 250 changed lines.")
	assert.Contains(extra, "usually has 15 or more findings")
	assert.NotContains(base, "Exhaustive review")
}

func TestExhaustiveReviewSizesUncommittedChanges(t *testing.T) {
	r := newGoldenTestRepo(t)
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"

	prompt, err := NewBuilder(nil).ForRepo(r.dir, 0).BuildDirty(diff, 0, "codex", "exhaustive", "")
	require.NoError(t, err)

	assert.Contains(t, prompt, "This change has 2 changed lines.")
	assert.Contains(t, prompt, "usually has 1 or more findings")
}

func TestExpectedFindings(t *testing.T) {
	tests := []struct {
		name  string
		lines int
		want  int
	}{
		{"six per hundred lines", 300, 18},
		{"rounds to nearest", 125, 8},
		{"at least one", 5, 1},
		{"capped", 13575, maxExpectedFindings},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, expectedFindings(tt.lines))
		})
	}
}

func TestCountChangedLines(t *testing.T) {
	diff := `diff --git a/x.sql b/x.sql
index 1111111..2222222 100644
--- a/x.sql
+++ b/x.sql
@@ -1,3 +1,3 @@
 select 1;
--- old comment
+-- new comment
+select 2;
diff --git a/y.go b/y.go
new file mode 100644
--- /dev/null
+++ b/y.go
@@ -0,0 +1 @@
+package y
`
	assert.Equal(t, 4, countChangedLines(diff))
}
