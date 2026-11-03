package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRefineTrivialFixCommits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		edit  func(*TestGitRepo) []string
		want  bool
	}{
		{"nested docs", map[string]string{"docs/guide.md": "old\n"}, func(repo *TestGitRepo) []string {
			return []string{repo.CommitFile("docs/guide.md", "new\n", "Update docs")}
		}, true},
		{"test file", map[string]string{"pkg/check_test.go": "package pkg\n"}, func(repo *TestGitRepo) []string {
			return []string{repo.CommitFile("pkg/check_test.go", "package pkg\n// more\n", "Update test")}
		}, true},
		{"hook adds docs commit", map[string]string{"README.md": "old\n"}, func(repo *TestGitRepo) []string {
			first := repo.CommitFile("README.md", "new\n", "Fix docs")
			return []string{first, repo.CommitFile("HOOK.md", "hook\n", "Hook docs")}
		}, true},
		{"source file", map[string]string{"README.md": "old\n", "main.go": "package main\n"}, func(repo *TestGitRepo) []string {
			repo.CommitFile("README.md", "new\n", "Fix docs")
			repo.CommitFile("main.go", "package main\n// changed\n", "Hook changes code")
			return nil
		}, false},
		{"rename code to docs", map[string]string{"README.go": "old\n"}, func(repo *TestGitRepo) []string {
			repo.Run("mv", "README.go", "README.md")
			repo.Run("commit", "-m", "Move file")
			return nil
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newPlanTestRepo(t, tc.files)
			before := repo.HeadSHA()
			want := tc.edit(repo)
			commits, ok := refineTrivialFixCommits(t.Context(), repo.Dir, before, repo.HeadSHA())
			assert.Equal(t, tc.want, ok)
			assert.ElementsMatch(t, want, commits)
		})
	}
}
