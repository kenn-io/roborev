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

func TestLoadRepoConfigWithRawPreservesExplicitZeroValues(t *testing.T) {
	dir := t.TempDir()
	writeRepoConfigStr(t, dir, `
reuse_review_session = false
reuse_review_session_lookback = 0
`)

	cfg, raw, err := LoadRepoConfigWithRaw(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.ReuseReviewSession)
	assert.False(t, *cfg.ReuseReviewSession)
	assert.Equal(t, 0, cfg.ReuseReviewSessionLookback)
	assert.True(t, IsKeyInTOMLFile(raw, "reuse_review_session"))
	assert.True(t, IsKeyInTOMLFile(raw, "reuse_review_session_lookback"))
}

func TestSelectReviewExperimentNamespacesCISourceRepository(t *testing.T) {
	enabled := true
	ratio := 1.0
	input := ExperimentSelectionInput{
		Workflow: ExperimentWorkflowCI,
		Subject: ExperimentSubject{
			Repository: "github.com/example/project",
			SourceRepo: "github.com/alice/project",
			Branch:     "feature",
		},
		Global: &Config{Experiments: map[string]ExperimentDefinition{
			"ci-v1": {
				Enabled: &enabled, Ratio: &ratio,
				Workflows: []ExperimentWorkflow{ExperimentWorkflowCI},
				Config:    map[string]any{"reuse_review_session": true},
			},
		}},
	}
	first, err := SelectReviewExperiment(input)
	require.NoError(t, err)
	require.NotNil(t, first.Assignment)

	input.Subject.SourceRepo = "github.com/bob/project"
	second, err := SelectReviewExperiment(input)
	require.NoError(t, err)
	require.NotNil(t, second.Assignment)
	assert.NotEqual(t, first.Assignment.SubjectHash, second.Assignment.SubjectHash)
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

func TestSelectReviewExperimentDefaultArmPreservesConfigLayers(t *testing.T) {
	enabled := true
	ratio := 0.0
	selection, err := SelectReviewExperiment(ExperimentSelectionInput{
		Workflow: ExperimentWorkflowReview,
		Subject: ExperimentSubject{
			Repository: "github.com/example/project", Branch: "feature",
		},
		Global: &Config{
			ReviewAgent: "codex",
			Experiments: map[string]ExperimentDefinition{
				"baseline-v1": {
					Enabled: &enabled, Ratio: &ratio,
					Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
					Config:    map[string]any{"reuse_review_session": true},
				},
			},
		},
		Repo:    &RepoConfig{Agent: "claude-code"},
		RawRepo: map[string]any{"agent": "claude-code"},
	})
	require.NoError(t, err)
	require.NotNil(t, selection.RepoConfig)
	assert.Equal(t, "claude-code", selection.RepoConfig.Agent)
	assert.Empty(t, selection.RepoConfig.ReviewAgent)
}

func TestSelectReviewExperimentTreatmentPreservesUnrelatedConfigLayers(t *testing.T) {
	enabled := true
	ratio := 1.0
	selection, err := SelectReviewExperiment(ExperimentSelectionInput{
		Workflow: ExperimentWorkflowReview,
		Subject: ExperimentSubject{
			Repository: "github.com/example/project", Branch: "feature",
		},
		Global: &Config{
			ReviewAgent: "codex",
			Experiments: map[string]ExperimentDefinition{
				"session-v1": {
					Enabled: &enabled, Ratio: &ratio,
					Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
					Config:    map[string]any{"reuse_review_session": true},
				},
			},
		},
		Repo:    &RepoConfig{Agent: "claude-code"},
		RawRepo: map[string]any{"agent": "claude-code"},
	})
	require.NoError(t, err)
	require.NotNil(t, selection.RepoConfig)
	assert.Equal(t, "claude-code", selection.RepoConfig.Agent)
	assert.Empty(t, selection.RepoConfig.ReviewAgent)
	require.NotNil(t, selection.RepoConfig.ReuseReviewSession)
	assert.True(t, *selection.RepoConfig.ReuseReviewSession)
}

func TestSelectReviewExperimentDefaultArmPreservesRepoCIReviewsReplacement(t *testing.T) {
	enabled := true
	ratio := 0.0
	selection, err := SelectReviewExperiment(ExperimentSelectionInput{
		Workflow: ExperimentWorkflowCI,
		Subject: ExperimentSubject{
			Repository: "github.com/example/project",
			SourceRepo: "github.com/example/project",
			Branch:     "feature",
		},
		Global: &Config{
			CI: CIConfig{Reviews: map[string][]string{"codex": {"security"}}},
			Experiments: map[string]ExperimentDefinition{
				"baseline-v1": {
					Enabled: &enabled, Ratio: &ratio,
					Workflows: []ExperimentWorkflow{ExperimentWorkflowCI},
					Config: map[string]any{"ci": map[string]any{
						"reasoning": "high",
					}},
				},
			},
		},
		Repo: &RepoConfig{CI: RepoCIConfig{Reviews: map[string][]string{}}},
		RawRepo: map[string]any{"ci": map[string]any{
			"reviews": map[string]any{},
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, selection.RepoConfig)
	assert.NotNil(t, selection.RepoConfig.CI.Reviews)
	assert.Empty(t, selection.RepoConfig.CI.Reviews)
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
					"ci":     map[string]any{"agents": []any{"codex", "test"}},
					"review": map[string]any{"default_panel": "experiment"},
				},
			},
		}},
		Repo: &RepoConfig{},
		RawRepo: map[string]any{
			"review_guidelines": "base guidance",
			"ci":                map[string]any{"agents": []any{"claude"}},
			"review": map[string]any{
				"hook_review_panel": "hooks",
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, selection.RepoConfig)
	require.NotNil(t, selection.RawRepoConfig)
	assert.Equal(t, "base guidance", selection.RepoConfig.ReviewGuidelines)
	assert.Equal(t, []string{"codex", "test"}, selection.RepoConfig.CI.Agents)
	assert.Equal(t, "experiment", selection.RepoConfig.Review.DefaultPanel)
	assert.Equal(t, "hooks", selection.RepoConfig.Review.HookPanel)
}

func TestSelectReviewExperimentMergesOverGlobalNestedConfig(t *testing.T) {
	enabled := true
	ratio := 1.0
	selection, err := SelectReviewExperiment(ExperimentSelectionInput{
		Workflow: ExperimentWorkflowReview,
		Subject: ExperimentSubject{
			Repository: "github.com/example/project", Branch: "feature",
		},
		Global: &Config{
			Review: ReviewConfig{Subagents: map[string]SubagentSpec{
				"bugs": {Agent: "codex", Model: "base-model", Reasoning: "high"},
			}},
			Experiments: map[string]ExperimentDefinition{
				"model-v1": {
					Enabled: &enabled, Ratio: &ratio,
					Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
					Config: map[string]any{"review": map[string]any{
						"subagents": map[string]any{"bugs": map[string]any{"model": "experiment-model"}},
					}},
				},
			},
		},
	})
	require.NoError(t, err)
	bugs := selection.RepoConfig.Review.Subagents["bugs"]
	assert.Equal(t, "codex", bugs.Agent)
	assert.Equal(t, "experiment-model", bugs.Model)
	assert.Equal(t, "high", bugs.Reasoning)
}

func TestSelectReviewExperimentPreservesRepoReplacementBeforeOverlay(t *testing.T) {
	enabled := true
	ratio := 1.0
	selection, err := SelectReviewExperiment(ExperimentSelectionInput{
		Workflow: ExperimentWorkflowReview,
		Subject: ExperimentSubject{
			Repository: "github.com/example/project", Branch: "feature",
		},
		Global: &Config{
			Review: ReviewConfig{Subagents: map[string]SubagentSpec{
				"bugs": {Agent: "codex", Model: "global-model", Reasoning: "high"},
			}},
			Experiments: map[string]ExperimentDefinition{
				"provider-v1": {
					Enabled: &enabled, Ratio: &ratio,
					Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
					Config: map[string]any{"review": map[string]any{
						"subagents": map[string]any{"bugs": map[string]any{"provider": "openai"}},
					}},
				},
			},
		},
		Repo: &RepoConfig{Review: ReviewConfig{Subagents: map[string]SubagentSpec{
			"bugs": {Model: "repo-model"},
		}}},
		RawRepo: map[string]any{"review": map[string]any{
			"subagents": map[string]any{"bugs": map[string]any{"model": "repo-model"}},
		}},
	})
	require.NoError(t, err)
	bugs := selection.RepoConfig.Review.Subagents["bugs"]
	assert.Empty(t, bugs.Agent)
	assert.Equal(t, "repo-model", bugs.Model)
	assert.Equal(t, "openai", bugs.Provider)
	assert.Empty(t, bugs.Reasoning)
}

func TestSelectReviewExperimentRejectsNonTableReviewEntries(t *testing.T) {
	enabled := true
	ratio := 1.0
	baseReview := ReviewConfig{
		Subagents: map[string]SubagentSpec{
			"bugs": {Agent: "codex"},
		},
		Panels: map[string]PanelSpec{
			"team": {Members: []string{"bugs"}},
		},
	}

	for _, tt := range []struct {
		name      string
		key       string
		entryName string
	}{
		{name: "subagent", key: "subagents", entryName: "bugs"},
		{name: "panel", key: "panels", entryName: "team"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SelectReviewExperiment(ExperimentSelectionInput{
				Workflow: ExperimentWorkflowReview,
				Subject: ExperimentSubject{
					Repository: "github.com/example/project", Branch: "feature",
				},
				Global: &Config{
					Review: baseReview,
					Experiments: map[string]ExperimentDefinition{
						"invalid-v1": {
							Enabled: &enabled, Ratio: &ratio,
							Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
							Config: map[string]any{"review": map[string]any{
								tt.key: map[string]any{tt.entryName: "not-a-table"},
							}},
						},
					},
				},
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "review."+tt.key+"."+tt.entryName+" must be a table")
		})
	}
}

func TestSelectReviewExperimentDefaultArmPreservesRepoReplacement(t *testing.T) {
	enabled := true
	ratio := 0.0
	selection, err := SelectReviewExperiment(ExperimentSelectionInput{
		Workflow: ExperimentWorkflowReview,
		Subject: ExperimentSubject{
			Repository: "github.com/example/project", Branch: "feature",
		},
		Global: &Config{
			Review: ReviewConfig{Subagents: map[string]SubagentSpec{
				"bugs": {Agent: "codex", Model: "global-model", Reasoning: "high"},
			}},
			Experiments: map[string]ExperimentDefinition{
				"baseline-v1": {
					Enabled: &enabled, Ratio: &ratio,
					Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
					Config:    map[string]any{"reuse_review_session": true},
				},
			},
		},
		Repo: &RepoConfig{Review: ReviewConfig{Subagents: map[string]SubagentSpec{
			"bugs": {Model: "repo-model"},
		}}},
		RawRepo: map[string]any{"review": map[string]any{
			"subagents": map[string]any{"bugs": map[string]any{"model": "repo-model"}},
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, selection.Assignment)
	assert.Equal(t, ExperimentArmDefault, selection.Assignment.Arm)
	assert.Equal(t, SubagentSpec{Model: "repo-model"}, selection.RepoConfig.Review.Subagents["bugs"])
}

func TestSelectReviewExperimentPreservesExplicitZeroPresence(t *testing.T) {
	enabled := true
	ratio := 1.0
	selection, err := SelectReviewExperiment(ExperimentSelectionInput{
		Workflow: ExperimentWorkflowReview,
		Subject: ExperimentSubject{
			Repository: "github.com/example/project", Branch: "feature",
		},
		Global: &Config{
			ReuseReviewSessionLookback: 5,
			Experiments: map[string]ExperimentDefinition{
				"lookback-v1": {
					Enabled: &enabled, Ratio: &ratio,
					Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
					Config:    map[string]any{"reuse_review_session_lookback": int64(0)},
				},
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, selection.RepoConfig.ReuseReviewSessionLookback)
	assert.True(t, IsKeyInTOMLFile(selection.RawRepoConfig, "reuse_review_session_lookback"))
}

func TestSelectReviewExperimentRequiresRawRepositoryConfigForOverlay(t *testing.T) {
	enabled := true
	ratio := 1.0
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
		Repo: &RepoConfig{ReviewGuidelines: "repository guidance"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing its paired raw representation")
}

func TestSelectReviewExperimentRejectsUnknownNestedKey(t *testing.T) {
	enabled := true
	ratio := 1.0
	_, err := SelectReviewExperiment(ExperimentSelectionInput{
		Workflow: ExperimentWorkflowReview,
		Subject: ExperimentSubject{
			Repository: "github.com/example/project", Branch: "feature",
		},
		Global: &Config{Experiments: map[string]ExperimentDefinition{
			"typo-v1": {
				Enabled: &enabled, Ratio: &ratio,
				Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
				Config: map[string]any{"review": map[string]any{
					"default_pnael": "typo",
				}},
			},
		}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strict mode")
}

func TestSelectReviewExperimentValidatesOverlayForDefaultArm(t *testing.T) {
	enabled := true
	ratio := 0.0
	_, err := SelectReviewExperiment(ExperimentSelectionInput{
		Workflow: ExperimentWorkflowReview,
		Subject: ExperimentSubject{
			Repository: "github.com/example/project", Branch: "feature",
		},
		Global: &Config{Experiments: map[string]ExperimentDefinition{
			"invalid-control-v1": {
				Enabled: &enabled, Ratio: &ratio,
				Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
				Config: map[string]any{"review": map[string]any{
					"default_pnael": "typo",
				}},
			},
		}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strict mode")
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
		"unsupported-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
			Config:    map[string]any{"review_guidelines": "not frozen at enqueue"},
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
