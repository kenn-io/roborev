package prompt

import (
	"os"
	"path/filepath"
	"strings"
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

func TestCustomReviewReadsAbsoluteRepoConfiguredPath(t *testing.T) {
	repoPath := t.TempDir()
	templatePath := filepath.Join(t.TempDir(), "review.tmpl")
	require.NoError(t, os.WriteFile(templatePath, []byte("External rubric."), 0o644))
	repoCfg := &config.RepoConfig{Review: config.ReviewConfig{
		Types: map[string]config.ReviewTypeSpec{
			"custom": {Template: templatePath},
		},
	}}

	got, err := NewBuilder(nil).
		ForRepo(repoPath, 0).
		WithRepoConfig(repoCfg, "").
		BuildDirty("diff", 0, "codex", "custom", "")
	require.NoError(t, err)
	assert.Contains(t, got, "External rubric.")
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

func TestCustomReviewReadsGlobalRelativeFilesFromConfiguredRef(t *testing.T) {
	repo := newTestRepo(t)
	baseRef := repo.fastCommitFile(
		"review.tmpl", "Trusted global rubric.", "add global rubric",
	)
	repo.writeFile("review.tmpl", "Untrusted working-tree rubric.")
	globalCfg := &config.Config{Review: config.ReviewConfig{
		Types: map[string]config.ReviewTypeSpec{
			"custom": {Template: "review.tmpl"},
		},
	}}

	got, custom, err := NewBuilderWithConfig(nil, globalCfg).
		ForRepo(repo.dir, 0).
		WithRepoConfig(nil, baseRef).
		resolveSystemPrompt("codex", "custom", "custom", MaxPromptSize)
	require.NoError(t, err)
	assert.True(t, custom)
	assert.Contains(t, got, "Trusted global rubric.")
	assert.NotContains(t, got, "Untrusted working-tree rubric.")
}

func TestCustomReviewUsesExplicitNilRepoConfig(t *testing.T) {
	repo := newTestRepo(t)
	baseRef := repo.fastCommitFiles(map[string]string{
		".roborev.toml": "invalid = [",
		"review.tmpl":   "Trusted global rubric.",
	}, "add global rubric")
	globalCfg := &config.Config{Review: config.ReviewConfig{
		Types: map[string]config.ReviewTypeSpec{
			"custom": {Template: "review.tmpl"},
		},
	}}

	got, custom, err := NewBuilderWithConfig(nil, globalCfg).
		ForRepo(repo.dir, 0).
		WithRepoConfig(nil, baseRef).
		resolveSystemPrompt("codex", "custom", "custom", MaxPromptSize)
	require.NoError(t, err)
	assert.True(t, custom)
	assert.Contains(t, got, "Trusted global rubric.")
}

func TestCustomReviewMissingDefinitionFails(t *testing.T) {
	_, _, err := NewBuilder(nil).
		ForRepo(t.TempDir(), 0).
		WithRepoConfig(&config.RepoConfig{}, "").
		resolveSystemPrompt("codex", "removed-type", "removed-type", MaxPromptSize)
	require.ErrorContains(t, err, `custom review type "removed-type" is not configured`)
}

func TestCustomReviewFilesUsePromptLimit(t *testing.T) {
	repoPath := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoPath, "review.tmpl"),
		[]byte(strings.Repeat("x", 1024)), 0o644,
	))
	repoCfg := &config.RepoConfig{Review: config.ReviewConfig{
		Types: map[string]config.ReviewTypeSpec{
			"custom": {Template: "review.tmpl"},
		},
	}}

	_, _, err := NewBuilder(nil).
		ForRepo(repoPath, 0).
		WithRepoConfig(repoCfg, "").
		resolveSystemPrompt(
			"codex", "custom", "custom", 512,
		)
	require.ErrorContains(t, err, "prompt limit")
}
