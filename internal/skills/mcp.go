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
	// Review-producing workflows still need the CLI for creation. Reads and
	// bookkeeping use MCP; this contract also applies to their examples.
	header := mcpModeMarker + `

## MCP transport

Use the roborev MCP tools for reads and review bookkeeping in this workflow.
The CLI remains necessary for starting reviews and running refine. Interpret
read/write CLI examples below using these MCP equivalents:

| Operation | MCP tool and arguments |
| --- | --- |
| Status | roborev_status |
| Find repository | roborev_list_repos; use its root_path |
| List jobs | roborev_list_jobs with repo_path and branch; follow next_cursor |
| Read review | roborev_get_review with job_id or sha |
| Read comments | roborev_list_comments with job_id |
| Read running output | roborev_get_job_output with job_id |
| Comment | roborev_add_comment with job_id, commenter, comment |
| Close | roborev_close_review with job_id |

MCP verdicts are pass, fail, or empty. Poll roborev_list_jobs for completion
before reading a newly queued review. Use synthesis parents for panels.
Use the agent's tool discovery to resolve server prefixes. If tools are missing,
report the MCP connection error; do not silently fall back to CLI reads.

`
	// Insert after frontmatter, leaving the skill's invocation metadata intact.
	if end := strings.Index(text[4:], "\n---"); strings.HasPrefix(text, "---\n") && end >= 0 {
		pos := end + 8
		text = text[:pos] + "\n\n" + header + text[pos:]
	}
	return []byte(text)
}
