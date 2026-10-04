package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoalReviewConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg := ResolveGoalReview(nil)
		assert := assert.New(t)
		assert.False(cfg.Enabled)
		assert.Nil(cfg.SpecFile)
		assert.Nil(cfg.PlanFile)
		assert.Equal([]string{"goal", "kata_graph"}, cfg.Watch)
		assert.Equal("block", ResolveGoalReviewGate(nil))
	})

	t.Run("repo settings", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, ".roborev.toml"), []byte(`
[goal_review]
enabled = true
spec_file = "docs/superpowers/specs/feature-design.md"
plan_file = "docs/superpowers/plans/feature.md"
watch = []
`), 0o600))
		repo, err := LoadRepoConfig(root)
		require.NoError(t, err)
		cfg := ResolveGoalReview(repo)
		assert := assert.New(t)
		assert.True(cfg.Enabled)
		assert.Equal(new("docs/superpowers/specs/feature-design.md"), cfg.SpecFile)
		assert.Equal(new("docs/superpowers/plans/feature.md"), cfg.PlanFile)
		assert.Empty(cfg.Watch)
	})

	for _, invalid := range []string{
		`spec_file = ""`, `plan_file = "  "`,
		`watch = ["commits"]`, `watch = ["goal", "goal"]`,
	} {
		t.Run(invalid, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, ".roborev.toml"),
				[]byte("[goal_review]\n"+invalid+"\n"), 0o600))
			_, err := LoadRepoConfig(root)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "goal_review")
		})
	}
}

func TestGoalReviewGatePolicyIsGlobalOnly(t *testing.T) {
	assert.Equal(t, "block", ResolveGoalReviewGate(nil))
	globalPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(globalPath, []byte("[goal_review.kata_gate]\ndefault = \"warn\"\n"), 0o600))
	global, err := LoadGlobalFrom(globalPath)
	require.NoError(t, err)
	assert.Equal(t, "warn", ResolveGoalReviewGate(global))
	assert.True(t, IsGlobalKey("goal_review.kata_gate.default"))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".roborev.toml"),
		[]byte("[goal_review.kata_gate]\ndefault = \"off\"\n"), 0o600))
	_, err = LoadRepoConfig(root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "global-only")
}

func TestGoalReviewGateGlobalValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[goal_review.kata_gate]\ndefault = \"allow\"\n"), 0o600))
	_, err := LoadGlobalFrom(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "goal_review.kata_gate.default")
}

func TestGoalReviewWorkflow(t *testing.T) {
	assert.Equal(t, "review", WorkflowForReviewType(ReviewTypeGoal))
	_, err := ValidateReviewTypes([]string{ReviewTypeGoal})
	require.Error(t, err, "commit-based matrices must not accept artifact reviews")
	assert.NotContains(t, ExplicitReviewTypes(), ReviewTypeGoal)
	assert.True(t, IsValidKey("goal_review.spec_file"))
	assert.True(t, IsValidKey("goal_review.plan_file"))
	assert.True(t, IsGlobalKey("goal_review.kata_gate.default"))
	assert.False(t, IsGlobalKey("goal_review.enabled"))
}
