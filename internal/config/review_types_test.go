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
