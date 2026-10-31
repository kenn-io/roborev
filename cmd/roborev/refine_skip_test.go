package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func trivialReview(severity string) *storage.Review {
	return &storage.Review{Job: &storage.ReviewJob{JobType: storage.JobTypeReview}, StructuredOutput: storage.StructuredOutput{
		"schema_version": 2, "summary": "Review findings", "verdict": "fail",
		"findings": []any{map[string]any{"severity": severity, "problem": "Incorrect documentation", "fix": "Correct the example", "location": "README.md"}},
	}}
}

func TestRefineTrivialReview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*storage.Review)
		want   bool
	}{
		{"medium", func(*storage.Review) {}, true},
		{"low", func(r *storage.Review) { r.StructuredOutput = trivialReview("low").StructuredOutput }, true},
		{"schema one", func(r *storage.Review) {
			r.StructuredOutput["schema_version"] = 1
			delete(r.StructuredOutput, "verdict")
		}, true},
		{"high", func(r *storage.Review) { r.StructuredOutput = trivialReview("high").StructuredOutput }, false},
		{"unknown severity", func(r *storage.Review) { r.StructuredOutput = trivialReview("unknown").StructuredOutput }, false},
		{"all original findings", func(r *storage.Review) {
			r.StructuredOutput["findings"] = append(r.StructuredOutput["findings"].([]any), trivialReview("high").StructuredOutput["findings"].([]any)...)
		}, false},
		{"security review", func(r *storage.Review) { r.Job.ReviewType = "security" }, false},
		{"schema three", func(r *storage.Review) {
			r.StructuredOutput["schema_version"] = 3
			finding := r.StructuredOutput["findings"].([]any)[0].(map[string]any)
			delete(finding, "location")
			finding["file"] = "README.md"
		}, true},
		{"security topic", func(r *storage.Review) {
			r.StructuredOutput["schema_version"] = 3
			f := r.StructuredOutput["findings"].([]any)[0].(map[string]any)
			delete(f, "location")
			f["file"] = "README.md"
			f["topic"] = "security"
		}, false},
		{"synthesis", func(r *storage.Review) { r.Job.JobType = storage.JobTypeSynthesis }, false},
		{"no job", func(r *storage.Review) { r.Job = nil }, false},
		{"no structured output", func(r *storage.Review) { r.StructuredOutput = nil }, false},
		{"empty findings", func(r *storage.Review) { r.StructuredOutput["findings"] = []any{} }, false},
		{"unable", func(r *storage.Review) { r.StructuredOutput["verdict"] = "unable_to_review" }, false},
		{"future", func(r *storage.Review) { r.StructuredOutput["schema_version"] = 99 }, false},
		{"invalid", func(r *storage.Review) { r.StructuredOutput["extra"] = true }, false},
		{"legacy", func(r *storage.Review) {
			r.StructuredOutput = storage.StructuredOutput{"schema_version": 0, "legacy": map[string]any{"markdown": "Medium: fix docs", "recorded_verdict": false}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := trivialReview("medium")
			tc.change(r)
			assert.Equal(t, tc.want, refineTrivialReview(r))
		})
	}
}

func rawTrivialChange(oldMode, newMode, status, path string) string {
	oldHash, newHash := strings.Repeat("a", 40), strings.Repeat("b", 40)
	if oldMode == "000000" {
		oldHash = strings.Repeat("0", 40)
	}
	if newMode == "000000" {
		newHash = strings.Repeat("0", 40)
	}
	return fmt.Sprintf(":%s %s %s %s %s\x00%s\x00", oldMode, newMode, oldHash, newHash, status, path)
}

func TestRefineTrivialDiff(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      bool
	}{
		{"docs", rawTrivialChange("100644", "100644", "M", "README.md"), true},
		{"test add", rawTrivialChange("000000", "100644", "A", "pkg/check_test.go"), true},
		{"test delete", rawTrivialChange("100644", "000000", "D", "pkg/testdata/sample"), true},
		{"whitespace", rawTrivialChange("100644", "100644", "M", "docs/a b\nc.md"), true},
		{"mixed", rawTrivialChange("100644", "100644", "M", "README.md") + rawTrivialChange("100644", "100644", "M", "main.go"), false},
		{"executable", rawTrivialChange("100644", "100755", "M", "example.spec.sh"), false},
		{"symlink", rawTrivialChange("000000", "120000", "A", "link.md"), false},
		{"gitlink", rawTrivialChange("000000", "160000", "A", "testdata/sub"), false},
		{"inconsistent status", rawTrivialChange("000000", "100644", "M", "README.md"), false},
		{"type change", rawTrivialChange("100644", "120000", "T", "README.md"), false},
		{"missing terminator", strings.TrimSuffix(rawTrivialChange("100644", "100644", "M", "README.md"), "\x00"), false},
		{"malformed tail", rawTrivialChange("100644", "100644", "M", "README.md") + "garbage\x00", false},
		{"empty path", rawTrivialChange("100644", "100644", "M", ""), false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, refineTrivialDiff([]byte(tc.raw))) })
	}
}

func TestRefineTrivialFix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		edit  func(*TestGitRepo, string)
		want  bool
	}{
		{"nested docs", map[string]string{"docs/guide.md": "old\n"}, func(repo *TestGitRepo, _ string) {
			repo.CommitFile("docs/guide.md", "new\n", "Update docs")
		}, true},
		{"rename code to docs", map[string]string{"README.go": "old\n"}, func(repo *TestGitRepo, _ string) {
			repo.Run("mv", "README.go", "README.md")
			repo.Run("commit", "-m", "Move file back")
		}, false},
		{"extra hook commit", map[string]string{"README.md": "old\n", "main.go": "package main\n"}, func(repo *TestGitRepo, _ string) {
			repo.CommitFile("main.go", "package main\n// changed\n", "Change logic")
			repo.CommitFile("README.md", "next\n", "Update docs after hook")
		}, false},
		{"reverted source changes", map[string]string{"README.md": "old\n", "main.go": "package main\n// changed\n"}, func(repo *TestGitRepo, _ string) {
			repo.CommitFile("main.go", "package main\n// temporary\n", "Temporary source change")
			repo.CommitFile("main.go", "package main\n// changed\n", "Revert temporary source change")
			repo.CommitFile("README.md", "Final docs\n", "Finish docs")
		}, true},
		{"gitlink", map[string]string{"README.md": "old\n"}, func(repo *TestGitRepo, before string) {
			repo.Run("update-index", "--add", "--cacheinfo", "160000", before, "testdata/dependency")
			repo.CommitFile("README.md", "new\n", "Update docs and submodule")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newPlanTestRepo(t, tc.files)
			before := repo.HeadSHA()
			tc.edit(repo, before)
			_, got := refineTrivialFixCommits(t.Context(), repo.Dir, before, repo.HeadSHA(), trivialReview("medium"))
			assert.Equal(t, tc.want, got)
		})
	}
	repo := newPlanTestRepo(t, map[string]string{"README.md": "old\n"})
	_, got := refineTrivialFixCommits(t.Context(), repo.Dir, "missing", repo.HeadSHA(), trivialReview("medium"))
	assert.False(t, got)
}

func FuzzRefineTrivialDiff(f *testing.F) {
	f.Add("guide", uint8(0))
	f.Add("a b\nc", uint8(2))
	f.Add("", uint8(3))
	f.Fuzz(func(t *testing.T, name string, variant uint8) {
		// Materialize names as one path component without narrowing the input domain.
		name = strings.NewReplacer("\x00", "_", "/", "_", "\\", "_").Replace(name)
		path := "docs/" + name + ".md"
		raw := rawTrivialChange("100644", "100644", "M", path)
		switch variant % 4 {
		case 0:
			assert.True(t, refineTrivialDiff([]byte(raw)))
		case 1:
			assert.False(t, refineTrivialDiff([]byte(raw+rawTrivialChange("100644", "100644", "M", "main.go"))))
		case 2:
			assert.False(t, refineTrivialDiff([]byte(raw+"incomplete")))
		case 3:
			assert.False(t, refineTrivialDiff([]byte(rawTrivialChange("100644", "100755", "M", path))))
		}
	})
}

func FuzzRefineTrivialReview(f *testing.F) {
	f.Add("medium", false, false)
	f.Add("low", true, false)
	f.Add("HIGH", false, true)
	f.Add(" critical ", false, false)
	f.Add("", false, true)
	f.Fuzz(func(t *testing.T, severity string, security, first bool) {
		review := trivialReview("low")
		extra := trivialReview(severity).StructuredOutput["findings"].([]any)[0]
		findings := review.StructuredOutput["findings"].([]any)
		if first {
			findings = append([]any{extra}, findings...)
		} else {
			findings = append(findings, extra)
		}
		review.StructuredOutput["findings"] = findings
		if security {
			review.StructuredOutput["schema_version"] = 3
			for _, item := range findings {
				finding := item.(map[string]any)
				delete(finding, "location")
				finding["file"] = "README.md"
			}
			extra.(map[string]any)["topic"] = " SECURITY "
		}
		normalized := strings.ToLower(strings.TrimSpace(severity))
		assert.Equal(t, !security && (normalized == "medium" || normalized == "low"), refineTrivialReview(review))
	})
}
