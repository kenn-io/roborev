// Package ghaction generates GitHub Actions workflow files
// for roborev CI reviews.
package ghaction

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
)

// Allowed values for validation (prevent injection).
var (
	allowedAgents = []string{
		"codex", "claude-code", "gemini",
		"copilot", "opencode", "cursor",
		"kiro", "kilo", "droid",
	}
	safeVersionRE = regexp.MustCompile(
		`^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.]+)?$`)
)

// WorkflowConfig holds the parameters for generating a GitHub
// Actions workflow.
type WorkflowConfig struct {
	// Agents is the list of AI agents to use (e.g.,
	// ["codex", "claude-code"]). Review types, reasoning,
	// and other review parameters come from .roborev.toml
	// at runtime.
	Agents []string

	// RoborevVersion is the roborev release version to
	// install. Empty means "latest".
	RoborevVersion string
}

// DefaultConfig returns a WorkflowConfig with sensible defaults.
func DefaultConfig() WorkflowConfig {
	return WorkflowConfig{
		Agents: []string{"codex"},
	}
}

// AgentInfo holds per-agent data for the workflow template.
type AgentInfo struct {
	Name       string // e.g., "codex"
	EnvVar     string // e.g., "OPENAI_API_KEY"
	SecretName string // e.g., "OPENAI_API_KEY"
	InstallCmd string // e.g., "npm install -g @openai/codex@latest"
}

// Validate checks all config fields against allowlists and safe
// patterns. Returns an error describing the first invalid field.
func (c *WorkflowConfig) Validate() error {
	if len(c.Agents) == 0 {
		return fmt.Errorf("at least one agent is required")
	}
	for _, ag := range c.Agents {
		if !contains(allowedAgents, ag) {
			return fmt.Errorf(
				"invalid agent %q (valid: %s)",
				ag, strings.Join(allowedAgents, ", "))
		}
	}
	if c.RoborevVersion != "" &&
		!safeVersionRE.MatchString(c.RoborevVersion) {
		return fmt.Errorf(
			"invalid roborev version %q "+
				"(expected semver like 0.33.1)",
			c.RoborevVersion)
	}
	return nil
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}

// AgentEnvVar returns the environment variable name that the
// agent expects for API authentication.
func AgentEnvVar(agentName string) string {
	switch agentName {
	case "claude-code":
		return "ANTHROPIC_API_KEY"
	case "opencode":
		return "ANTHROPIC_API_KEY"
	case "gemini":
		return "GOOGLE_API_KEY"
	case "copilot":
		return "GITHUB_TOKEN"
	case "kiro":
		// kiro-cli is not CI-compatible yet; use GITHUB_TOKEN
		// so envEntries skips it (same as copilot).
		return "GITHUB_TOKEN"
	case "kilo":
		return "ANTHROPIC_API_KEY"
	default:
		return "OPENAI_API_KEY"
	}
}

// AgentInstallCmd returns the shell command(s) to install the
// given agent CLI.
func AgentInstallCmd(agentName string) string {
	switch agentName {
	case "claude-code":
		return "npm install -g @anthropic-ai/claude-code@latest"
	case "gemini":
		return "npm install -g @google/gemini-cli@latest"
	case "copilot":
		return "npm install -g @github/copilot@latest"
	case "codex":
		return "npm install -g @openai/codex@latest"
	case "opencode":
		return "npm install -g opencode-ai@latest"
	case "cursor":
		return "echo 'Cursor agent is not available in CI" +
			" environments; choose a different agent'"
	case "kiro":
		return "echo 'Install kiro-cli: see https://kiro.dev/'"
	case "kilo":
		return "npm install -g @kilocode/cli@latest"
	case "droid":
		return "pip install droid-cli || echo 'Note: droid" +
			" agent may require additional setup; see" +
			" Factory documentation'"
	default:
		return "echo 'Install your agent CLI manually'"
	}
}

// AgentSecrets returns the deduped list of agent secret
// requirements (by env var) for display in next-steps output.
func AgentSecrets(agents []string) []AgentInfo {
	return envEntries(buildAgentInfos(agents))
}

// buildAgentInfos deduplicates agents and builds AgentInfo
// entries. Agents sharing the same env var are deduped so only
// one secret entry appears in the workflow.
func buildAgentInfos(agents []string) []AgentInfo {
	seen := make(map[string]bool)
	var infos []AgentInfo

	for _, ag := range agents {
		if seen[ag] {
			continue
		}
		seen[ag] = true
		envVar := AgentEnvVar(ag)
		infos = append(infos, AgentInfo{
			Name:       ag,
			EnvVar:     envVar,
			SecretName: envVar,
			InstallCmd: AgentInstallCmd(ag),
		})
	}
	return infos
}

// envEntries deduplicates agent infos by env var so the env
// block doesn't repeat the same variable. GITHUB_TOKEN is
// skipped because the workflow template already provides it
// via the hardcoded GH_TOKEN line.
func envEntries(infos []AgentInfo) []AgentInfo {
	seen := make(map[string]bool)
	var entries []AgentInfo
	for _, info := range infos {
		if info.EnvVar == "GITHUB_TOKEN" {
			continue
		}
		if seen[info.EnvVar] {
			continue
		}
		seen[info.EnvVar] = true
		entries = append(entries, info)
	}
	return entries
}

// Generate produces a GitHub Actions workflow YAML string from
// the given config.
func Generate(cfg WorkflowConfig) (string, error) {
	if len(cfg.Agents) == 0 {
		cfg.Agents = []string{"codex"}
	}

	if err := cfg.Validate(); err != nil {
		return "", fmt.Errorf("invalid config: %w", err)
	}

	agentInfos := buildAgentInfos(cfg.Agents)

	data := templateData{
		Agents:         agentInfos,
		EnvEntries:     envEntries(agentInfos),
		RoborevVersion: cfg.RoborevVersion,
	}

	tmpl, err := template.New("workflow").Parse(
		workflowTemplate)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}

	return buf.String(), nil
}

// WriteWorkflow generates the workflow and writes it to the
// given path. Creates parent directories as needed. Returns an
// error if the file already exists and force is false.
func WriteWorkflow(
	cfg WorkflowConfig,
	outputPath string,
	force bool,
) error {
	if !force {
		if _, err := os.Stat(outputPath); err == nil {
			return fmt.Errorf(
				"workflow file already exists: %s "+
					"(use --force to overwrite)",
				outputPath)
		}
	}

	content, err := Generate(cfg)
	if err != nil {
		return err
	}

	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf(
			"create directory %s: %w", dir, err)
	}

	if err := os.WriteFile(
		outputPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write workflow: %w", err)
	}

	return nil
}

type templateData struct {
	Agents         []AgentInfo
	EnvEntries     []AgentInfo
	RoborevVersion string
}

// Pinned SHA for actions/checkout v6.0.2 — matches the pattern
// used in the project's own .github/workflows/ci.yml for
// supply-chain hardening.
var workflowTemplate = `# roborev CI Review
# Generated by: roborev init gh-action
# Runs AI-powered code reviews on pull requests.
#
# Required setup:
{{- range .EnvEntries }}
{{- if or (eq .Name "opencode") (eq .Name "kilo") }}
#   - Add a repository secret named "{{ .SecretName }}" (default for {{ .Name }}).
#     If you use a different model provider, replace with the appropriate key
#     (e.g., OPENAI_API_KEY, GOOGLE_API_KEY) and update the env block below.
{{- else }}
#   - Add a repository secret named "{{ .SecretName }}" with your {{ .Name }} API key
{{- end }}
{{- end }}
#
# Review behavior (types, reasoning, severity) is configured in
# .roborev.toml under the [ci] section.

name: roborev

on:
  pull_request:
    types: [opened, synchronize, reopened]

permissions:
  contents: read
  pull-requests: write

jobs:
  review:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout
        uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd  # v6.0.2
        with:
          fetch-depth: 0

      - name: Install roborev
        run: |
          set -euo pipefail
          {{- if .RoborevVersion }}
          ROBOREV_VERSION="{{ .RoborevVersion }}"
          {{- else }}
          ROBOREV_VERSION=$(curl -sfL https://api.github.com/repos/roborev-dev/roborev/releases/latest | grep '"tag_name"' | sed -E 's/.*"v?([^"]+)".*/\1/')
          {{- end }}
          ARCHIVE="roborev_${ROBOREV_VERSION}_linux_amd64.tar.gz"
          curl -sfLO "https://github.com/roborev-dev/roborev/releases/download/v${ROBOREV_VERSION}/${ARCHIVE}"
          curl -sfLO "https://github.com/roborev-dev/roborev/releases/download/v${ROBOREV_VERSION}/SHA256SUMS"
          grep -F "  ${ARCHIVE}" SHA256SUMS > verify.txt
          sha256sum --check verify.txt
          mkdir -p "$HOME/.local/bin"
          tar xzf "${ARCHIVE}" -C "$HOME/.local/bin" roborev
          echo "$HOME/.local/bin" >> "$GITHUB_PATH"
          rm -f "${ARCHIVE}" SHA256SUMS verify.txt
          "$HOME/.local/bin/roborev" version

      # TODO: Pin agent CLI versions for supply-chain safety.
      # Replace @latest with a specific version (e.g., @1.2.3).
      - name: Install agents
        run: |
          set -euo pipefail
          {{- range .Agents }}
          {{ .InstallCmd }}
          {{- end }}

      - name: Run review
        env:
          GH_TOKEN: ${{"{{"}} secrets.GITHUB_TOKEN {{"}}"}}
          {{- range .EnvEntries }}
          {{ .EnvVar }}: ${{"{{"}} secrets.{{ .SecretName }} {{"}}"}}
          {{- end }}
        run: |
          set -euo pipefail
          roborev ci review \
            --ref "${{"{{"}} github.event.pull_request.base.sha {{"}}"}}..${{"{{"}} github.event.pull_request.head.sha {{"}}"}}" \
            --comment \
            --gh-repo "${{"{{"}} github.repository {{"}}"}}" \
            --pr "${{"{{"}} github.event.pull_request.number {{"}}"}}"
`
