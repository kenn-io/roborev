package autotype

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type nilClassifier struct{}

func (nilClassifier) Decide(ctx context.Context, in Input) (bool, string, error) {
	return false, "", ErrNeedsClassifier
}

func BenchmarkClassify_HeuristicTrigger(b *testing.B) {
	in := Input{
		RepoPath:     "/tmp/repo",
		GitRef:       "abc",
		Diff:         "+CREATE TABLE foo (id INT);\n",
		Message:      "feat: add users table",
		ChangedFiles: []string{"db/migrations/001_users.sql"},
	}
	h := DefaultHeuristics()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Classify(b.Context(), in, h, nil)
	}
}

func BenchmarkClassify_HeuristicSkip(b *testing.B) {
	in := Input{
		RepoPath:     "/tmp/repo",
		GitRef:       "abc",
		Diff:         strings.Repeat("+doc line\n", 30),
		Message:      "docs: update README",
		ChangedFiles: []string{"README.md", "docs/intro.md"},
	}
	h := DefaultHeuristics()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Classify(b.Context(), in, h, nil)
	}
}

func BenchmarkClassify_Ambiguous(b *testing.B) {
	in := Input{
		RepoPath:     "/tmp/repo",
		GitRef:       "abc",
		Diff:         strings.Repeat("+x := do()\n", 50),
		Message:      "feat: small helper",
		ChangedFiles: []string{"src/helper.go"},
	}
	h := DefaultHeuristics()
	cls := nilClassifier{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := Classify(b.Context(), in, h, cls)
		if err != nil && !errors.Is(err, ErrNeedsClassifier) {
			b.Fatal(err)
		}
	}
}
