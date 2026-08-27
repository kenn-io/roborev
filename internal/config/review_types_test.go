package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateReviewTypesFromConfig(t *testing.T) {
	repoCfg := &RepoConfig{Review: ReviewConfig{Types: map[string]ReviewTypeSpec{
		"thermonuclear": {Template: "review.tmpl"},
	}}}

	got, err := ValidateReviewTypesFromConfig(
		[]string{"review", "thermonuclear", "thermonuclear"},
		repoCfg, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"default", "thermonuclear"}, got)
}

func TestConfigValidateAllowsGlobalCustomCIReviewTypes(t *testing.T) {
	globalCfg := &Config{
		Review: ReviewConfig{Types: map[string]ReviewTypeSpec{
			"thermonuclear": {Template: "review.tmpl"},
		}},
		CI: CIConfig{
			ReviewTypes: []string{"thermonuclear"},
			Reviews: map[string][]string{
				"codex": {"thermonuclear"},
			},
		},
	}

	require.NoError(t, globalCfg.Validate())
}

func TestEffectiveConfigAllowsRepoCIToUseGlobalCustomReviewType(t *testing.T) {
	globalCfg := &Config{Review: ReviewConfig{Types: map[string]ReviewTypeSpec{
		"thermonuclear": {Template: "review.tmpl"},
	}}}
	repoCfg := &RepoConfig{CI: RepoCIConfig{
		ReviewTypes: []string{"thermonuclear"},
		Reviews: map[string][]string{
			"codex": {"thermonuclear"},
		},
	}}

	require.NoError(t, repoCfg.Validate())
	require.NoError(t, ValidateEffectiveReviewConfig(globalCfg, repoCfg))
}

func TestEffectiveConfigRejectsUnknownRepoCIReviewTypes(t *testing.T) {
	tests := []struct {
		name    string
		ci      RepoCIConfig
		wantErr string
	}{
		{
			name:    "flat list",
			ci:      RepoCIConfig{ReviewTypes: []string{"mystery"}},
			wantErr: "ci.review_types",
		},
		{
			name: "matrix",
			ci: RepoCIConfig{Reviews: map[string][]string{
				"codex": {"mystery"},
			}},
			wantErr: "ci.reviews.codex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoCfg := &RepoConfig{CI: tt.ci}

			require.NoError(t, repoCfg.Validate())
			err := ValidateEffectiveReviewConfig(nil, repoCfg)
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestReviewTypeDefinitionsValidateNamesAndTemplates(t *testing.T) {
	tests := []struct {
		name    string
		types   map[string]ReviewTypeSpec
		wantErr string
	}{
		{
			name: "valid",
			types: map[string]ReviewTypeSpec{
				"api-contract": {Template: "review.tmpl"},
			},
		},
		{
			name: "reserved",
			types: map[string]ReviewTypeSpec{
				"security": {Template: "review.tmpl"},
			},
			wantErr: "reserved",
		},
		{
			name: "fix workflow reserved",
			types: map[string]ReviewTypeSpec{
				"fix": {Template: "review.tmpl"},
			},
			wantErr: "reserved",
		},
		{
			name: "refine workflow reserved",
			types: map[string]ReviewTypeSpec{
				"refine": {Template: "review.tmpl"},
			},
			wantErr: "reserved",
		},
		{
			name: "classify workflow reserved",
			types: map[string]ReviewTypeSpec{
				"classify": {Template: "review.tmpl"},
			},
			wantErr: "reserved",
		},
		{
			name: "missing template",
			types: map[string]ReviewTypeSpec{
				"api-contract": {},
			},
			wantErr: "has no template",
		},
		{
			name: "invalid include name",
			types: map[string]ReviewTypeSpec{
				"api-contract": {
					Template: "review.tmpl",
					Includes: map[string]string{"bad.key": "rubric.md"},
				},
			},
			wantErr: "invalid name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCustomReviewTypes(tt.types)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestMergeReviewConfigRepoTypeReplacesGlobalType(t *testing.T) {
	global := ReviewConfig{Types: map[string]ReviewTypeSpec{
		"thermonuclear": {
			Template: "global.tmpl",
			Includes: map[string]string{"rubric": "global.md"},
		},
	}}
	repo := ReviewConfig{Types: map[string]ReviewTypeSpec{
		"thermonuclear": {Template: "repo.tmpl"},
	}}

	merged := MergeReviewConfig(repo, global)
	assert.Equal(t, "repo.tmpl", merged.Types["thermonuclear"].Template)
	assert.Empty(t, merged.Types["thermonuclear"].Includes)
}

func TestResolveCIReviewReasoningForType(t *testing.T) {
	repoCfg := &RepoConfig{Review: ReviewConfig{Types: map[string]ReviewTypeSpec{
		"thermonuclear": {
			Template:  "review.tmpl",
			Reasoning: "maximum",
		},
	}}}

	got, err := ResolveCIReviewReasoningForType(
		"", repoCfg, nil, "thermonuclear",
	)
	require.NoError(t, err)
	assert.Equal(t, "maximum", got)

	repoCfg.CI.Reasoning = "standard"
	got, err = ResolveCIReviewReasoningForType(
		"", repoCfg, nil, "thermonuclear",
	)
	require.NoError(t, err)
	assert.Equal(t, "standard", got)

	got, err = ResolveCIReviewReasoningForType(
		"fast", repoCfg, nil, "thermonuclear",
	)
	require.NoError(t, err)
	assert.Equal(t, "fast", got)
}

func TestCustomReviewTypeWorkflowOverrides(t *testing.T) {
	globalCfg := &Config{Review: ReviewConfig{Types: map[string]ReviewTypeSpec{
		"thermonuclear": {
			Template: "global.tmpl",
			Agent:    "global-agent",
			Model:    "global-model",
		},
	}}}
	repoCfg := &RepoConfig{Review: ReviewConfig{Types: map[string]ReviewTypeSpec{
		"thermonuclear": {
			Template: "repo.tmpl",
			Agent:    "repo-agent",
			Model:    "repo-model",
		},
	}}}

	assert.True(t, HasWorkflowAgentOverrideFromConfig(
		repoCfg, globalCfg, "thermonuclear", "thorough",
	))
	assert.Equal(t, "repo-model", ResolveWorkflowModelFromConfig(
		repoCfg, globalCfg, "thermonuclear", "thorough",
	))

	repoCfg.Review.Types["thermonuclear"] = ReviewTypeSpec{
		Template: "repo.tmpl",
	}
	assert.True(t, HasWorkflowAgentOverrideFromConfig(
		repoCfg, globalCfg, "thermonuclear", "thorough",
	))
	assert.Equal(t, "global-model", ResolveWorkflowModelFromConfig(
		repoCfg, globalCfg, "thermonuclear", "thorough",
	))
	assert.Equal(t, "global-agent", ResolveAgentForWorkflowFromConfig(
		"", repoCfg, globalCfg, "thermonuclear", "thorough",
	))
	assert.Equal(t, "global-model", ResolveModelForWorkflowFromConfig(
		"", repoCfg, globalCfg, "thermonuclear", "thorough",
	))
}
