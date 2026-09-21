// Package analyze provides built-in analysis prompts for file-level reviews.
package analyze

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

func BuildOutputPrefix(analysisType string, filePaths []string) string {
	sorted := append([]string(nil), filePaths...)
	sort.Strings(sorted)
	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s Analysis\n\n", analysisType)
	sb.WriteString("**Files:**\n")
	for _, path := range sorted {
		fmt.Fprintf(&sb, "- %s\n", path)
	}
	sb.WriteString("\n---\n\n")
	return sb.String()
}

//go:embed *.txt
var promptFS embed.FS

// AnalysisType represents a built-in analysis type
type AnalysisType struct {
	Name        string
	Description string
	promptFile  string
}

// Available analysis types
var (
	TestFixtures = AnalysisType{
		Name:        "test-fixtures",
		Description: "Find test fixture and helper opportunities to reduce duplication",
		promptFile:  "test_fixtures.txt",
	}
	Duplication = AnalysisType{
		Name:        "duplication",
		Description: "Find code duplication across files",
		promptFile:  "duplication.txt",
	}
	Refactor = AnalysisType{
		Name:        "refactor",
		Description: "Suggest refactoring opportunities",
		promptFile:  "refactor.txt",
	}
	Complexity = AnalysisType{
		Name:        "complexity",
		Description: "Analyze complexity and suggest simplifications",
		promptFile:  "complexity.txt",
	}
	APIDesign = AnalysisType{
		Name:        "api-design",
		Description: "Review API consistency and design patterns",
		promptFile:  "api_design.txt",
	}
	DeadCode = AnalysisType{
		Name:        "dead-code",
		Description: "Find unused exports and unreachable code",
		promptFile:  "dead_code.txt",
	}
	Architecture = AnalysisType{
		Name:        "architecture",
		Description: "Review architectural patterns and structure",
		promptFile:  "architecture.txt",
	}
	Security = AnalysisType{
		Name:        "security",
		Description: "Find concrete security risks in existing code",
		promptFile:  "security.txt",
	}
)

// AllTypes returns all available analysis types
var AllTypes = []AnalysisType{
	TestFixtures,
	Duplication,
	Refactor,
	Complexity,
	APIDesign,
	DeadCode,
	Architecture,
	Security,
}

// GetType returns an analysis type by name, or nil if not found
func GetType(name string) *AnalysisType {
	for _, t := range AllTypes {
		if t.Name == name {
			return &t
		}
	}
	return nil
}

// GetPrompt returns the prompt template for this analysis type
func (t AnalysisType) GetPrompt() (string, error) {
	data, err := promptFS.ReadFile(t.promptFile)
	if err != nil {
		return "", fmt.Errorf("failed to read prompt for %s: %w", t.Name, err)
	}
	return string(data), nil
}

// BuildPrompt constructs the full prompt with file contents
func (t AnalysisType) BuildPrompt(files map[string]string) (string, error) {
	template, err := t.GetPrompt()
	if err != nil {
		return "", err
	}

	var sb strings.Builder

	// Build sorted file list for deterministic output
	fileNames := make([]string, 0, len(files))
	for name := range files {
		fileNames = append(fileNames, name)
	}
	sort.Strings(fileNames)

	// Write metadata header (for easy copy-paste into agent sessions)
	sb.WriteString("## Analysis Request\n\n")
	fmt.Fprintf(&sb, "**Type:** %s\n", t.Name)
	fmt.Fprintf(&sb, "**Description:** %s\n", t.Description)
	fmt.Fprintf(&sb, "**Files:** %s\n\n", strings.Join(fileNames, ", "))

	// Write file contents in sorted order
	sb.WriteString("## Files to Analyze\n\n")
	for _, name := range fileNames {
		content := files[name]
		fmt.Fprintf(&sb, "### %s\n\n", name)
		sb.WriteString("```\n")
		sb.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n\n")
	}

	// Write the analysis prompt
	sb.WriteString("## Instructions\n\n")
	sb.WriteString(template)

	return sb.String(), nil
}

// BuildPromptWithPaths constructs a prompt with file paths only (no contents).
// The agent is expected to read the files itself. Used when files are too large
// to embed in the prompt. The repoRoot is displayed in the header for context;
// filePaths should be absolute paths that the agent can read directly.
func (t AnalysisType) BuildPromptWithPaths(repoRoot string, filePaths []string) (string, error) {
	template, err := t.GetPrompt()
	if err != nil {
		return "", err
	}

	sort.Strings(filePaths)

	var sb strings.Builder

	// Write metadata header
	sb.WriteString("## Analysis Request\n\n")
	fmt.Fprintf(&sb, "**Type:** %s\n", t.Name)
	fmt.Fprintf(&sb, "**Description:** %s\n", t.Description)
	fmt.Fprintf(&sb, "**Repository:** %s\n", repoRoot)
	fmt.Fprintf(&sb, "**Files:** %s\n\n", strings.Join(filePaths, ", "))

	// List file paths for the agent to read
	sb.WriteString("## Files to Analyze\n\n")
	sb.WriteString("The following files are too large to embed. Please read them directly:\n\n")
	for _, path := range filePaths {
		fmt.Fprintf(&sb, "- `%s`\n", path)
	}
	sb.WriteString("\n")

	// Write the analysis prompt
	sb.WriteString("## Instructions\n\n")
	sb.WriteString(template)

	return sb.String(), nil
}
