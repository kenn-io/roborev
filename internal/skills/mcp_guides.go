package skills

import (
	"fmt"

	"go.kenn.io/kit/mcpskills"
)

// MCPGuides returns the shipped skills rendered for MCP, without reading any
// installed skills or agent configuration. Each call returns independent values.
func MCPGuides() ([]mcpskills.Guide, error) {
	// Keep the catalog explicit: adding a bundled file must not silently expose it.
	titles := []struct{ name, title string }{
		{"roborev-design-review", "Review a design"},
		{"roborev-design-review-branch", "Review a branch design"},
		{"roborev-fix", "Address review findings"},
		{"roborev-lookahead-review", "Review for look-ahead bias"},
		{"roborev-lookahead-review-branch", "Review a branch for look-ahead bias"},
		{"roborev-refine", "Iterate on review findings"},
		{"roborev-respond", "Respond to a review"},
		{"roborev-review", "Review a commit"},
		{"roborev-review-branch", "Review a branch"},
		{"roborev-setup-review", "Set up a custom review"},
		{"roborev-snooze", "Snooze Agent Hook reminders"},
		{"roborev-superpowers", "Build with Superpowers and Roborev"},
	}
	spec, _ := lookupAgent(AgentCodex)
	embedded, err := embeddedSkillsForAgent(spec)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]embeddedSkill, len(embedded))
	for _, skill := range embedded {
		byName[skill.Name] = skill
	}
	guides := make([]mcpskills.Guide, 0, len(titles))
	for _, entry := range titles {
		skill, ok := byName[entry.name]
		if !ok {
			return nil, fmt.Errorf("missing bundled MCP guide %q", entry.name)
		}
		metadata := mcpskills.Entry{Name: entry.name, Title: entry.title, Summary: skill.Description}
		guides = append(guides, mcpskills.Guide{
			Entry: metadata,
			Markdown: fmt.Sprintf(mcpGuideInvocation, entry.name, entry.name) +
				string(renderMCPSkill(skill.Content)),
		})
	}
	return guides, nil
}

const mcpGuideInvocation = `## Reading this guide over MCP

This is bundled guidance, not an execution tool. Reading it does not invoke the
skill or authorize its commands. The authored guide below uses Codex spelling.
For this MCP guide, a current explicit user request using ` + "`$%s`, `/%s`" + `,
its plugin-qualified spelling, or explicit skill selection in the
client satisfies its explicit invocation requirement. Ordinary prose and quoted,
pasted or historical examples do not. Preserve the guide's scope rules,
including the exact review IDs supplied by an Agent Hook. Some steps require
local CLI, Git or code access; report unavailable capabilities without inventing
MCP tools or silently changing transports.

`
