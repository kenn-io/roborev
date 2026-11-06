package mcpserver

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/mcpskills"

	"go.kenn.io/roborev/internal/skills"
)

func TestSkillsToolListsBundledGuides(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	// Any data operation panics: guide discovery needs only shipped content.
	session := connectSession(t, &struct{ Backend }{})
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "roborev.skills", Arguments: map[string]any{},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	output := skillsMetadata(t, result)
	withoutArguments, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "roborev.skills"})
	require.NoError(t, err)
	assert.Equal(output, skillsMetadata(t, withoutArguments))
	assert.Equal(result.Content, withoutArguments.Content)
	var names []string
	for _, entry := range output.Skills {
		names = append(names, entry.Name)
		assert.NotEmpty(entry.Title)
		assert.NotEmpty(entry.Summary)
	}
	assert.Equal([]string{
		"workflow", "roborev-design-review", "roborev-design-review-branch",
		"roborev-fix", "roborev-lookahead-review", "roborev-lookahead-review-branch",
		"roborev-refine", "roborev-respond", "roborev-review", "roborev-review-branch",
		"roborev-setup-review", "roborev-snooze", "roborev-superpowers",
	}, names)
}

func skillsMetadata(t *testing.T, result *mcp.CallToolResult) mcpskills.Output {
	t.Helper()
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var output mcpskills.Output
	require.NoError(t, json.Unmarshal(data, &output))
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	assert.Len(t, raw, 1)
	for _, entry := range raw["skills"].([]any) {
		assert.Len(t, entry, 3, "metadata has only name, title and summary")
	}
	return output
}

func TestSkillsDiscoveryAndGuideContents(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	session := connectSession(t, &struct{ Backend }{})
	assert.Contains(session.InitializeResult().Instructions, "roborev.skills")
	assert.Contains(session.InitializeResult().Instructions, `{"name":"workflow"}`)
	assert.Contains(session.InitializeResult().Instructions, "does not invoke")
	tools, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	index := slices.IndexFunc(tools.Tools, func(tool *mcp.Tool) bool { return tool.Name == "roborev.skills" })
	require.NotEqual(t, -1, index)
	tool := tools.Tools[index]
	require.NotNil(t, tool.Annotations)
	assert.True(tool.Annotations.ReadOnlyHint)
	assert.True(tool.Annotations.IdempotentHint)
	require.NotNil(t, tool.Annotations.OpenWorldHint)
	assert.False(*tool.Annotations.OpenWorldHint)

	guides, err := skills.MCPGuides()
	require.NoError(t, err)
	for _, guide := range guides {
		t.Run(guide.Name, func(t *testing.T) {
			result := callTool(t, session, "roborev.skills", map[string]any{"name": guide.Name})
			require.False(t, result.IsError)
			require.Len(t, result.Content, 1)
			assert.Equal(guide.Markdown, result.Content[0].(*mcp.TextContent).Text)
			assert.Equal([]mcpskills.Entry{guide.Entry}, skillsMetadata(t, result).Skills)
		})
	}
	resource, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: guidanceResourceURI})
	require.NoError(t, err)
	workflow := callTool(t, session, "roborev.skills", map[string]any{"name": "workflow"})
	assert.Equal(resource.Contents[0].Text, workflow.Content[0].(*mcp.TextContent).Text)
	assert.Len(skillsMetadata(t, workflow).Skills, 1)
}

func TestSkillsHTTPTransport(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(New(&struct{ Backend }{}, "test", nil).HTTPHandler())
	t.Cleanup(server.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: server.URL + HTTPPath}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	result := callTool(t, session, "roborev.skills", map[string]any{"name": "workflow"})
	assert.Equal(t, guidanceMarkdown, result.Content[0].(*mcp.TextContent).Text)
}
