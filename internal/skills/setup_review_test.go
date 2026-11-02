package skills

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agentguide"
	"go.kenn.io/roborev/internal/config"
)

func TestInstallSetupReviewWorkflow(t *testing.T) {
	for _, agent := range Agents() {
		t.Run(string(agent), func(t *testing.T) {
			destination := t.TempDir()
			result, err := InstallToPath(agent, destination, nil)
			require.NoError(t, err)
			assert.Contains(t, result.Installed, "roborev-setup-review")
			content, err := os.ReadFile(filepath.Join(destination, "roborev-setup-review", "SKILL.md"))
			require.NoError(t, err)
			name, _ := parseFrontmatter(content)
			assert.Equal(t, "roborev-setup-review", name)
		})
	}
}

// Agents copy the setup skill's example into real configuration, so it must
// load and validate as roborev reads it, and its hook panel must resolve to the
// custom review types it names.
func TestSetupReviewConfigExampleValidates(t *testing.T) {
	content, err := fs.ReadFile(codexSkills, path.Join("codex", "roborev-setup-review", "SKILL.md"))
	require.NoError(t, err)
	example := setupReviewTOMLExample(t, string(content), "# .roborev.toml")

	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".roborev.toml"), []byte(example), 0o644))
	repoCfg, err := config.LoadRepoConfig(repo)
	require.NoError(t, err)
	require.NotNil(t, repoCfg)
	require.NoError(t, config.ValidateEffectiveReviewConfig(nil, repoCfg))

	panel, ok := repoCfg.Review.Panels[repoCfg.Review.HookPanel]
	require.True(t, ok, "example hook panel %q is not defined", repoCfg.Review.HookPanel)
	require.NotEmpty(t, panel.Members)
	for _, member := range panel.Members {
		subagent, ok := repoCfg.Review.Subagents[member]
		require.True(t, ok, "panel member %s is not defined", member)
		_, ok = config.ResolveCustomReviewTypeFromConfig(subagent.ReviewType, repoCfg, nil)
		require.True(t, ok, "member %s uses undefined custom type %q", member, subagent.ReviewType)
	}

	flow := strings.TrimSpace(repoCfg.AgentHook.ReviewFlow)
	require.NotEmpty(t, flow, "example must show a review flow")
	graph, err := agentguide.LintDOT(agentguide.FenceDOT(flow))
	require.NoError(t, err)
	assert.Equal(t, "roborev_"+strings.ReplaceAll(repoCfg.Review.HookPanel, "-", "_"), graph.Name)
	for _, member := range panel.Members {
		assert.Contains(t, flow, member)
	}
	exit := false
	for _, node := range graph.Nodes {
		if node.Shape == "oval" && len(graph.Out(node.ID)) == 0 && strings.Contains(node.Label, "roborev_triage") {
			exit = true
		}
	}
	assert.True(t, exit, "flow must hand back to the triage diagram")
	for _, token := range []string{"$", "/roborev", "AGENTS.md"} {
		assert.NotContains(t, flow, token)
	}
	t.Run("Graphviz", func(t *testing.T) {
		err := agentguide.CanonWithGraphviz(t.Context(), flow)
		if errors.Is(err, agentguide.ErrGraphvizUnavailable) {
			t.Skip("graphviz not installed")
		}
		require.NoError(t, err)
	})

	// Custom types do not inherit review_agent. Resolve the example with
	// different generic defaults to ensure it follows the selected default
	// rather than pinning a reviewer in the example.
	for _, defaultAgent := range []string{"grok", "pi"} {
		t.Run(defaultAgent, func(t *testing.T) {
			assert := assert.New(t)
			members, _, err := config.ResolvePanel(
				repoCfg.Review.HookPanel,
				repo,
				&config.Config{ReviewAgent: "codex", DefaultAgent: defaultAgent},
			)
			require.NoError(t, err)
			require.NotEmpty(t, members)
			for _, member := range members {
				assert.Equal(defaultAgent, member.Agent,
					"custom reviewer %s should follow default_agent", member.Name)
			}
		})
	}
}

// setupReviewTOMLExample returns the one fenced TOML block whose first line is
// the given file marker comment.
func setupReviewTOMLExample(t *testing.T, content, marker string) string {
	t.Helper()
	var matches []string
	var block []string
	inTOML := false
	for line := range strings.SplitSeq(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		switch {
		case !inTOML && strings.TrimSpace(line) == "```toml":
			inTOML = true
			block = block[:0]
		case inTOML && strings.TrimSpace(line) == "```":
			inTOML = false
			if len(block) > 0 && strings.TrimSpace(block[0]) == marker {
				matches = append(matches, strings.Join(block, "\n")+"\n")
			}
		case inTOML:
			block = append(block, line)
		}
	}
	require.Len(t, matches, 1, "want exactly one TOML example marked %q", marker)
	return matches[0]
}
