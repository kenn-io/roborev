package prompt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestCustomReviewTemplateRendersNamedIncludes(t *testing.T) {
	repoPath := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoPath, "review.tmpl"),
		[]byte("Review type: {{ .ReviewType }}\n{{ index .Includes \"rubric\" }}"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoPath, "rubric.md"),
		[]byte("Find dangerous abstractions."), 0o644,
	))
	repoCfg := &config.RepoConfig{Review: config.ReviewConfig{
		Types: map[string]config.ReviewTypeSpec{
			"thermonuclear": {
				Template: "review.tmpl",
				Includes: map[string]string{"rubric": "rubric.md"},
			},
		},
	}}

	got, err := NewBuilder(nil).
		ForRepo(repoPath, 0).
		WithRepoConfig(repoCfg, "").
		BuildDirty("diff --git a/a.go b/a.go", 0, "codex", "thermonuclear", "high")
	require.NoError(t, err)
	assert.Contains(t, got, "Review type: thermonuclear")
	assert.Contains(t, got, "Find dangerous abstractions.")
	assert.Contains(t, got, "Roborev will constrain the final response with a JSON Schema")
	assert.NotContains(t, got, "SEVERITY_THRESHOLD_MET")
}

func TestCustomReviewRejectsAbsoluteRepoPath(t *testing.T) {
	repoPath := t.TempDir()
	repoCfg := &config.RepoConfig{Review: config.ReviewConfig{
		Types: map[string]config.ReviewTypeSpec{
			"custom": {Template: filepath.Join(repoPath, "review.tmpl")},
		},
	}}

	_, err := NewBuilder(nil).
		ForRepo(repoPath, 0).
		WithRepoConfig(repoCfg, "").
		BuildDirty("diff", 0, "codex", "custom", "")
	require.ErrorContains(t, err, "must be relative to the repository root")
}

func TestCustomReviewReadsRepoFilesFromConfiguredRef(t *testing.T) {
	repo := newTestRepo(t)
	baseRef := repo.fastCommitFile(
		"review.tmpl", "Trusted base rubric.", "add review rubric",
	)
	repo.writeFile("review.tmpl", "Untrusted working-tree rubric.")
	repoCfg := &config.RepoConfig{Review: config.ReviewConfig{
		Types: map[string]config.ReviewTypeSpec{
			"custom": {Template: "review.tmpl"},
		},
	}}

	got, custom, err := NewBuilder(nil).
		ForRepo(repo.dir, 0).
		WithRepoConfig(repoCfg, baseRef).
		resolveSystemPrompt("codex", "custom", "custom", MaxPromptSize)
	require.NoError(t, err)
	assert.True(t, custom)
	assert.Contains(t, got, "Trusted base rubric.")
	assert.NotContains(t, got, "Untrusted working-tree rubric.")
}
