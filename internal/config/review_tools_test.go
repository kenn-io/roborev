package config

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewToolPolicyCanonicalization(t *testing.T) {
	p, err := CanonicalReviewToolPolicy(nil)
	require.NoError(t, err)
	assert.Nil(t, p)
	input := &ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{
		"research":   {"read_source", "find-citation", "read_source"},
		"unused":     {},
		"nil_grants": nil,
	}}
	p, err = CanonicalReviewToolPolicy(input)
	require.NoError(t, err)
	assert.Equal(t, &ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{
		"research": {"find-citation", "read_source"},
	}}, p)
	assert.Equal(t, []string{"read_source", "find-citation", "read_source"}, input.MCP["research"])
	p.MCP["research"][0] = "changed"
	p.MCP["other"] = []string{"read"}
	assert.Equal(t, []string{"read_source", "find-citation", "read_source"}, input.MCP["research"])
	assert.NotContains(t, input.MCP, "other")
	assert.Contains(t, input.MCP, "unused")
	assert.Contains(t, input.MCP, "nil_grants")

	p, err = CanonicalReviewToolPolicy(&ReviewToolPolicy{Mode: "read-only"})
	require.NoError(t, err)
	assert.Equal(t, &ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{}}, p)
}

func TestReviewToolPolicyRejectsInvalid(t *testing.T) {
	for _, mode := range []string{"", "read-write", "READ-ONLY", " read-only"} {
		t.Run("mode="+mode, func(t *testing.T) {
			_, err := CanonicalReviewToolPolicy(&ReviewToolPolicy{Mode: mode})
			require.ErrorContains(t, err, "mode")
		})
	}
	for _, name := range []string{"", "*", "a/b", "a__b", "a b", "a\tb", "a.b", "a\x00b", "é", "a\\b"} {
		t.Run("name="+name, func(t *testing.T) {
			_, err := CanonicalReviewToolPolicy(&ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{name: {"read"}}})
			require.Error(t, err)
			_, err = CanonicalReviewToolPolicy(&ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{name: {}}})
			require.Error(t, err, "empty grants do not exempt an invalid server name")
			_, err = CanonicalReviewToolPolicy(&ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"source": {name}}})
			require.Error(t, err)
		})
	}
	_, err := CanonicalReviewToolPolicy(&ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{
		"A-0_b": {"_read", "READ-2", strings.Repeat("x", 1024)},
	}})
	require.NoError(t, err)
}

func TestReviewToolPolicyPresence(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", `{"binary_evidence":false,"evidence_manifest":""}`},
		{"[tools]\nmode = 'read-only'\nmcp = {}\n", `{"tools":{"mode":"read-only","mcp":{}},"binary_evidence":false,"evidence_manifest":""}`},
	} {
		var settings ReviewSettings
		_, err := toml.Decode(tc.input, &settings)
		require.NoError(t, err)
		settings.Tools, err = CanonicalReviewToolPolicy(settings.Tools)
		require.NoError(t, err)
		data, err := json.Marshal(settings)
		require.NoError(t, err)
		assert.JSONEq(t, tc.want, string(data))
	}
}

func TestReviewToolPolicySettings(t *testing.T) {
	global := &Config{Review: ReviewConfig{Types: map[string]ReviewTypeSpec{
		"quotes": {Template: "quotes.md", Tools: &ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"source": {"read"}}}, BinaryEvidence: true, EvidenceManifest: "evidence.toml"},
	}}}
	settings, err := ResolveReviewSettings("quotes", nil, nil, global)
	require.NoError(t, err)
	assert.Equal(t, ReviewSettings{Tools: &ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"source": {"read"}}}, BinaryEvidence: true, EvidenceManifest: "evidence.toml"}, settings)
	settings.Tools.MCP["source"][0] = "changed"
	assert.Equal(t, []string{"read"}, global.Review.Types["quotes"].Tools.MCP["source"])
	settings, err = ResolveReviewSettings("quotes", &ReviewToolPolicy{Mode: "read-only"}, nil, global)
	require.NoError(t, err)
	assert.Empty(t, settings.Tools.MCP)
	assert.True(t, settings.BinaryEvidence)
	repo := &RepoConfig{Review: ReviewConfig{Types: map[string]ReviewTypeSpec{"quotes": {Template: "repo.md"}}}}
	settings, err = ResolveReviewSettings("quotes", nil, repo, global)
	require.NoError(t, err)
	assert.Equal(t, ReviewSettings{}, settings, "repository definition replaces the whole global type")
	_, err = ResolveReviewSettings("missing", nil, repo, global)
	require.Error(t, err)
	settings, err = ResolveReviewSettings("default", nil, repo, global)
	require.NoError(t, err)
	assert.Equal(t, ReviewSettings{}, settings)

	repo.Review.Subagents = map[string]SubagentSpec{"reviewer": {ReviewType: "quotes", Tools: &ReviewToolPolicy{Mode: "read-only"}}}
	repo.Review.Panels = map[string]PanelSpec{"panel": {Members: []string{"reviewer"}}}
	members, _, err := ResolveCIPanel("panel", repo, global)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.NotNil(t, members[0].Tools)
	assert.Equal(t, "read-only", members[0].Tools.Mode)
	cloned := MergeReviewConfig(repo.Review, global.Review)
	cloned.Subagents["reviewer"].Tools.Mode = "changed"
	assert.Equal(t, "read-only", repo.Review.Subagents["reviewer"].Tools.Mode)
	cloned = MergeReviewConfig(ReviewConfig{}, global.Review)
	cloned.Types["quotes"].Tools.MCP["source"][0] = "changed"
	assert.Equal(t, []string{"read"}, global.Review.Types["quotes"].Tools.MCP["source"])
}

func TestReviewToolPolicyAuthority(t *testing.T) {
	for _, entry := range []string{"read", "source/", "/read", "source/read/extra", "source/*", "source/a__b", "source /read"} {
		cfg := DefaultConfig()
		cfg.ReviewMCPAllowedTools = []string{entry}
		require.Error(t, cfg.Validate(), entry)
	}
	cfg := DefaultConfig()
	cfg.ReviewMCPAllowedTools = []string{"source/read", "source/read"}
	require.NoError(t, cfg.Validate())
	for _, body := range []string{"review_mcp_config = 'servers.json'", "review_mcp_allowed_tools = ['source/read']"} {
		dir := t.TempDir()
		writeTestFile(t, dir, ".roborev.toml", body)
		_, err := LoadRepoConfig(dir)
		require.ErrorContains(t, err, "global-only")
	}
}

func TestReviewToolPolicyTrustedConfig(t *testing.T) {
	dir := t.TempDir()
	execGit(t, dir, "init")
	execGit(t, dir, "config", "user.email", "test@example.com")
	execGit(t, dir, "config", "user.name", "Test")
	execGit(t, dir, "commit", "--allow-empty", "-m", "base")
	base := execGit(t, dir, "rev-parse", "HEAD")
	writeTestFile(t, dir, ".roborev.toml", "agent = 'test'\n")
	loaded, err := LoadTrustedRepoConfig(dir, base)
	require.NoError(t, err)
	assert.Nil(t, loaded.Config)
	assert.Nil(t, loaded.Raw)
	assert.Equal(t, base, loaded.Ref)
	_, err = LoadTrustedRepoConfig(dir, "")
	require.Error(t, err)
	_, err = LoadTrustedRepoConfig(dir, "missing-trusted-ref")
	require.Error(t, err)
	writeTestFile(t, dir, ".roborev.toml", "[broken\n")
	execGit(t, dir, "add", ".roborev.toml")
	execGit(t, dir, "commit", "-m", "invalid trusted configuration")
	trusted := execGit(t, dir, "rev-parse", "HEAD")
	writeTestFile(t, dir, ".roborev.toml", "agent = 'test'\n")
	loaded, err = LoadTrustedRepoConfig(dir, trusted)
	require.Error(t, err)
	assert.Equal(t, trusted, loaded.Ref)
	assert.Nil(t, loaded.Config)
}

func TestReviewToolPolicyTrustedConfigRequiresTree(t *testing.T) {
	dir := t.TempDir()
	execGit(t, dir, "init")
	execGit(t, dir, "config", "user.email", "test@example.com")
	execGit(t, dir, "config", "user.name", "Test")
	writeTestFile(t, dir, "tracked.txt", "trusted repository\n")
	execGit(t, dir, "add", "tracked.txt")
	execGit(t, dir, "commit", "-m", "trusted tree without configuration")
	tree := execGit(t, dir, "rev-parse", "HEAD^{tree}")
	writeTestFile(t, dir, ".roborev.toml", "agent = 'test'\n")
	blob := execGit(t, dir, "hash-object", "-w", ".roborev.toml")

	for _, tc := range []struct {
		name    string
		ref     string
		wantErr bool
	}{
		{name: "missing full object ID", ref: strings.Repeat("e", 40), wantErr: true},
		{name: "blob is not a trusted tree", ref: blob, wantErr: true},
		{name: "tree with missing configuration", ref: tree},
		{name: "root CI empty tree", ref: "4b825dc642cb6eb9a060e54bf8d69288fbee4904"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loaded, err := LoadTrustedRepoConfig(dir, tc.ref)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert := assert.New(t)
			assert.Nil(loaded.Config)
			assert.Nil(loaded.Raw)
			assert.Equal(tc.ref, loaded.Ref)
		})
	}
}

func TestReviewToolPolicyExperimentReplacement(t *testing.T) {
	base := &Config{Review: ReviewConfig{Types: map[string]ReviewTypeSpec{
		"quotes": {Template: "quotes.md", Tools: &ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"first": {"read"}, "second": {"find"}}}},
	}, Subagents: map[string]SubagentSpec{
		"reader": {ReviewType: "quotes", Tools: &ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"first": {"read"}, "second": {"find"}}}},
	}}}
	for _, section := range []string{"types", "subagents"} {
		name := "quotes"
		if section == "subagents" {
			name = "reader"
		}
		for _, grants := range []map[string]any{{}, {"first": []string{"lookup"}}} {
			overlay := map[string]any{"review": map[string]any{section: map[string]any{name: map[string]any{"tools": map[string]any{"mode": "read-only", "mcp": grants}}}}}
			require.NoError(t, validateExperimentOverlay(overlay))
			raw, err := applyExperimentOverlay(base, nil, overlay)
			require.NoError(t, err)
			repo, err := decodeExperimentRepoConfig(raw)
			require.NoError(t, err)
			var policy *ReviewToolPolicy
			if section == "types" {
				policy = repo.Review.Types[name].Tools
				assert.Equal(t, "quotes.md", repo.Review.Types[name].Template)
			} else {
				policy = repo.Review.Subagents[name].Tools
				assert.Equal(t, "quotes", repo.Review.Subagents[name].ReviewType)
			}
			require.NotNil(t, policy)
			assert.Len(t, policy.MCP, len(grants))
			assert.NotContains(t, policy.MCP, "second")
		}
		// A partial policy cannot borrow a mode from the base during overlay.
		overlay := map[string]any{"review": map[string]any{section: map[string]any{name: map[string]any{"tools": map[string]any{"mcp": map[string]any{}}}}}}
		require.Error(t, validateExperimentOverlay(overlay))
	}
	for _, key := range []string{"review_mcp_config", "review_mcp_allowed_tools"} {
		require.Error(t, validateExperimentOverlay(map[string]any{key: "forbidden"}))
	}
}

// Canonicalization must preserve the exact logical grant set, without aliases
// or implicit wildcard/namespace expansion. The oracle checks ASCII bytes
// independently of the production identifier validator.
func FuzzReviewToolPolicyCanonicalization(f *testing.F) {
	for _, seed := range [][4]string{
		{"read-only", "research", "read", "find"},
		{"read-only", "A-0_b", "read", "read"},
		{"", "research", "read", "read"},
		{"read-only", "a__b", "read", "read"},
		{"read-only", "research", "read/*", "read"},
		{"read-only", "research", "", "read"},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3])
	}
	f.Fuzz(func(t *testing.T, mode, server, first, second string) {
		validName := func(s string) bool {
			if s == "" || strings.Contains(s, "__") {
				return false
			}
			for _, c := range []byte(s) {
				if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
					return false
				}
			}
			return true
		}
		input := &ReviewToolPolicy{Mode: mode, MCP: map[string][]string{server: {first, second}}}
		t.Cleanup(func() {
			assert.Equal(t, &ReviewToolPolicy{Mode: mode, MCP: map[string][]string{server: {first, second}}}, input)
		})
		got, err := CanonicalReviewToolPolicy(input)
		if mode != "read-only" || !validName(server) || !validName(first) || !validName(second) {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		want := []string{first}
		if second != first {
			if second < first {
				want = []string{second, first}
			} else {
				want = append(want, second)
			}
		}
		assert.Equal(t, &ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{server: want}}, got)
		again, err := CanonicalReviewToolPolicy(got)
		require.NoError(t, err)
		assert.Equal(t, got, again)
	})
}

func TestReviewToolPolicyExperimentRejectsPanelPolicy(t *testing.T) {
	overlay := map[string]any{"review": map[string]any{"panels": map[string]any{"summary": map[string]any{"tools": map[string]any{"mode": "read-only"}}}}}
	require.ErrorContains(t, validateExperimentOverlay(overlay), "review.panels.summary.tools is not supported")
}
