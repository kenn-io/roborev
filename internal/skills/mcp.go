package skills

import (
	"bytes"
	"embed"
	"os"
	"strings"
)

//go:embed mcp/*.md
var mcpInstructions embed.FS

const mcpModeMarker = "<!-- roborev transport: mcp -->"

func installedMCPMode(skillsDir string) bool {
	content, err := os.ReadFile(skillInstallPath(skillsDir, "roborev-fix"))
	return err == nil && bytes.Contains(content, []byte(mcpModeMarker))
}

func renderMCPSkill(content []byte) []byte {
	name, _ := parseFrontmatter(content)
	text := string(content)
	if body, err := mcpInstructions.ReadFile("mcp/" + name + ".md"); err == nil {
		// Preserve invocation and scope policy; replace CLI-specific execution steps.
		cut := strings.Index(text, "## Instructions")
		if important := strings.Index(text, "## IMPORTANT"); important >= 0 {
			cut = important
		}
		if cut >= 0 {
			text = text[:cut]
		}
		if start := strings.Index(text, "## Sandbox access"); start >= 0 {
			end := strings.Index(text[start+3:], "\n## ")
			if end < 0 {
				text = text[:start]
			} else {
				text = text[:start] + text[start+3+end+1:]
			}
		}
		return []byte(text + "\n" + mcpModeMarker + "\n\n" + string(body))
	}
	return content
}
