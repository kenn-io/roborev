package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalReviewToolPolicy(t *testing.T) {
	got, err := CanonicalReviewToolPolicy(&ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{
		"research": {"read_source", "find-citation", "read_source"},
		"_archive": {"lookup_"},
		"unused":   {},
	}})
	require.NoError(t, err)
	assert.Equal(t, &ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{
		"research": {"find-citation", "read_source"},
		"_archive": {"lookup_"},
	}}, got)

	got, err = CanonicalReviewToolPolicy(nil)
	require.NoError(t, err)
	assert.Nil(t, got, "absence never becomes a strict policy")

	for name, policy := range map[string]ReviewToolPolicy{
		"missing mode":     {},
		"wrong mode":       {Mode: "read-write"},
		"wildcard tool":    {Mode: "read-only", MCP: map[string][]string{"source": {"*"}}},
		"slash in tool":    {Mode: "read-only", MCP: map[string][]string{"source": {"a/b"}}},
		"separator":        {Mode: "read-only", MCP: map[string][]string{"source": {"a__b"}}},
		"space in server":  {Mode: "read-only", MCP: map[string][]string{"my source": {"read"}}},
		"empty server":     {Mode: "read-only", MCP: map[string][]string{"": {"read"}}},
		"bad server alone": {Mode: "read-only", MCP: map[string][]string{"a.b": {}}},
		"server ends in _": {Mode: "read-only", MCP: map[string][]string{"archive_": {"lookup"}}},
		"tool starts in _": {Mode: "read-only", MCP: map[string][]string{"archive": {"_lookup"}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CanonicalReviewToolPolicy(&policy)
			require.Error(t, err)
		})
	}
}

func TestResolveReviewSettingsMemberReplacesTypePolicy(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, ".roborev.toml", `
[review.types.citations]
template = "rubric.md"

[review.types.citations.tools]
mode = "read-only"

[review.types.citations.tools.mcp]
sources = ["read", "find"]

[review.subagents.checker]
review_type = "citations"

[review.subagents.checker.tools]
mode = "read-only"

[review.subagents.inherits]
review_type = "citations"

[review.panels.audit]
members = ["checker", "inherits"]
`)
	repoCfg, err := LoadRepoConfig(dir)
	require.NoError(t, err)

	settings, err := ResolveReviewSettings("citations", nil, repoCfg, nil)
	require.NoError(t, err)
	assert.Equal(t, &ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"sources": {"find", "read"}}}, settings.Tools)

	settings, err = ResolveReviewSettings("default", nil, repoCfg, nil)
	require.NoError(t, err)
	assert.Nil(t, settings.Tools)

	members, _, err := ResolveCIPanel("audit", repoCfg, nil)
	require.NoError(t, err)
	require.Len(t, members, 2)
	assert.Equal(t, &ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{}}, members[0].Tools,
		"a member policy replaces the type policy instead of merging grants")
	assert.Equal(t, map[string][]string{"sources": {"find", "read"}}, members[1].Tools.MCP)
}

func TestReviewMCPAuthorityIsGlobalOnly(t *testing.T) {
	for _, body := range []string{
		"review_mcp_config = '/etc/roborev/mcp.json'",
		"review_mcp_allowed_tools = ['sources/read']",
	} {
		dir := t.TempDir()
		writeTestFile(t, dir, ".roborev.toml", body)
		_, err := LoadRepoConfig(dir)
		require.ErrorContains(t, err, "global-only")
	}

	cfg := DefaultConfig()
	cfg.ReviewMCPAllowedTools = []string{"sources/read"}
	require.NoError(t, cfg.Validate())
	for _, entry := range []string{"read", "sources/", "sources/*", "sources/read/extra", "sources_/read"} {
		cfg.ReviewMCPAllowedTools = []string{entry}
		require.Error(t, cfg.Validate(), entry)
	}
}

func TestExperimentReviewTypeToolPolicy(t *testing.T) {
	baseType := ReviewTypeSpec{Template: "rubric.md", Tools: &ReviewToolPolicy{
		Mode: "read-only", MCP: map[string][]string{"sources": {"find", "read"}},
	}}
	rawBaseType := map[string]any{"template": "rubric.md", "tools": map[string]any{
		"mode": "read-only", "mcp": map[string]any{"sources": []any{"find", "read"}},
	}}
	for name, tc := range map[string]struct {
		repoBase bool
		overlay  map[string]any
		want     *ReviewToolPolicy
	}{
		"override of a global type without tools keeps its policy": {
			overlay: map[string]any{"template": "rubric-v2.md", "model": "experiment-model"},
			want:    baseType.Tools,
		},
		"experiment tools replace a repository type's policy": {
			repoBase: true,
			overlay: map[string]any{"template": "rubric-v2.md", "tools": map[string]any{
				"mode": "read-only", "mcp": map[string]any{},
			}},
			want: &ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			enabled, ratio := true, 1.0
			global := &Config{
				Review: ReviewConfig{
					Subagents: map[string]SubagentSpec{"checker": {ReviewType: "citations"}},
					Panels:    map[string]PanelSpec{"audit": {Members: []string{"checker"}}},
				},
				Experiments: map[string]ExperimentDefinition{"rubric-v2": {
					Enabled: &enabled, Ratio: &ratio,
					Workflows: []ExperimentWorkflow{ExperimentWorkflowReview},
					Config: map[string]any{"review": map[string]any{
						"types": map[string]any{"citations": tc.overlay},
					}},
				}},
			}
			input := ExperimentSelectionInput{
				Workflow: ExperimentWorkflowReview,
				Subject:  ExperimentSubject{Repository: "github.com/example/project", Branch: "feature"},
				Global:   global,
			}
			if tc.repoBase {
				input.Repo = &RepoConfig{Review: ReviewConfig{Types: map[string]ReviewTypeSpec{"citations": baseType}}}
				input.RawRepo = map[string]any{"review": map[string]any{"types": map[string]any{"citations": rawBaseType}}}
			} else {
				global.Review.Types = map[string]ReviewTypeSpec{"citations": baseType}
			}
			selection, err := SelectReviewExperiment(input)
			require.NoError(t, err)
			require.NotNil(t, selection.Assignment)
			require.Equal(t, ExperimentArmExperimental, selection.Assignment.Arm)

			resolved, ok := ResolveCustomReviewTypeFromConfig("citations", selection.RepoConfig, global)
			require.True(t, ok)
			assert.Equal(t, "rubric-v2.md", resolved.Spec.Template)
			settings, err := ResolveReviewSettings("citations", nil, selection.RepoConfig, global)
			require.NoError(t, err)
			assert.Equal(t, tc.want, settings.Tools)

			members, _, err := ResolveCIPanel("audit", selection.RepoConfig, global)
			require.NoError(t, err)
			require.Len(t, members, 1)
			assert.Equal(t, tc.want, members[0].Tools)
		})
	}
}
