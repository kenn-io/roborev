package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestMCPGuidesReuseInstalledContentAndPolicy(t *testing.T) {
	t.Parallel()
	guides, err := MCPGuides()
	require.NoError(t, err)
	require.Len(t, guides, 12)
	dir := t.TempDir()
	_, err = InstallToPath(AgentCodex, dir, new(true))
	require.NoError(t, err)
	spec, ok := lookupAgent(AgentCodex)
	require.True(t, ok)
	embedded, err := embeddedSkillsForAgent(spec)
	require.NoError(t, err)
	for _, guide := range guides {
		t.Run(guide.Name, func(t *testing.T) {
			assert := assert.New(t)
			installed, err := os.ReadFile(filepath.Join(dir, guide.Name, "SKILL.md"))
			require.NoError(t, err)
			assert.True(strings.HasSuffix(guide.Markdown, string(installed)), "retains the entire rendered authored guide")
			assert.Contains(guide.Markdown, "`$"+guide.Name+"`")
			assert.Contains(guide.Markdown, "`/"+guide.Name+"`")
			assert.Contains(guide.Markdown, "Reading it does not invoke")
			assert.Contains(guide.Markdown, "quoted,")
			assert.Contains(guide.Markdown, "exact review IDs supplied by an Agent Hook")
			for _, source := range embedded {
				if source.Name == guide.Name {
					assert.Equal(source.Description, guide.Summary)
				}
			}
		})
	}
}

func TestMCPGuidesAreIndependent(t *testing.T) {
	t.Parallel()
	expected, err := MCPGuides()
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		index := hegel.Draw(ht, hegel.Integers(0, len(expected)-1))
		text := hegel.Draw(ht, hegel.Text())
		guides, err := MCPGuides()
		require.NoError(ht, err)
		guides[index].Name, guides[index].Title = text, text
		guides[index].Summary, guides[index].Markdown = text, text
		actual, err := MCPGuides()
		require.NoError(ht, err)
		assert.Equal(ht, expected, actual)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestMCPGuidesIgnoreInstalledContent(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "PI_CODING_AGENT_DIR"} {
		t.Setenv(key, root)
	}
	installed := filepath.Join(root, "skills", "roborev-fix", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(installed), 0o755))
	expected, err := MCPGuides()
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text())
		require.NoError(ht, os.WriteFile(installed, []byte(text), 0o600))
		actual, err := MCPGuides()
		require.NoError(ht, err)
		assert.Equal(ht, expected, actual)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestMCPGuidesPreserveWorkflowBoundaries(t *testing.T) {
	t.Parallel()
	guides, err := MCPGuides()
	require.NoError(t, err)
	byName := make(map[string]string, len(guides))
	for _, guide := range guides {
		byName[guide.Name] = guide.Markdown
	}
	// Check policy independently of the shared installer renderer.
	for name, phrases := range map[string][]string{
		"roborev-fix": {
			"Use the exact review job IDs supplied for this fix session",
			"An Agent Hook invocation does not broaden the user's current task",
			"An Agent Hook invocation is limited to its exact IDs",
			"Never invent or discover a session ID",
			"Leave reviews with an Ask finding open",
		},
		"roborev-respond": {
			"## Explicit invocation only",
			"If the invocation has no message, ask the user what to record",
		},
		"roborev-setup-review": {
			"write files only after the user approves",
			"Do not comment on or close trial reviews unless the user asks",
		},
	} {
		for _, phrase := range phrases {
			assert.Contains(t, byName[name], phrase, name)
		}
	}
}
