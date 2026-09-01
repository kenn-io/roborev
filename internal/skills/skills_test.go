package skills

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/autofix"
)

type agentCase struct {
	agent       Agent
	configDir   string
	legacyDir   string
	displayName string
}

var agentCases = []agentCase{
	{agent: AgentClaude, configDir: ".claude", legacyDir: ".claude", displayName: string(AgentClaude)},
	{agent: AgentCodex, configDir: ".codex", legacyDir: ".codex", displayName: string(AgentCodex)},
	{agent: AgentDroid, configDir: ".factory", legacyDir: ".factory", displayName: string(AgentDroid)},
}

func setupTestEnv(t *testing.T) string {
	t.Helper()
	tmpHome := t.TempDir()

	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")

	// Clear config-dir overrides so tests never touch a real agent config.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")

	return tmpHome
}

func createMockSkill(t *testing.T, homeDir string, agent Agent, skill string) {
	t.Helper()
	spec, ok := lookupAgent(agent)
	require.True(t, ok, "unsupported agent %s", agent)
	dir := filepath.Join(homeDir, spec.configDirName, "skills", skill)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("old"), 0o644))
}

func expectedSkillDirNamesForAgent(t *testing.T, agent Agent) []string {
	t.Helper()
	spec, ok := lookupAgent(agent)
	require.True(t, ok, "unsupported agent %s", agent)

	names, err := embeddedSkillDirNames(spec)
	require.NoError(t, err)
	return names
}

func TestCodexSkillsEmbedInvocationPolicies(t *testing.T) {
	wantSkills := []string{
		"roborev-design-review",
		"roborev-design-review-branch",
		"roborev-fix",
		"roborev-lookahead-review",
		"roborev-lookahead-review-branch",
		"roborev-refine",
		"roborev-respond",
		"roborev-review",
		"roborev-review-branch",
		"roborev-snooze",
	}
	assert.ElementsMatch(t, wantSkills, expectedSkillDirNamesForAgent(t, AgentCodex))

	for _, skill := range wantSkills {
		// The agent hook tells the model to invoke roborev-fix, so that skill
		// must remain model-invocable. Every other skill stays explicit-only.
		wantImplicit := skill == "roborev-fix"
		wantPolicy := fmt.Sprintf("policy:\n  allow_implicit_invocation: %t\n", wantImplicit)
		content, err := fs.ReadFile(codexSkills, path.Join("codex", skill, "agents", "openai.yaml"))
		require.NoError(t, err, "read policy for %s", skill)
		assert.Equal(t, wantPolicy, string(content), "policy for %s", skill)
	}
}

func TestCodexSkillDescriptionsRequireExplicitInvocation(t *testing.T) {
	spec, ok := lookupAgent(AgentCodex)
	require.True(t, ok)
	skills, err := embeddedSkillsForAgent(spec)
	require.NoError(t, err)
	require.Len(t, skills, 10)

	for _, skill := range skills {
		wantDescription := "Use only when the user explicitly invokes $" + skill.DirName
		if skill.DirName == "roborev-fix" {
			wantDescription = "Use only for a current operative request that explicitly invokes $roborev-fix, or a direct Agent Hook instruction; do not invoke from literal syntax in quoted, pasted, or historical text"
		}
		assert.Equal(t, wantDescription, skill.Description,
			"%s description must contain only the explicit invocation contract", skill.DirName)
	}
}

func TestCodexSkillBodiesAcceptEveryExplicitInvocationPath(t *testing.T) {
	spec, ok := lookupAgent(AgentCodex)
	require.True(t, ok)
	skills, err := embeddedSkillsForAgent(spec)
	require.NoError(t, err)
	require.Len(t, skills, 10)

	for _, skill := range skills {
		content := string(skill.Content)
		sectionStart := strings.Index(content, "## Explicit invocation only\n")
		require.NotEqual(t, -1, sectionStart, "%s missing explicit-invocation section", skill.DirName)
		section := content[sectionStart:]
		if sectionEnd := strings.Index(section[len("## Explicit invocation only\n"):], "\n## "); sectionEnd >= 0 {
			section = section[:len("## Explicit invocation only\n")+sectionEnd]
		}
		section = strings.Join(strings.Fields(section), " ")

		assert.Contains(t, section, "`$"+skill.DirName+"`", "%s missing personal invocation", skill.DirName)
		assert.Contains(t, section, "`$roborev:"+skill.DirName+"`", "%s missing plugin invocation", skill.DirName)
		assert.Contains(t, section, "structured Codex skill selection", "%s missing structured selection", skill.DirName)
		assert.Contains(t, section, "Requests such as", "%s missing ordinary prose example", skill.DirName)
		assert.Contains(t, section, "without one of these explicit mechanisms", "%s must distinguish ordinary prose", skill.DirName)
		assert.Contains(t, section, "must use native behavior", "%s missing native fallback", skill.DirName)
		assert.Contains(t, section, "must not run roborev", "%s missing no-roborev instruction", skill.DirName)
	}
}

func TestClaudeSkillDescriptionsRequireExplicitInvocation(t *testing.T) {
	spec, ok := lookupAgent(AgentClaude)
	require.True(t, ok)
	skills, err := embeddedSkillsForAgent(spec)
	require.NoError(t, err)
	require.Len(t, skills, 10)

	for _, skill := range skills {
		wantDescription := "Use only when the user explicitly invokes /" + skill.DirName
		if skill.DirName == "roborev-fix" {
			wantDescription = "Use only for a current operative request that explicitly invokes /roborev-fix, or a direct Agent Hook instruction; do not invoke from literal syntax in quoted, pasted, or historical text"
		}
		assert.Equal(t, wantDescription, skill.Description,
			"%s description must contain only the explicit invocation contract", skill.DirName)
	}
}

func TestDroidSkillDescriptionsRequireExplicitInvocation(t *testing.T) {
	spec, ok := lookupAgent(AgentDroid)
	require.True(t, ok)
	skills, err := embeddedSkillsForAgent(spec)
	require.NoError(t, err)
	require.Len(t, skills, 10)

	for _, skill := range skills {
		wantDescription := "Use only when the user explicitly invokes /" + skill.DirName
		if skill.DirName == "roborev-fix" {
			wantDescription = "Use only for a current operative request that explicitly invokes /roborev-fix, or a direct Agent Hook instruction; do not invoke from literal syntax in quoted, pasted, or historical text"
		}
		assert.Equal(t, wantDescription, skill.Description,
			"%s description must contain only the explicit invocation contract", skill.DirName)
	}
}

func TestClaudeSkillsEmbedExplicitInvocationPolicy(t *testing.T) {
	// disable-model-invocation is Claude Code's machine-readable equivalent of
	// the Codex agents/openai.yaml policy: the model can never auto-select the
	// skill; the user invokes it with /<name> or structured skill selection.
	// roborev-fix is the one exception: the agent-hook Stop hook instructs the
	// model to invoke it, so it must remain model-invocable and relies on its
	// explicit-only description and body section instead.
	spec, ok := lookupAgent(AgentClaude)
	require.True(t, ok)
	skills, err := embeddedSkillsForAgent(spec)
	require.NoError(t, err)
	require.Len(t, skills, 10)

	for _, skill := range skills {
		content := strings.ReplaceAll(string(skill.Content), "\r\n", "\n")
		require.True(t, strings.HasPrefix(content, "---\n"), "%s missing frontmatter", skill.DirName)
		frontmatterEnd := strings.Index(content[len("---\n"):], "\n---\n")
		require.NotEqual(t, -1, frontmatterEnd, "%s missing frontmatter close", skill.DirName)
		frontmatterLines := strings.Split(content[len("---\n"):len("---\n")+frontmatterEnd], "\n")
		if skill.DirName == "roborev-fix" {
			assert.NotContains(t, frontmatterLines, "disable-model-invocation: true",
				"roborev-fix must stay model-invocable for the agent-hook instruction")
		} else {
			assert.Contains(t, frontmatterLines, "disable-model-invocation: true",
				"%s frontmatter must disable implicit model invocation", skill.DirName)
		}
	}
}

func TestClaudeSkillBodiesAcceptEveryExplicitInvocationPath(t *testing.T) {
	spec, ok := lookupAgent(AgentClaude)
	require.True(t, ok)
	skills, err := embeddedSkillsForAgent(spec)
	require.NoError(t, err)
	require.Len(t, skills, 10)

	for _, skill := range skills {
		content := string(skill.Content)
		sectionStart := strings.Index(content, "## Explicit invocation only\n")
		require.NotEqual(t, -1, sectionStart, "%s missing explicit-invocation section", skill.DirName)
		section := content[sectionStart:]
		if sectionEnd := strings.Index(section[len("## Explicit invocation only\n"):], "\n## "); sectionEnd >= 0 {
			section = section[:len("## Explicit invocation only\n")+sectionEnd]
		}
		section = strings.Join(strings.Fields(section), " ")

		assert.Contains(t, section, "`/"+skill.DirName+"`", "%s missing personal invocation", skill.DirName)
		assert.Contains(t, section, "structured Claude Code skill selection", "%s missing structured selection", skill.DirName)
		assert.Contains(t, section, "Requests such as", "%s missing ordinary prose example", skill.DirName)
		assert.Contains(t, section, "without one of these explicit mechanisms", "%s must distinguish ordinary prose", skill.DirName)
		assert.Contains(t, section, "must use native behavior", "%s missing native fallback", skill.DirName)
		assert.Contains(t, section, "must not run roborev", "%s missing no-roborev instruction", skill.DirName)
	}
}

func TestAgentSkillsDocumentSandboxRecovery(t *testing.T) {
	tests := []struct {
		agent          Agent
		parameter      string
		otherParameter string
	}{
		{
			agent:          AgentCodex,
			parameter:      `sandbox_permissions: "require_escalated"`,
			otherParameter: "dangerouslyDisableSandbox: true",
		},
		{
			agent:          AgentClaude,
			parameter:      "dangerouslyDisableSandbox: true",
			otherParameter: `sandbox_permissions: "require_escalated"`,
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.agent), func(t *testing.T) {
			spec, ok := lookupAgent(tt.agent)
			require.True(t, ok)
			skills, err := embeddedSkillsForAgent(spec)
			require.NoError(t, err)
			require.Len(t, skills, 10)

			for _, skill := range skills {
				content := strings.Join(strings.Fields(string(skill.Content)), " ")
				assert.Contains(t, content, "roborev uses a local daemon", skill.DirName)
				assert.Contains(t, content, "Do not start or restart the daemon", skill.DirName)
				assert.Contains(t, content, tt.parameter, skill.DirName)
				assert.NotContains(t, content, tt.otherParameter, skill.DirName)
			}
		})
	}
}

func TestPluginDefaultPromptsExplicitlyInvokeNamespacedSkills(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".codex-plugin", "plugin.json"))
	require.NoError(t, err)
	var manifest struct {
		Interface struct {
			DefaultPrompt []string `json:"defaultPrompt"`
		} `json:"interface"`
	}
	require.NoError(t, json.Unmarshal(data, &manifest))
	assert.Equal(t, []string{
		"Review the current branch with $roborev:roborev-review-branch.",
		"Fix open roborev findings with $roborev:roborev-fix.",
		"Respond to a roborev review with $roborev:roborev-respond.",
	}, manifest.Interface.DefaultPrompt)
}

func findResultByAgent(t *testing.T, results []InstallResult, agent Agent) *InstallResult {
	t.Helper()
	for i := range results {
		if results[i].Agent == agent {
			return &results[i]
		}
	}
	require.Condition(t, func() bool { return false }, "missing install result: no result found for agent %s", agent)
	return nil
}

func findStatusByAgent(t *testing.T, statuses []AgentStatus, agent Agent) AgentStatus {
	t.Helper()
	for _, status := range statuses {
		if status.Agent == agent {
			return status
		}
	}
	require.Condition(t, func() bool { return false }, "missing status for agent %s", agent)
	return AgentStatus{}
}

func requireResultCount(t *testing.T, results []InstallResult, want int) {
	t.Helper()

	require.Len(t, results, want, "unexpected install result count")
}

func resultMap(results []InstallResult) map[Agent]InstallResult {
	out := make(map[Agent]InstallResult, len(results))
	for _, result := range results {
		out[result.Agent] = result
	}
	return out
}

func assertSkillsInstalled(t *testing.T, homeDir string, tc agentCase) {
	t.Helper()

	skillsDir := filepath.Join(homeDir, tc.configDir, "skills")
	for _, skill := range expectedSkillDirNamesForAgent(t, tc.agent) {
		path := filepath.Join(skillsDir, skill, "SKILL.md")
		_, err := os.Stat(path)
		require.NoError(t, err, "expected %s to exist", path)
	}
}

func TestInstallClaudeSkipsWhenDirMissing(t *testing.T) {
	setupTestEnv(t)

	results, err := Install()
	require.NoError(t, err, "Install failed")

	claudeResult := findResultByAgent(t, results, AgentClaude)
	assert.True(t, claudeResult.Skipped, "expected Claude to be skipped when ~/.claude doesn't exist")
	assert.Empty(t, claudeResult.Installed, "expected no installed skills")
}

func TestInstallWhenDirExists(t *testing.T) {
	for _, tc := range agentCases {
		t.Run(tc.displayName, func(t *testing.T) {
			expectedSkills := expectedSkillDirNamesForAgent(t, tc.agent)
			tmpHome := setupTestEnv(t)
			agentDir := filepath.Join(tmpHome, tc.configDir)
			require.NoError(t, os.MkdirAll(agentDir, 0o755))

			results, err := Install()
			require.NoError(t, err, "Install failed")

			res := findResultByAgent(t, results, tc.agent)
			assert.False(t, res.Skipped, "expected not to be skipped")
			assert.Len(t, res.Installed, len(expectedSkills))
			assertSkillsInstalled(t, tmpHome, tc)
		})
	}
}

func TestInstallWritesCodexInvocationPolicies(t *testing.T) {
	tmpHome := setupTestEnv(t)
	for _, tc := range agentCases {
		require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, tc.configDir), 0o755))
	}

	_, err := Install()
	require.NoError(t, err)

	for _, skill := range expectedSkillDirNamesForAgent(t, AgentCodex) {
		wantImplicit := skill == "roborev-fix"
		wantPolicy := fmt.Sprintf("policy:\n  allow_implicit_invocation: %t\n", wantImplicit)
		policyPath := filepath.Join(tmpHome, ".codex", "skills", skill, "agents", "openai.yaml")
		content, err := os.ReadFile(policyPath)
		require.NoError(t, err, "read installed policy for %s", skill)
		assert.Equal(t, wantPolicy, string(content), "installed policy for %s", skill)
	}
	for _, tc := range []agentCase{agentCases[0], agentCases[2]} {
		for _, skill := range expectedSkillDirNamesForAgent(t, tc.agent) {
			policyPath := filepath.Join(tmpHome, tc.configDir, "skills", skill, "agents", "openai.yaml")
			_, err := os.Stat(policyPath)
			assert.ErrorIs(t, err, os.ErrNotExist, "%s should not install Codex policy", tc.agent)
		}
	}
}

func TestCodexStatusRequiresCurrentPolicy(t *testing.T) {
	tmpHome := setupTestEnv(t)
	require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, ".codex"), 0o755))
	_, err := Install()
	require.NoError(t, err)

	skill := expectedSkillDirNamesForAgent(t, AgentCodex)[0]
	policyPath := filepath.Join(tmpHome, ".codex", "skills", skill, "agents", "openai.yaml")
	require.NoError(t, os.Remove(policyPath))
	status := findStatusByAgent(t, Status(), AgentCodex)
	assert.Equal(t, SkillOutdated, status.Skills[skill], "missing policy should be outdated when SKILL.md is present")
	assert.True(t, IsInstalled(AgentCodex), "SKILL.md should remain the installed-presence signal")

	require.NoError(t, os.MkdirAll(filepath.Dir(policyPath), 0o755))
	require.NoError(t, os.WriteFile(policyPath, []byte("policy:\n  allow_implicit_invocation: true\n"), 0o644))
	status = findStatusByAgent(t, Status(), AgentCodex)
	assert.Equal(t, SkillOutdated, status.Skills[skill], "changed policy should be outdated")
}

func TestUpdateAddsCodexPolicyToSkillOnlyInstall(t *testing.T) {
	tmpHome := setupTestEnv(t)
	skill := expectedSkillDirNamesForAgent(t, AgentCodex)[0]
	createMockSkill(t, tmpHome, AgentCodex, skill)

	results, err := Update()
	require.NoError(t, err)
	findResultByAgent(t, results, AgentCodex)

	policyPath := filepath.Join(tmpHome, ".codex", "skills", skill, "agents", "openai.yaml")
	content, err := os.ReadFile(policyPath)
	require.NoError(t, err)
	assert.Equal(t, "policy:\n  allow_implicit_invocation: false\n", string(content))
}

func TestInstallHonorsConfigDirEnvOverride(t *testing.T) {
	tests := []struct {
		agent  Agent
		envVar string
	}{
		{agent: AgentClaude, envVar: "CLAUDE_CONFIG_DIR"},
		{agent: AgentCodex, envVar: "CODEX_HOME"},
	}

	for _, tt := range tests {
		t.Run(tt.envVar, func(t *testing.T) {
			expectedSkills := expectedSkillDirNamesForAgent(t, tt.agent)
			tmpHome := setupTestEnv(t)
			configDir := t.TempDir()
			t.Setenv(tt.envVar, configDir)

			results, err := Install()
			require.NoError(t, err, "Install failed")

			res := findResultByAgent(t, results, tt.agent)
			assert.False(t, res.Skipped, "expected not to be skipped")
			assert.Equal(t, configDir, res.ConfigDir)
			assert.Len(t, res.Installed, len(expectedSkills))

			for _, skill := range expectedSkills {
				path := filepath.Join(configDir, "skills", skill, "SKILL.md")
				_, err := os.Stat(path)
				require.NoError(t, err, "expected %s to exist", path)
			}

			spec, ok := lookupAgent(tt.agent)
			require.True(t, ok)
			_, err = os.Stat(filepath.Join(tmpHome, spec.configDirName))
			assert.True(t, os.IsNotExist(err), "expected nothing under the home config dir")

			assert.True(t, IsInstalled(tt.agent), "expected IsInstalled to honor the override")

			for _, status := range Status() {
				if status.Agent != tt.agent {
					continue
				}
				assert.True(t, status.Available, "expected Status to honor the override")
				for _, skill := range expectedSkills {
					assert.Equal(t, SkillCurrent, status.Skills[skill])
				}
			}
		})
	}
}

func TestInstallSkipsWhenConfigDirEnvOverrideMissing(t *testing.T) {
	tmpHome := setupTestEnv(t)

	// The home config dir exists, but the override takes precedence.
	require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, ".claude"), 0o755))
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("CLAUDE_CONFIG_DIR", missing)

	results, err := Install()
	require.NoError(t, err, "Install failed")

	claudeResult := findResultByAgent(t, results, AgentClaude)
	assert.True(t, claudeResult.Skipped, "expected Claude to be skipped when the override dir doesn't exist")
	assert.Equal(t, missing, claudeResult.ConfigDir)

	_, err = os.Stat(filepath.Join(tmpHome, ".claude", "skills"))
	assert.True(t, os.IsNotExist(err), "expected no skills under the home config dir")
}

func TestInstallIdempotent(t *testing.T) {
	tmpHome := setupTestEnv(t)

	err := os.MkdirAll(filepath.Join(tmpHome, ".claude"), 0o755)
	require.NoError(t, err)

	results1, err := Install()
	require.NoError(t, err, "First install failed: %v", err)

	expectedSkills := expectedSkillDirNamesForAgent(t, AgentClaude)

	claude1 := findResultByAgent(t, results1, AgentClaude)
	require.Len(t, claude1.Installed, len(expectedSkills), "first install: expected %d installed, got %d", len(expectedSkills), len(claude1.Installed))
	require.Empty(t, claude1.Updated, "first install: expected 0 updated, got %d", len(claude1.Updated))

	results2, err := Install()
	require.NoError(t, err, "Second install failed: %v", err)

	claude2 := findResultByAgent(t, results2, AgentClaude)
	require.Empty(t, claude2.Installed, "second install: expected 0 installed, got %d", len(claude2.Installed))
	require.Len(t, claude2.Updated, len(expectedSkills), "second install: expected %d updated, got %d", len(expectedSkills), len(claude2.Updated))
}

func TestInstallToPathDefaultsToSelectedAgentDestination(t *testing.T) {
	for _, agent := range []Agent{AgentClaude, AgentDroid} {
		t.Run(string(agent), func(t *testing.T) {
			skillsDir := filepath.Join(t.TempDir(), "custom", "skills")
			expectedSkills := expectedSkillDirNamesForAgent(t, agent)

			result, err := InstallToPath(agent, skillsDir)
			require.NoError(t, err)
			assert.Equal(t, agent, result.Agent)
			assert.Len(t, result.Installed, len(expectedSkills))
			assert.Empty(t, result.Updated)

			for _, skill := range expectedSkills {
				_, err := os.Stat(filepath.Join(skillsDir, skill, "SKILL.md"))
				require.NoError(t, err, "expected %s skill to be installed", skill)
			}
		})
	}
}

func TestInstallToPathWritesCodexPolicies(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "custom", "skills")

	result, err := InstallToPath(AgentCodex, skillsDir)
	require.NoError(t, err)
	assert.Equal(t, AgentCodex, result.Agent)

	for _, skill := range expectedSkillDirNamesForAgent(t, AgentCodex) {
		policyPath := filepath.Join(skillsDir, skill, "agents", "openai.yaml")
		_, err := os.Stat(policyPath)
		require.NoError(t, err, "expected Codex policy for %s", skill)
	}
}

func TestInstallToPathIsIdempotent(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "skills")
	expectedSkills := expectedSkillDirNamesForAgent(t, AgentClaude)

	first, err := InstallToPath(AgentClaude, skillsDir)
	require.NoError(t, err)
	assert.Len(t, first.Installed, len(expectedSkills))
	assert.Empty(t, first.Updated)

	second, err := InstallToPath(AgentClaude, skillsDir)
	require.NoError(t, err)
	assert.Empty(t, second.Installed)
	assert.Len(t, second.Updated, len(expectedSkills))
}

func TestInstallToPathRemovesLegacySkills(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "skills")
	legacyDir := filepath.Join(skillsDir, "roborev-address")
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(legacyDir, "SKILL.md"), []byte("old"), 0o644))

	_, err := InstallToPath(AgentClaude, skillsDir)
	require.NoError(t, err)

	_, err = os.Stat(legacyDir)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestInstallToPathRejectsUnsupportedAgentWithoutCreatingDestination(t *testing.T) {
	skillsDir := filepath.Join(t.TempDir(), "skills")

	_, err := InstallToPath(Agent("unknown"), skillsDir)
	require.EqualError(t, err, `unsupported agent "unknown" (expected claude, codex, droid, or grok)`)

	_, statErr := os.Stat(skillsDir)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestIsInstalled(t *testing.T) {
	type testCase struct {
		name        string
		agent       Agent
		setup       func(t *testing.T, home string)
		shouldExist bool
	}

	tests := []testCase{
		{
			name:        "Claude missing dir",
			agent:       AgentClaude,
			setup:       func(t *testing.T, h string) {},
			shouldExist: false,
		},
		{
			name:  "Claude dir exists no skills",
			agent: AgentClaude,
			setup: func(t *testing.T, h string) {
				err := os.MkdirAll(filepath.Join(h, ".claude"), 0o755)
				require.NoError(t, err)
			},
			shouldExist: false,
		},
		{
			name:        "Codex missing dir",
			agent:       AgentCodex,
			setup:       func(t *testing.T, h string) {},
			shouldExist: false,
		},
		{
			name:  "Codex dir exists no skills",
			agent: AgentCodex,
			setup: func(t *testing.T, h string) {
				err := os.MkdirAll(filepath.Join(h, ".codex"), 0o755)
				require.NoError(t, err)
			},
			shouldExist: false,
		},
	}

	for _, tc := range agentCases {
		for _, skill := range expectedSkillDirNamesForAgent(t, tc.agent) {
			s := skill
			agent := tc.agent
			tests = append(tests, testCase{
				name:        tc.displayName + " with skill " + s,
				agent:       agent,
				setup:       func(t *testing.T, h string) { createMockSkill(t, h, agent, s) },
				shouldExist: true,
			})
		}
	}

	tests = append(tests, testCase{
		name:  "unsupported agent",
		agent: Agent("unknown"),
		setup: func(t *testing.T, h string) {
			createMockSkill(t, h, AgentClaude, "roborev-fix")
			createMockSkill(t, h, AgentCodex, "roborev-fix")
		},
		shouldExist: false,
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpHome := setupTestEnv(t)
			if tt.setup != nil {
				tt.setup(t, tmpHome)
			}
			require.Equal(t, tt.shouldExist, IsInstalled(tt.agent), "IsInstalled(%s) = %v, want %v", tt.agent, IsInstalled(tt.agent), tt.shouldExist)
		})
	}
}

func TestInstallRemovesLegacySkills(t *testing.T) {
	for _, tc := range agentCases {
		t.Run(tc.displayName, func(t *testing.T) {
			tmpHome := setupTestEnv(t)

			require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, tc.configDir), 0o755))
			createMockSkill(t, tmpHome, tc.agent, "roborev-address")

			_, err := Install()
			require.NoError(t, err)

			legacyDir := filepath.Join(tmpHome, tc.legacyDir, "skills", "roborev-address")
			_, err = os.Stat(legacyDir)
			assert.True(t, os.IsNotExist(err), "expected legacy dir to be removed after install")

			assertSkillsInstalled(t, tmpHome, tc)
		})
	}
}

func TestUpdateRemovesLegacySkills(t *testing.T) {
	tmpHome := setupTestEnv(t)

	// Install a current skill so IsInstalled returns true
	createMockSkill(t, tmpHome, AgentClaude, "roborev-fix")

	// Plant the legacy skill
	legacyDir := filepath.Join(tmpHome, ".claude", "skills", "roborev-address")
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(legacyDir, "SKILL.md"), []byte("old"), 0o644))

	_, err := Update()
	require.NoError(t, err)

	// Legacy skill should be removed
	_, err = os.Stat(legacyDir)
	assert.True(t, os.IsNotExist(err), "expected legacy roborev-address dir to be removed")
}

func TestUpdateLegacyOnlyInstall(t *testing.T) {
	for _, tc := range agentCases {
		t.Run(tc.displayName, func(t *testing.T) {
			expectedSkills := expectedSkillDirNamesForAgent(t, tc.agent)
			tmpHome := setupTestEnv(t)

			// User only has the deprecated skill — no current skills
			createMockSkill(t, tmpHome, tc.agent, "roborev-address")

			results, err := Update()
			require.NoError(t, err)

			require.Len(t, results, 1)
			res := findResultByAgent(t, results, tc.agent)
			assert.Len(t, res.Installed, len(expectedSkills))

			// Legacy dir should be removed
			legacyDir := filepath.Join(tmpHome, tc.legacyDir, "skills", "roborev-address")
			_, err = os.Stat(legacyDir)
			assert.True(t, os.IsNotExist(err), "expected legacy dir to be removed")
		})
	}
}

func TestUpdateOnlyUpdatesInstalled(t *testing.T) {
	expectedSkillCount := len(expectedSkillDirNamesForAgent(t, AgentClaude))

	tests := []struct {
		name          string
		setup         func(t *testing.T, homeDir string)
		wantResults   int
		wantAgents    []Agent
		wantUpdated   int
		wantInstalled int
	}{
		{
			name: "updates Claude with fix skill only",
			setup: func(t *testing.T, homeDir string) {
				createMockSkill(t, homeDir, AgentClaude, "roborev-fix")

				err := os.MkdirAll(filepath.Join(homeDir, ".codex"), 0o755)
				require.NoError(t, err)
			},
			wantResults:   1,
			wantAgents:    []Agent{AgentClaude},
			wantUpdated:   1,
			wantInstalled: expectedSkillCount - 1,
		},
		{
			name: "updates Claude with respond skill only",
			setup: func(t *testing.T, homeDir string) {
				createMockSkill(t, homeDir, AgentClaude, "roborev-respond")
			},
			wantResults:   1,
			wantAgents:    []Agent{AgentClaude},
			wantUpdated:   1,
			wantInstalled: expectedSkillCount - 1,
		},
		{
			name: "updates Codex with fix skill only",
			setup: func(t *testing.T, homeDir string) {
				createMockSkill(t, homeDir, AgentCodex, "roborev-fix")
			},
			wantResults:   1,
			wantAgents:    []Agent{AgentCodex},
			wantUpdated:   1,
			wantInstalled: expectedSkillCount - 1,
		},
		{
			name: "updates Codex with respond skill only",
			setup: func(t *testing.T, homeDir string) {
				createMockSkill(t, homeDir, AgentCodex, "roborev-respond")
			},
			wantResults:   1,
			wantAgents:    []Agent{AgentCodex},
			wantUpdated:   1,
			wantInstalled: expectedSkillCount - 1,
		},
		{
			name: "updates both agents when both have skills",
			setup: func(t *testing.T, homeDir string) {
				createMockSkill(t, homeDir, AgentClaude, "roborev-fix")
				createMockSkill(t, homeDir, AgentCodex, "roborev-respond")
			},
			wantResults:   2,
			wantAgents:    []Agent{AgentClaude, AgentCodex},
			wantUpdated:   1,
			wantInstalled: expectedSkillCount - 1,
		},
		{
			name: "skips both when neither has skills",
			setup: func(t *testing.T, homeDir string) {
				err := os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o755)
				require.NoError(t, err)
				err = os.MkdirAll(filepath.Join(homeDir, ".codex"), 0o755)
				require.NoError(t, err)
			},
			wantResults:   0,
			wantAgents:    []Agent{},
			wantUpdated:   0,
			wantInstalled: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpHome := setupTestEnv(t)
			tt.setup(t, tmpHome)

			results, err := Update()
			require.NoError(t, err, "Update failed: %v", err)
			requireResultCount(t, results, tt.wantResults)

			if tt.wantResults > 0 {
				resultsByAgent := resultMap(results)
				for _, want := range tt.wantAgents {
					r, ok := resultsByAgent[want]
					require.True(t, ok, "expected %s in results", want)
					require.Len(t, r.Updated, tt.wantUpdated, "expected %d updated for %s, got %d", tt.wantUpdated, r.Agent, len(r.Updated))
					require.Len(t, r.Installed, tt.wantInstalled, "expected %d installed for %s, got %d", tt.wantInstalled, r.Agent, len(r.Installed))
				}
			}

			if tt.wantResults == 0 {
				assert.Empty(t, results)
			}
		})
	}
}

func TestListSkillsDeduplicatesAcrossAgents(t *testing.T) {
	skills, err := ListSkills()
	require.NoError(t, err)

	seen := make(map[string]bool)
	for _, skill := range skills {
		assert.False(t, seen[skill.DirName], "duplicate skill in ListSkills output: %s", skill.DirName)
		seen[skill.DirName] = true
	}
}

func TestListSkillsUsesFirstAgentMetadata(t *testing.T) {
	// When frontmatter differs across agents for the same skill,
	// ListSkills should return the first agent's (Claude's) metadata.
	skills, err := ListSkills()
	require.NoError(t, err)

	claudeSkillsByDir := make(map[string]embeddedSkill)
	claudeSpec := supportedAgents[0]
	require.Equal(t, AgentClaude, claudeSpec.agent, "first agent must be Claude for this test")

	embedded, err := embeddedSkillsForAgent(claudeSpec)
	require.NoError(t, err)
	for _, s := range embedded {
		claudeSkillsByDir[s.DirName] = s
	}

	for _, skill := range skills {
		cs, ok := claudeSkillsByDir[skill.DirName]
		if !ok {
			continue
		}
		assert.Equal(t, cs.Name, skill.Name,
			"skill %s: name should match first agent (Claude)", skill.DirName)
		assert.Equal(t, cs.Description, skill.Description,
			"skill %s: description should match first agent (Claude)", skill.DirName)
	}
}

func TestListSkillsReportsSupportedAgents(t *testing.T) {
	skills, err := ListSkills()
	require.NoError(t, err)

	skillsByDir := make(map[string]SkillInfo, len(skills))
	for _, skill := range skills {
		skillsByDir[skill.DirName] = skill
	}

	assert.ElementsMatch(t,
		[]Agent{AgentClaude, AgentCodex, AgentDroid, AgentGrok},
		skillsByDir["roborev-review"].SupportedAgents)
	assert.ElementsMatch(t,
		[]Agent{AgentClaude, AgentCodex, AgentDroid, AgentGrok},
		skillsByDir["roborev-lookahead-review"].SupportedAgents)
	assert.ElementsMatch(t,
		[]Agent{AgentClaude, AgentCodex, AgentDroid, AgentGrok},
		skillsByDir["roborev-lookahead-review-branch"].SupportedAgents)
}

func TestDirNameEnumerationDoesNotReadContent(t *testing.T) {
	// embeddedSkillDirNames only enumerates directories, so it must
	// succeed even when SKILL.md files are absent. This guards against
	// regressions that would make IsInstalled/Update depend on file reads.
	mockFS := fstest.MapFS{
		"agent/skill-a/.keep": &fstest.MapFile{Data: []byte("")},
		"agent/skill-b/.keep": &fstest.MapFile{Data: []byte("")},
	}
	spec := agentSpec{
		agent:         "mock",
		configDirName: ".mock",
		embedFS:       mockFS,
		embedDir:      "agent",
	}

	// embeddedSkillDirNames should succeed (only reads directory entries)
	names, err := embeddedSkillDirNames(spec)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"skill-a", "skill-b"}, names)

	// embeddedSkillsForAgent should fail (reads SKILL.md content)
	_, err = embeddedSkillsForAgent(spec)
	require.Error(t, err, "embeddedSkillsForAgent should fail when SKILL.md is missing")

	// currentInstalledSkillFilePaths should succeed via embeddedSkillDirNames
	home := t.TempDir()
	paths, err := currentInstalledSkillFilePaths(home, spec)
	require.NoError(t, err)
	require.Len(t, paths, 2)
	for _, p := range paths {
		assert.Contains(t, p, filepath.Join(home, ".mock", "skills"),
			"path should be under agent skills dir: %s", p)
		assert.Contains(t, p, "SKILL.md",
			"path should end with SKILL.md: %s", p)
	}
}

func TestCodexSkillsUseCodexProjectInstructions(t *testing.T) {
	spec, ok := lookupAgent(AgentCodex)
	require.True(t, ok)
	skills, err := embeddedSkillsForAgent(spec)
	require.NoError(t, err)
	require.NotEmpty(t, skills)
	for _, skill := range skills {
		assert.Contains(t, string(skill.Content), "AGENTS.md",
			"codex skill %s should reference Codex project instructions", skill.DirName)
	}
}

func TestDroidSkillsUseDroidAdaptations(t *testing.T) {
	// Droid skills are derived from the Codex skills (agent-agnostic, synchronous
	// --wait, no Claude-specific Task tool) with two Factory-specific
	// adaptations: slash invocation (/roborev-X, matching Factory's /skill-name
	// convention) and Factory-specific sandbox escalation wording.
	spec, ok := lookupAgent(AgentDroid)
	require.True(t, ok)
	skills, err := embeddedSkillsForAgent(spec)
	require.NoError(t, err)
	require.NotEmpty(t, skills)
	for _, s := range skills {
		content := string(s.Content)
		assert.NotContains(t, content, "$roborev", "droid skill %s must use /roborev slash invocation, not $roborev", s.DirName)
		assert.NotContains(t, content, "CLAUDE.md", "droid skill %s must reference AGENTS.md, not CLAUDE.md", s.DirName)
		assert.Contains(t, content, "AGENTS.md", "droid skill %s should reference AGENTS.md", s.DirName)
		assert.Contains(t, content, "/roborev-", "droid skill %s should use /roborev- slash invocation", s.DirName)
	}
}

func TestDerivedSkillFilesAreCurrent(t *testing.T) {
	derived, err := renderDerivedSkills(os.DirFS("."))
	require.NoError(t, err)
	// 10 droid + 4 claude + 10 grok (full capability-set parity for Grok)
	require.Len(t, derived, 24)

	for relPath, want := range derived {
		got, err := os.ReadFile(filepath.FromSlash(relPath))
		require.NoError(t, err, "read checked-in derived skill %s", relPath)
		assert.Equal(t, string(want), string(got), "derived skill %s is stale; run `go generate ./internal/skills`", relPath)
	}
}

func TestGrokSkillsCapabilityParityAndLinks(t *testing.T) {
	// Capability set matches Droid (full derived surface), not the smaller
	// Claude install set — so review/design/lookahead cross-links resolve.
	assert.ElementsMatch(t, derivedDroidSkills, derivedGrokSkills)

	spec, ok := lookupAgent(AgentGrok)
	require.True(t, ok)
	skills, err := embeddedSkillsForAgent(spec)
	require.NoError(t, err)
	require.NotEmpty(t, skills)

	installed := make(map[string]struct{}, len(skills))
	for _, s := range skills {
		installed[s.DirName] = struct{}{}
		content := string(s.Content)
		assert.NotContains(t, content, "$roborev", "grok skill %s must use /roborev slash invocation", s.DirName)
		assert.NotContains(t, content, "CLAUDE.md", "grok skill %s must reference AGENTS.md", s.DirName)
		assert.NotContains(t, content, "plugin\n`$roborev", "no Codex plugin namespace remains")
		assert.Contains(t, content, "/roborev-", "grok skill %s should use /roborev- slash invocation", s.DirName)
		assert.Contains(t, content, "AGENTS.md", "grok skill %s should reference AGENTS.md", s.DirName)
		if s.DirName == "roborev-fix" {
			assert.NotContains(t, content, "disable-model-invocation: true",
				"roborev-fix must stay model-invocable for agent hooks")
		} else {
			assert.Contains(t, content, "disable-model-invocation: true",
				"non-fix grok skills must be explicit-only")
		}
	}

	// Every /roborev-* cross-link in Grok skills must resolve to an installed skill.
	linkRE := regexp.MustCompile(`/roborev-[a-z0-9-]+`)
	for _, s := range skills {
		for _, m := range linkRE.FindAllString(string(s.Content), -1) {
			name := strings.TrimPrefix(m, "/")
			// Strip optional suffixes already matched as full skill names.
			_, ok := installed[name]
			assert.True(t, ok, "dangling skill link %s in %s", m, s.DirName)
		}
	}
}

func TestDerivedExplicitInvocationWordingUsesTargetAgent(t *testing.T) {
	derived, err := renderDerivedSkills(os.DirFS("."))
	require.NoError(t, err)

	for relPath, content := range derived {
		text := strings.Join(strings.Fields(string(content)), " ")
		skillName := path.Base(path.Dir(relPath))
		assert.NotContains(t, text, "structured Codex skill selection", "%s retains Codex-specific wording", relPath)
		assert.NotContains(t, text, "roborev:", "%s retains Codex plugin namespace", relPath)
		switch {
		case strings.HasPrefix(relPath, "droid/"):
			assert.Contains(t, text, "`/"+skillName+"`, or structured Factory skill selection", relPath)
			if skillName == "roborev-snooze" {
				assert.Contains(t, text, "disable-model-invocation: true",
					"roborev-snooze must be human-triggered only")
			} else {
				assert.NotContains(t, text, "disable-model-invocation",
					"%s must not carry model-invocation policy", relPath)
			}
		case strings.HasPrefix(relPath, "grok/"):
			assert.Contains(t, text, "`/"+skillName+"`, or structured Grok Build skill selection", relPath)
			if skillName == "roborev-fix" {
				assert.NotContains(t, text, "disable-model-invocation",
					"roborev-fix must stay model-invocable for the agent-hook instruction")
			} else {
				assert.Contains(t, text, "disable-model-invocation: true", "%s missing frontmatter policy", relPath)
			}
		default:
			assert.Contains(t, text, "`/"+skillName+"`, or structured Claude Code skill selection", relPath)
			if skillName == "roborev-fix" {
				assert.NotContains(t, text, "disable-model-invocation",
					"roborev-fix must stay model-invocable for the agent-hook instruction")
			} else {
				assert.Contains(t, text, "disable-model-invocation: true", "%s missing Claude frontmatter policy", relPath)
			}
		}
	}
}

func TestDerivedSandboxWordingUsesTargetAgent(t *testing.T) {
	derived, err := renderDerivedSkills(os.DirFS("."))
	require.NoError(t, err)

	for relPath, content := range derived {
		text := strings.Join(strings.Fields(string(content)), " ")
		if strings.HasPrefix(relPath, "droid/") || strings.HasPrefix(relPath, "grok/") {
			assert.Contains(t, text, "runtime's supported sandbox escalation mechanism", relPath)
			assert.NotContains(t, text, `sandbox_permissions: "require_escalated"`, relPath)
			assert.NotContains(t, text, "dangerouslyDisableSandbox: true", relPath)
		} else {
			assert.Contains(t, text, "dangerouslyDisableSandbox: true", relPath)
			assert.NotContains(t, text, `sandbox_permissions: "require_escalated"`, relPath)
		}
	}
}

func TestFixSkillsUseHeredocForCommentText(t *testing.T) {
	for _, agent := range []Agent{AgentClaude, AgentCodex, AgentDroid, AgentGrok} {
		t.Run(string(agent), func(t *testing.T) {
			spec, ok := lookupAgent(agent)
			require.True(t, ok)
			skills, err := embeddedSkillsForAgent(spec)
			require.NoError(t, err)

			var content string
			for _, skill := range skills {
				if skill.DirName == "roborev-fix" {
					content = strings.ReplaceAll(string(skill.Content), "\r\n", "\n")
				}
			}
			require.NotEmpty(t, content, "missing roborev-fix skill for %s", agent)
			assert.Contains(t, content, "cat <<'ROBOREV_COMMENT'")
			assert.Contains(t, content, "never\nby interpolating dynamic text directly into a shell string")
			assert.NotContains(t, content, `"<summary of changes>"`)
			assert.NotContains(t, content, "Escape quotes and special characters in the bash command")
			assert.Equal(t, 0, strings.Count(content, `roborev comment --commenter roborev-fix --job 1019 "`))
			assert.Equal(t, 0, strings.Count(content, `roborev comment --commenter roborev-fix --job 1021 "`))
		})
	}
}

// If the runtime policy heading and shipped skill drift apart, an Agent Hook
// invocation can supply policy the selected skill does not recognize.
func TestFixSkillsRecognizeRuntimeAutofixGuidelines(t *testing.T) {
	for _, agent := range []Agent{AgentClaude, AgentCodex, AgentDroid, AgentGrok} {
		t.Run(string(agent), func(t *testing.T) {
			spec, ok := lookupAgent(agent)
			require.True(t, ok)
			skills, err := embeddedSkillsForAgent(spec)
			require.NoError(t, err)

			var content string
			for _, skill := range skills {
				if skill.DirName == "roborev-fix" {
					content = string(skill.Content)
				}
			}
			require.NotEmpty(t, content)
			assert.Contains(t, content, autofix.GuidelinesHeading)
		})
	}
}

func TestDroidSkillsInstallToFactoryDir(t *testing.T) {
	// Droid skills install under ~/.factory/skills (Factory's personal skills
	// location), not ~/.droid, and are skipped when ~/.factory is absent so the
	// install stays opt-in for Factory users.
	t.Run("installs under .factory when present", func(t *testing.T) {
		tmpHome := setupTestEnv(t)
		require.NoError(t, os.MkdirAll(filepath.Join(tmpHome, ".factory"), 0o755))

		results, err := Install()
		require.NoError(t, err)
		res := findResultByAgent(t, results, AgentDroid)
		assert.False(t, res.Skipped)
		for _, name := range expectedSkillDirNamesForAgent(t, AgentDroid) {
			_, err := os.Stat(filepath.Join(tmpHome, ".factory", "skills", name, "SKILL.md"))
			require.NoError(t, err, "expected %s skill to install under .factory", name)
		}
		_, err = os.Stat(filepath.Join(tmpHome, ".droid"))
		assert.True(t, os.IsNotExist(err), "no .droid dir should be created")
	})

	t.Run("skipped when .factory absent", func(t *testing.T) {
		setupTestEnv(t)
		results, err := Install()
		require.NoError(t, err)
		res := findResultByAgent(t, results, AgentDroid)
		assert.True(t, res.Skipped, "Droid should be skipped when ~/.factory does not exist")
	})
}

func TestDroidSkillOperationsUseHomeEnvWhenUserHomeDirDiffers(t *testing.T) {
	envHome := t.TempDir()
	userHome := t.TempDir()
	t.Setenv("HOME", envHome)
	stubUserHomeDir(t, userHome)
	require.NoError(t, os.MkdirAll(filepath.Join(envHome, ".factory"), 0o755))

	results, err := Install()
	require.NoError(t, err)
	droidInstall := findResultByAgent(t, results, AgentDroid)
	require.False(t, droidInstall.Skipped, "Droid should use HOME for Factory config discovery")
	assertSkillsInstalled(t, envHome, agentCase{
		agent:       AgentDroid,
		configDir:   ".factory",
		displayName: string(AgentDroid),
	})
	_, err = os.Stat(filepath.Join(userHome, ".factory"))
	require.ErrorIs(t, err, os.ErrNotExist)

	assert.True(t, IsInstalled(AgentDroid), "Droid installed detection should use HOME")

	updates, err := Update()
	require.NoError(t, err)
	droidUpdate := findResultByAgent(t, updates, AgentDroid)
	assert.NotEmpty(t, droidUpdate.Updated, "Droid update should use HOME")

	var droidStatus AgentStatus
	for _, status := range Status() {
		if status.Agent == AgentDroid {
			droidStatus = status
		}
	}
	assert.True(t, droidStatus.Available, "Droid status should use HOME")
	for _, name := range expectedSkillDirNamesForAgent(t, AgentDroid) {
		assert.Equal(t, SkillCurrent, droidStatus.Skills[name])
	}
}

func stubUserHomeDir(t *testing.T, home string) {
	t.Helper()
	old := userHomeDir
	userHomeDir = func() (string, error) {
		return home, nil
	}
	t.Cleanup(func() {
		userHomeDir = old
	})
}
