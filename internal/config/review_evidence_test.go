package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvidenceManifestSettingsResolution(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, ".roborev.toml", `[review.types.citations]
template = "rubric.md"
evidence_manifest = "evidence/manifest.toml"
[review.subagents.sources]
review_type = "citations"
[review.panels.drafts]
members = ["sources"]
`)
	repo, err := LoadRepoConfig(dir)
	require.NoError(t, err)
	settings, err := ResolveReviewSettings("citations", nil, repo, nil)
	require.NoError(t, err)
	assert.Equal(t, ReviewSettings{EvidenceManifest: "evidence/manifest.toml"}, settings, "a manifest does not require binary evidence")
	members, _, err := ResolveCIPanel("drafts", repo, nil)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, settings, members[0].ReviewSettings)
	repo.Review.Types["citations"] = ReviewTypeSpec{Template: "rubric.md"}
	current, err := ResolveReviewSettings("citations", nil, repo, nil)
	require.NoError(t, err)
	assert.Equal(t, ReviewSettings{}, current)
	global := DefaultConfig()
	global.Review.Types = map[string]ReviewTypeSpec{"citations": {Template: "global.md", BinaryEvidence: true, EvidenceManifest: "required.toml"}}
	current, err = ResolveReviewSettings("citations", nil, repo, global)
	require.NoError(t, err)
	assert.Equal(t, ReviewSettings{}, current, "repository definition replaces global evidence requirements")
}
