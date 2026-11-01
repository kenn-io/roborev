package autotype

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubClassifier struct {
	yes    bool
	reason string
	err    error
}

func (s stubClassifier) Decide(ctx context.Context, in Input) (bool, string, error) {
	return s.yes, s.reason, s.err
}

func newTestHeuristics() Heuristics { return DefaultHeuristics() }

func TestClassify_TriggerPath(t *testing.T) {
	d, err := Classify(t.Context(), Input{
		ChangedFiles: []string{"db/migrations/001.sql"},
		Diff:         "+CREATE TABLE foo (\n",
		Message:      "feat: add foo table",
	}, newTestHeuristics(), nil)
	require.NoError(t, err)
	assert.True(t, d.Run)
	assert.Equal(t, MethodHeuristic, d.Method)
	assert.Contains(t, d.Reason, "touches")
}

func TestClassify_TriggerLargeDiff(t *testing.T) {
	d, err := Classify(t.Context(), Input{
		ChangedFiles: []string{"src/foo.go"},
		Diff:         strings.Repeat("+newline\n", 600),
		Message:      "feat: expand",
	}, newTestHeuristics(), nil)
	require.NoError(t, err)
	assert.True(t, d.Run)
	assert.Equal(t, MethodHeuristic, d.Method)
	assert.Contains(t, d.Reason, "large")
}

func TestClassify_TriggerFileCount(t *testing.T) {
	files := make([]string, 15)
	for i := range files {
		files[i] = "src/f" + string(rune('a'+i)) + ".go"
	}
	d, err := Classify(t.Context(), Input{
		ChangedFiles: files,
		Diff:         "+x\n+y\n",
		Message:      "feat: spread",
	}, newTestHeuristics(), nil)
	require.NoError(t, err)
	assert.True(t, d.Run)
	assert.Contains(t, d.Reason, "wide-reaching")
}

func TestClassify_TriggerMessage(t *testing.T) {
	d, err := Classify(t.Context(), Input{
		ChangedFiles: []string{"src/foo.go"},
		Diff:         "+x\n+y\n+z\n+a\n+b\n+c\n+d\n+e\n+f\n+g\n+h\n+i\n+j\n+k\n+l\n+m\n",
		Message:      "refactor: split auth module",
	}, newTestHeuristics(), nil)
	require.NoError(t, err)
	assert.True(t, d.Run)
	assert.Contains(t, d.Reason, "refactor")
}

func TestClassify_SkipTrivialDiff(t *testing.T) {
	d, err := Classify(t.Context(), Input{
		ChangedFiles: []string{"src/foo.go"},
		Diff:         "+x\n",
		Message:      "fix: oneliner",
	}, newTestHeuristics(), nil)
	require.NoError(t, err)
	assert.False(t, d.Run)
	assert.Equal(t, MethodHeuristic, d.Method)
	assert.Contains(t, d.Reason, "trivial")
}

func TestClassify_SkipAllMatchPaths(t *testing.T) {
	d, err := Classify(t.Context(), Input{
		ChangedFiles: []string{"README.md", "CHANGELOG.md"},
		Diff:         strings.Repeat("+line\n", 30),
		Message:      "feat: new section",
	}, newTestHeuristics(), nil)
	require.NoError(t, err)
	assert.False(t, d.Run)
	assert.Contains(t, d.Reason, "doc/test-only")
}

func TestClassify_SkipMessage(t *testing.T) {
	d, err := Classify(t.Context(), Input{
		ChangedFiles: []string{"src/foo.go"},
		Diff:         strings.Repeat("+line\n", 30),
		Message:      "chore: bump go.mod",
	}, newTestHeuristics(), nil)
	require.NoError(t, err)
	assert.False(t, d.Run)
	assert.Contains(t, d.Reason, "conventional marker")
}

func TestClassify_TriggerPathBeatsSkipMessage(t *testing.T) {
	d, err := Classify(t.Context(), Input{
		ChangedFiles: []string{"docs/superpowers/specs/new-spec.md"},
		Diff:         "+# Spec\n+body\n+more\n+more\n+more\n+more\n+more\n+more\n+more\n+more\n+more\n",
		Message:      "docs: add spec",
	}, newTestHeuristics(), nil)
	require.NoError(t, err)
	assert.True(t, d.Run)
	assert.Contains(t, d.Reason, "touches")
}

func TestClassify_ClassifierYes(t *testing.T) {
	d, err := Classify(t.Context(), Input{
		ChangedFiles: []string{"src/foo.go"},
		Diff:         strings.Repeat("+line\n", 50),
		Message:      "feat: revise API",
	}, newTestHeuristics(), stubClassifier{yes: true, reason: "public API change"})
	require.NoError(t, err)
	assert.True(t, d.Run)
	assert.Equal(t, MethodClassifier, d.Method)
	assert.Equal(t, "public API change", d.Reason)
}

func TestClassify_ClassifierNo(t *testing.T) {
	d, err := Classify(t.Context(), Input{
		ChangedFiles: []string{"src/foo.go"},
		Diff:         strings.Repeat("+line\n", 50),
		Message:      "feat: rename var",
	}, newTestHeuristics(), stubClassifier{yes: false, reason: "local rename"})
	require.NoError(t, err)
	assert.False(t, d.Run)
	assert.Equal(t, MethodClassifier, d.Method)
	assert.Equal(t, "local rename", d.Reason)
}

func TestClassify_ClassifierNilWhenAmbiguous(t *testing.T) {
	_, err := Classify(t.Context(), Input{
		ChangedFiles: []string{"src/foo.go"},
		Diff:         strings.Repeat("+line\n", 50),
		Message:      "feat: rename var",
	}, newTestHeuristics(), nil)
	assert.ErrorContains(t, err, "classifier required")
}

func TestClassify_HeuristicReasonSanitized(t *testing.T) {
	// File path with control chars + newline — must not survive into Reason.
	d, err := Classify(t.Context(), Input{
		ChangedFiles: []string{"db/migrations/001\x07evil\nfoo.sql"},
		Diff:         "+x\n",
		Message:      "feat: x",
	}, newTestHeuristics(), nil)
	require.NoError(t, err)
	assert.True(t, d.Run)
	assert.NotContains(t, d.Reason, "\x07")
	assert.NotContains(t, d.Reason, "\n")
}

func TestClassify_HeuristicReasonCappedLength(t *testing.T) {
	huge := "db/migrations/" + strings.Repeat("a", 1000) + ".sql"
	d, err := Classify(t.Context(), Input{
		ChangedFiles: []string{huge},
		Diff:         "+x\n",
		Message:      "feat: x",
	}, newTestHeuristics(), nil)
	require.NoError(t, err)
	assert.True(t, d.Run)
	assert.LessOrEqual(t, len(d.Reason), reasonMaxLen)
}

func TestClassify_ClassifierError(t *testing.T) {
	_, err := Classify(t.Context(), Input{
		ChangedFiles: []string{"src/foo.go"},
		Diff:         strings.Repeat("+line\n", 50),
		Message:      "feat: rename var",
	}, newTestHeuristics(), stubClassifier{err: errors.New("boom")})
	assert.ErrorContains(t, err, "boom")
}
