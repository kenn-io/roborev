package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalReviewToolPolicy(t *testing.T) {
	got, err := CanonicalReviewToolPolicy(&ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{
		"research": {"read_source", "find-citation", "read_source"},
		"unused":   {},
	}})
	require.NoError(t, err)
	assert.Equal(t, &ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{
		"research": {"find-citation", "read_source"},
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
	for _, entry := range []string{"read", "sources/", "sources/*", "sources/read/extra"} {
		cfg.ReviewMCPAllowedTools = []string{entry}
		require.Error(t, cfg.Validate(), entry)
	}
}

func TestExperimentOverlayRejectsReviewTools(t *testing.T) {
	err := validateExperimentOverlay(map[string]any{"review": map[string]any{
		"subagents": map[string]any{"checker": map[string]any{"tools": map[string]any{"mode": "read-only"}}},
	}})
	require.ErrorContains(t, err, "review.subagents.checker.tools")
}
