// Package skills provides embedded skill files for AI agents (Claude Code, Codex,
// Factory Droid) and installation utilities.
package skills

import (
	"bufio"
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

//go:generate go run ./generate

//go:embed claude/*/SKILL.md
var claudeSkills embed.FS

//go:embed codex/*/SKILL.md
var codexSkills embed.FS

//go:embed droid/*/SKILL.md
var droidSkills embed.FS

// Agent represents a supported AI agent
type Agent string

const (
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
	AgentDroid  Agent = "droid"
)

type agentSpec struct {
	agent         Agent
	configDirName string
	configDirEnv  string
	embedFS       fs.FS
	embedDir      string
}

type embeddedSkill struct {
	DirName     string
	Name        string
	Description string
	Content     []byte
}

var supportedAgents = []agentSpec{
	{agent: AgentClaude, configDirName: ".claude", configDirEnv: "CLAUDE_CONFIG_DIR", embedFS: claudeSkills, embedDir: "claude"},
	{agent: AgentCodex, configDirName: ".codex", configDirEnv: "CODEX_HOME", embedFS: codexSkills, embedDir: "codex"},
	{agent: AgentDroid, configDirName: ".factory", embedFS: droidSkills, embedDir: "droid"},
}

var userHomeDir = os.UserHomeDir

// InstallResult contains the result of a skill installation
type InstallResult struct {
	Agent     Agent
	ConfigDir string // Resolved agent config directory
	Installed []string
	Updated   []string
	Skipped   bool // True if agent config dir doesn't exist
}

// Install installs skills for all supported agents whose config directories exist.
// It is idempotent - running multiple times will update existing skills.
func Install() ([]InstallResult, error) {
	results := make([]InstallResult, 0, len(supportedAgents))
	for _, spec := range supportedAgents {
		result, err := installAgent(spec)
		if err != nil {
			return nil, fmt.Errorf("%s skills: %w", spec.agent, err)
		}
		results = append(results, result)
	}
	return results, nil
}

// IsInstalled checks if any roborev skills are installed for the given agent
func IsInstalled(agent Agent) bool {
	spec, ok := lookupAgent(agent)
	if !ok {
		return false
	}

	home, err := homeDirForAgent(spec)
	if err != nil {
		return false
	}

	checkFiles, err := installedSkillFilePaths(home, spec)
	if err != nil {
		return false
	}

	// Return true if any skill file exists
	for _, f := range checkFiles {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// legacySkills lists skill directories that have been removed and
// should be deleted from user machines during Update.
var legacySkills = []string{
	"roborev-address",
}

func lookupAgent(agent Agent) (agentSpec, bool) {
	for _, spec := range supportedAgents {
		if spec.agent == agent {
			return spec, true
		}
	}
	return agentSpec{}, false
}

func agentConfigDir(home string, spec agentSpec) string {
	if spec.configDirEnv != "" {
		if dir := os.Getenv(spec.configDirEnv); dir != "" {
			return dir
		}
	}
	return filepath.Join(home, spec.configDirName)
}

func agentSkillsDir(home string, spec agentSpec) string {
	return filepath.Join(agentConfigDir(home, spec), "skills")
}

func homeDirForAgent(spec agentSpec) (string, error) {
	if spec.agent == AgentDroid {
		if home := os.Getenv("HOME"); home != "" {
			return home, nil
		}
	}
	return userHomeDir()
}

func skillInstallPath(skillsDir, skillName string) string {
	return filepath.Join(skillsDir, skillName, "SKILL.md")
}

func embeddedSkillsForAgent(spec agentSpec) ([]embeddedSkill, error) {
	entries, err := fs.ReadDir(spec.embedFS, spec.embedDir)
	if err != nil {
		return nil, fmt.Errorf("read embedded skills: %w", err)
	}

	skills := make([]embeddedSkill, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dirName := entry.Name()
		content, err := fs.ReadFile(spec.embedFS, path.Join(spec.embedDir, dirName, "SKILL.md"))
		if err != nil {
			return nil, fmt.Errorf("read %s/SKILL.md: %w", dirName, err)
		}

		name, desc := parseFrontmatter(content)
		if name == "" {
			name = dirName
		}
		skills = append(skills, embeddedSkill{
			DirName:     dirName,
			Name:        name,
			Description: desc,
			Content:     content,
		})
	}
	return skills, nil
}

// embeddedSkillDirNames returns just the directory names of embedded skills
// without reading file contents. Use this for path-only operations like
// IsInstalled and Update where content is not needed.
func embeddedSkillDirNames(spec agentSpec) ([]string, error) {
	entries, err := fs.ReadDir(spec.embedFS, spec.embedDir)
	if err != nil {
		return nil, fmt.Errorf("read embedded skills: %w", err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

func currentInstalledSkillFilePaths(home string, spec agentSpec) ([]string, error) {
	dirNames, err := embeddedSkillDirNames(spec)
	if err != nil {
		return nil, err
	}

	skillsDir := agentSkillsDir(home, spec)
	paths := make([]string, 0, len(dirNames))
	for _, name := range dirNames {
		paths = append(paths, skillInstallPath(skillsDir, name))
	}
	return paths, nil
}

func legacyInstalledSkillFilePaths(skillsDir string) []string {
	out := make([]string, len(legacySkills))
	for i, n := range legacySkills {
		out[i] = skillInstallPath(skillsDir, n)
	}
	return out
}

func installedSkillFilePaths(home string, spec agentSpec) ([]string, error) {
	skillsDir := agentSkillsDir(home, spec)
	current, err := currentInstalledSkillFilePaths(home, spec)
	if err != nil {
		return nil, err
	}
	return append(current, legacyInstalledSkillFilePaths(skillsDir)...), nil
}

// Update updates skills for agents that already have them installed
// and removes legacy skills that are no longer shipped.
func Update() ([]InstallResult, error) {
	var results []InstallResult
	for _, spec := range supportedAgents {
		home, err := homeDirForAgent(spec)
		if err != nil {
			return nil, fmt.Errorf("get home dir: %w", err)
		}
		installed, err := installedSkillFilePaths(home, spec)
		if err != nil {
			return nil, fmt.Errorf("update %s skills: %w", spec.agent, err)
		}
		if !anyFileExists(installed) {
			continue
		}

		result, err := installAgent(spec)
		if err != nil {
			return nil, fmt.Errorf("update %s skills: %w", spec.agent, err)
		}
		results = append(results, result)
	}

	return results, nil
}

// removeLegacySkills deletes skill directories that are no longer
// embedded in the binary.
func removeLegacySkills(spec agentSpec) error {
	home, err := homeDirForAgent(spec)
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}

	skillsDir := agentSkillsDir(home, spec)
	for _, name := range legacySkills {
		dir := filepath.Join(skillsDir, name)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove legacy skill %s: %w", name, err)
		}
	}
	return nil
}

func installAgent(spec agentSpec) (InstallResult, error) {
	result := InstallResult{Agent: spec.agent}

	home, err := homeDirForAgent(spec)
	if err != nil {
		return result, fmt.Errorf("get home dir: %w", err)
	}

	configDir := agentConfigDir(home, spec)
	result.ConfigDir = configDir
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		result.Skipped = true
		return result, nil
	}

	skillsDir := agentSkillsDir(home, spec)
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return result, fmt.Errorf("create skills dir: %w", err)
	}

	skills, err := embeddedSkillsForAgent(spec)
	if err != nil {
		return result, err
	}

	for _, skill := range skills {
		skillName := skill.DirName
		skillDir := filepath.Join(skillsDir, skillName)

		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			return result, fmt.Errorf("create %s dir: %w", skillName, err)
		}

		destPath := filepath.Join(skillDir, "SKILL.md")
		existed := fileExists(destPath)

		if err := os.WriteFile(destPath, skill.Content, 0o644); err != nil {
			return result, fmt.Errorf("write %s/SKILL.md: %w", skillName, err)
		}

		if existed {
			result.Updated = append(result.Updated, skillName)
		} else {
			result.Installed = append(result.Installed, skillName)
		}
	}

	if err := removeLegacySkills(spec); err != nil {
		return result, err
	}
	return result, nil
}

// SkillInfo describes an available skill.
type SkillInfo struct {
	DirName         string // e.g. "roborev-fix"
	Name            string // e.g. "roborev-fix"
	Description     string
	SupportedAgents []Agent
}

// SkillState describes whether a skill is installed and up to date for an agent.
type SkillState int

const (
	SkillMissing  SkillState = iota // Not installed
	SkillCurrent                    // Installed and matches embedded version
	SkillOutdated                   // Installed but content differs from embedded
)

// AgentStatus describes the installation state for a single agent.
type AgentStatus struct {
	Agent     Agent
	Available bool                  // Whether the agent config dir exists
	Skills    map[string]SkillState // keyed by dir name (e.g. "roborev-fix")
}

// ListSkills returns metadata for all embedded skills, deduplicated by
// directory name. When the same skill exists across multiple agents, the
// first agent's metadata is used.
func ListSkills() ([]SkillInfo, error) {
	seen := make(map[string]int)
	var out []SkillInfo
	for _, spec := range supportedAgents {
		skills, err := embeddedSkillsForAgent(spec)
		if err != nil {
			return nil, err
		}
		for _, skill := range skills {
			if idx, ok := seen[skill.DirName]; ok {
				if !slices.Contains(out[idx].SupportedAgents, spec.agent) {
					out[idx].SupportedAgents = append(out[idx].SupportedAgents, spec.agent)
				}
				continue
			}
			seen[skill.DirName] = len(out)
			out = append(out, SkillInfo{
				DirName:         skill.DirName,
				Name:            skill.Name,
				Description:     skill.Description,
				SupportedAgents: []Agent{spec.agent},
			})
		}
	}
	return out, nil
}

// Status returns per-agent, per-skill installation state.
func Status() []AgentStatus {
	var out []AgentStatus
	for _, spec := range supportedAgents {
		home, err := homeDirForAgent(spec)
		if err != nil {
			return nil
		}
		status := AgentStatus{
			Agent:  spec.agent,
			Skills: make(map[string]SkillState),
		}

		configDir := agentConfigDir(home, spec)
		if _, err := os.Stat(configDir); err != nil {
			out = append(out, status)
			continue
		}
		status.Available = true

		skills, err := embeddedSkillsForAgent(spec)
		if err != nil {
			out = append(out, status)
			continue
		}

		skillsDir := agentSkillsDir(home, spec)
		for _, skill := range skills {
			installedPath := skillInstallPath(skillsDir, skill.DirName)

			installedContent, err := os.ReadFile(installedPath)
			if err != nil {
				status.Skills[skill.DirName] = SkillMissing
				continue
			}

			if bytes.Equal(installedContent, skill.Content) {
				status.Skills[skill.DirName] = SkillCurrent
			} else {
				status.Skills[skill.DirName] = SkillOutdated
			}
		}

		out = append(out, status)
	}
	return out
}

// parseFrontmatter extracts name and description from YAML frontmatter.
func parseFrontmatter(data []byte) (name, description string) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return "", ""
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		if after, ok := strings.CutPrefix(line, "name:"); ok {
			name = strings.TrimSpace(after)
		} else if after, ok := strings.CutPrefix(line, "description:"); ok {
			description = strings.TrimSpace(after)
		}
	}
	return name, description
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func anyFileExists(paths []string) bool {
	return slices.ContainsFunc(paths, fileExists)
}
