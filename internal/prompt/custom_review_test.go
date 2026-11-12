package prompt

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	assert.Contains(t, got, "Return exactly one JSON object")
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
		resolveSystemPrompt("codex", "custom", "custom")
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
		resolveSystemPrompt("codex", "custom", "custom")
	require.NoError(t, err)
	assert.True(t, custom)
	assert.Contains(t, got, "Trusted global rubric.")
	assert.NotContains(t, got, "Untrusted working-tree rubric.")
}

func TestCustomReviewReadsExternalSymlinkFromConfiguredRef(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably available on Windows")
	}
	repo := newTestRepo(t)
	externalPath := filepath.Join(t.TempDir(), "review.tmpl")
	require.NoError(t, os.WriteFile(externalPath, []byte("External rubric."), 0o644))
	linkPath := filepath.Join(repo.dir, "review.tmpl")
	linkTarget, err := filepath.Rel(filepath.Dir(linkPath), externalPath)
	require.NoError(t, err)
	require.NoError(t, os.Symlink(linkTarget, linkPath))
	repo.git("add", "review.tmpl")
	repo.git("commit", "-m", "add external rubric link")
	baseRef := repo.git("rev-parse", "HEAD")
	repoCfg := &config.RepoConfig{Review: config.ReviewConfig{
		Types: map[string]config.ReviewTypeSpec{
			"custom": {Template: "review.tmpl"},
		},
	}}

	got, custom, err := NewBuilder(nil).
		ForRepo(repo.dir, 0).
		WithRepoConfig(repoCfg, baseRef).
		resolveSystemPrompt("codex", "custom", "custom")
	require.NoError(t, err)
	assert.True(t, custom)
	assert.Contains(t, got, "External rubric.")
	assert.NotContains(t, got, linkTarget)
}

func TestCustomReviewReadsThroughDirectorySymlinkFromConfiguredRef(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably available on Windows")
	}
	repo := newTestRepo(t)
	require.NoError(t, os.Mkdir(filepath.Join(repo.dir, "shared"), 0o755))
	repo.writeFile("shared/review.tmpl", "Trusted directory-link rubric.")
	require.NoError(t, os.Symlink("shared", filepath.Join(repo.dir, "reviews")))
	repo.git("add", "shared/review.tmpl", "reviews")
	repo.git("commit", "-m", "add linked review directory")
	baseRef := repo.git("rev-parse", "HEAD")
	repo.writeFile("shared/review.tmpl", "Working-tree directory-link rubric.")
	repoCfg := &config.RepoConfig{Review: config.ReviewConfig{
		Types: map[string]config.ReviewTypeSpec{
			"custom": {Template: "reviews/review.tmpl"},
		},
	}}

	got, custom, err := NewBuilder(nil).
		ForRepo(repo.dir, 0).
		WithRepoConfig(repoCfg, baseRef).
		resolveSystemPrompt("codex", "custom", "custom")
	require.NoError(t, err)
	assert.True(t, custom)
	assert.Contains(t, got, "Trusted directory-link rubric.")
	assert.NotContains(t, got, "Working-tree directory-link rubric.")
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
		resolveSystemPrompt("codex", "custom", "custom")
	require.NoError(t, err)
	assert.True(t, custom)
	assert.Contains(t, got, "Trusted global rubric.")
}

func TestCustomReviewMissingDefinitionFails(t *testing.T) {
	_, _, err := NewBuilder(nil).
		ForRepo(t.TempDir(), 0).
		WithRepoConfig(&config.RepoConfig{}, "").
		resolveSystemPrompt("codex", "removed-type", "removed-type")
	require.ErrorContains(t, err, `custom review type "removed-type" is not configured`)
}

func TestCustomReviewFilesPreserveCompleteTemplate(t *testing.T) {
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

	rendered, _, err := NewBuilder(nil).
		ForRepo(repoPath, 0).
		WithRepoConfig(repoCfg, "").
		resolveSystemPrompt(
			"codex", "custom", "custom",
		)
	require.NoError(t, err)
	assert.Contains(t, rendered, strings.Repeat("x", 1024))
}

func TestBuiltInReviewPromptStructuredOutputInstruction(t *testing.T) {
	repoPath := t.TempDir()
	const diff = "diff --git a/a.go b/a.go"

	prose, err := NewBuilder(nil).ForRepo(repoPath, 0).
		BuildDirty(diff, 0, "codex", "", "high")
	require.NoError(t, err)
	assert.NotContains(t, prose, "Return exactly one JSON object")
	assert.NotContains(t, prose, "Severity threshold", "the threshold never reaches the agent")
	assert.NotContains(t, prose, "SEVERITY_THRESHOLD_MET")

	structured, err := NewBuilder(nil).ForRepo(repoPath, 0).WithStructuredOutput(true).
		BuildDirty(diff, 0, "codex", "", "high")
	require.NoError(t, err)
	assert.Contains(t, structured, "Return exactly one JSON object")
	assert.Contains(t, structured, `"unable_to_review" only when you could not assess the change`)
	assert.NotContains(t, structured, "Severity threshold")
	assert.NotContains(t, structured, "high severity")
}

func TestReconcileStructuredOutputInstruction(t *testing.T) {
	assert := assert.New(t)
	prose := "Review the change.\n"
	structured := prose + structuredReviewOutputInstruction

	assert.Equal(structured, ReconcileStructuredOutputInstruction(prose, true),
		"a structured agent gets the instruction appended")
	assert.Equal(structured, ReconcileStructuredOutputInstruction(structured, true),
		"an existing instruction is not duplicated")
	assert.Equal(prose, ReconcileStructuredOutputInstruction(structured, false),
		"a prose agent never sees the JSON instruction")
	assert.Equal(prose, ReconcileStructuredOutputInstruction(prose, false))
}

func TestRenderedRubricBytes(t *testing.T) {
	repoPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "review.tmpl"), []byte("  Check {{ index .Includes \"rule\" }}  \n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "rule.md"), []byte("safety"), 0o644))
	cfg := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{
		"custom": {Template: "review.tmpl", Includes: map[string]string{"rule": "rule.md"}},
	}}}
	b := NewBuilder(nil).ForRepo(repoPath, 0).WithRepoConfig(cfg, "")
	result, err := b.BuildDirtyResult("diff", nil, 0, "test", "custom", "")
	require.NoError(t, err)
	assert.Equal(t, []byte("Check safety\n"), result.Rubric)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("Check safety\n"))), result.RubricSHA256)
	assert.Contains(t, result.Prompt, "Check safety")
	other := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(other, "review.tmpl"), []byte("  Check {{ index .Includes \"rule\" }}  \n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(other, "rule.md"), []byte("safety"), 0o644))
	same, err := NewBuilder(nil).ForRepo(other, 0).WithRepoConfig(cfg, "").BuildDirtyResult("different diff", nil, 0, "test", "custom", "")
	require.NoError(t, err)
	assert.Equal(t, result.RubricSHA256, same.RubricSHA256)
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "rule.md"), []byte("correctness"), 0o644))
	changed, err := b.BuildDirtyResult("diff", nil, 0, "test", "custom", "")
	require.NoError(t, err)
	assert.NotEqual(t, result.RubricSHA256, changed.RubricSHA256)
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "review.tmpl"), []byte("Other {{ index .Includes \"rule\" }}"), 0o644))
	changedTemplate, err := b.BuildDirtyResult("diff", nil, 0, "test", "custom", "")
	require.NoError(t, err)
	assert.NotEqual(t, changed.RubricSHA256, changedTemplate.RubricSHA256)
	builtin, err := b.BuildDirtyResult("diff", nil, 0, "test", "security", "")
	require.NoError(t, err)
	assert.Nil(t, builtin.Rubric)
	assert.Empty(t, builtin.RubricSHA256)
}
