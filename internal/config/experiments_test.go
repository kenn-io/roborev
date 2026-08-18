package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectReviewExperimentIsStableForBranch(t *testing.T) {
	enabled := true
	ratio := 0.5
	global := &Config{Experiments: map[string]ExperimentDefinition{
		"session-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
			Config:    map[string]any{"reuse_review_session": true},
		},
	}}
	input := ExperimentSelectionInput{
		Workflow: ExperimentWorkflowReview,
		Subject: ExperimentSubject{
			Repository: "github.com/example/project", Branch: "feature-a",
		},
		Global: global,
	}

	first, err := SelectReviewExperiment(input)
	require.NoError(t, err)
	second, err := SelectReviewExperiment(input)
	require.NoError(t, err)
	require.NotNil(t, first.Assignment)
	require.NotNil(t, second.Assignment)
	assert.Equal(t, first.Assignment, second.Assignment)

	input.Subject.Branch = "feature-b"
	third, err := SelectReviewExperiment(input)
	require.NoError(t, err)
	require.NotNil(t, third.Assignment)
	assert.NotEqual(t, first.Assignment.SubjectHash, third.Assignment.SubjectHash)
}

func TestLoadRepoConfigExperimentDefinition(t *testing.T) {
	dir := t.TempDir()
	writeRepoConfigStr(t, dir, `
[experiments.session-v1]
enabled = true
ratio = 1.0
workflows = ["review"]

[experiments.session-v1.config]
reuse_review_session = true
`)

	cfg, err := LoadRepoConfig(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	definition := cfg.Experiments["session-v1"]
	require.NotNil(t, definition.Enabled)
	require.NotNil(t, definition.Ratio)
	assert.True(t, *definition.Enabled)
	assert.InDelta(t, 1.0, *definition.Ratio, 0)
	assert.Equal(t, []ExperimentWorkflow{ExperimentWorkflowReview}, definition.Workflows)
	assert.Equal(t, true, definition.Config["reuse_review_session"])
}

func TestSelectReviewExperimentHonorsRatioBoundaries(t *testing.T) {
	enabled := true
	for _, tc := range []struct {
		name  string
		ratio float64
		arm   ExperimentArm
	}{
		{name: "all default", ratio: 0, arm: ExperimentArmDefault},
		{name: "all experimental", ratio: 1, arm: ExperimentArmExperimental},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selection, err := SelectReviewExperiment(ExperimentSelectionInput{
				Workflow: ExperimentWorkflowReview,
				Subject: ExperimentSubject{
					Repository: "github.com/example/project", Branch: "feature",
				},
				Global: &Config{Experiments: map[string]ExperimentDefinition{
					"boundary-v1": {
						Enabled: &enabled, Ratio: &tc.ratio,
						Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
						Config:    map[string]any{"reuse_review_session": true},
					},
				}},
			})
			require.NoError(t, err)
			require.NotNil(t, selection.Assignment)
			assert.Equal(t, tc.arm, selection.Assignment.Arm)
		})
	}
}

func TestSelectReviewExperimentRequiresBranchSubject(t *testing.T) {
	enabled := true
	ratio := 1.0
	selection, err := SelectReviewExperiment(ExperimentSelectionInput{
		Workflow: ExperimentWorkflowReview,
		Subject:  ExperimentSubject{Repository: "github.com/example/project"},
		Global: &Config{Experiments: map[string]ExperimentDefinition{
			"session-v1": {
				Enabled: &enabled, Ratio: &ratio,
				Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
				Config:    map[string]any{"reuse_review_session": true},
			},
		}},
	})
	require.NoError(t, err)
	assert.Nil(t, selection.Assignment)
}

func TestSelectReviewExperimentMergesOverlayRecursively(t *testing.T) {
	enabled := true
	ratio := 1.0
	selection, err := SelectReviewExperiment(ExperimentSelectionInput{
		Workflow: ExperimentWorkflowReview,
		Subject: ExperimentSubject{
			Repository: "github.com/example/project", Branch: "feature",
		},
		Global: &Config{Experiments: map[string]ExperimentDefinition{
			"panel-v1": {
				Enabled: &enabled, Ratio: &ratio,
				Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
				Config: map[string]any{
					"exclude_patterns": []any{"generated/**"},
					"review":           map[string]any{"default_panel": "experiment"},
				},
			},
		}},
		Repo: &RepoConfig{},
		RawRepo: map[string]any{
			"review_guidelines": "base guidance",
			"exclude_patterns":  []any{"vendor/**"},
			"review": map[string]any{
				"hook_review_panel": "hooks",
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, selection.RepoConfig)
	assert.Equal(t, "base guidance", selection.RepoConfig.ReviewGuidelines)
	assert.Equal(t, []string{"generated/**"}, selection.RepoConfig.ExcludePatterns)
	assert.Equal(t, "experiment", selection.RepoConfig.Review.DefaultPanel)
	assert.Equal(t, "hooks", selection.RepoConfig.Review.HookPanel)
}

func TestSelectReviewExperimentRejectsDefinitionMutation(t *testing.T) {
	enabled := true
	disabled := false
	ratio := 0.5
	changedRatio := 0.75
	_, err := SelectReviewExperiment(ExperimentSelectionInput{
		Workflow: ExperimentWorkflowReview,
		Subject: ExperimentSubject{
			Repository: "github.com/example/project", Branch: "feature",
		},
		Global: &Config{Experiments: map[string]ExperimentDefinition{
			"session-v1": {
				Enabled: &enabled, Ratio: &ratio,
				Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
				Config:    map[string]any{"reuse_review_session": true},
			},
		}},
		Repo: &RepoConfig{Experiments: map[string]ExperimentDefinition{
			"session-v1": {Enabled: &disabled, Ratio: &changedRatio},
		}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "may override only enabled")
}

func TestValidateExperimentEntriesRejectsUnsafeAndOverlappingConfig(t *testing.T) {
	enabled := true
	ratio := 0.5
	err := validateExperimentEntries(map[string]ExperimentDefinition{
		"unsafe-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
			Config:    map[string]any{"server_addr": "127.0.0.1:9999"},
		},
	}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a review-time configuration setting")

	err = validateExperimentEntries(map[string]ExperimentDefinition{
		"one-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
			Config:    map[string]any{"reuse_review_session": true},
		},
		"two-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
			Config:    map[string]any{"review_reasoning": "high"},
		},
	}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both apply to workflow")
}
