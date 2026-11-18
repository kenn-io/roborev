package mcpserver

import (
	"fmt"

	"go.kenn.io/kit/mcpskills"

	"go.kenn.io/roborev/internal/skills"
)

func newSkillsCatalog() *mcpskills.Catalog {
	guides, err := skills.MCPGuides()
	if err != nil {
		panic(fmt.Errorf("load bundled MCP guides: %w", err))
	}
	workflow := mcpskills.Entry{
		Name: "workflow", Title: "Roborev MCP workflow",
		Summary: "Navigate reviews and use the available MCP tools and local CLI steps.",
	}
	guides = append([]mcpskills.Guide{{
		Entry:    workflow,
		Markdown: guidanceMarkdown,
	}}, guides...)
	catalog, err := mcpskills.New(mcpskills.Config{ToolName: "roborev.skills", Guides: guides})
	if err != nil {
		panic(fmt.Errorf("configure bundled MCP guides: %w", err))
	}
	return catalog
}
