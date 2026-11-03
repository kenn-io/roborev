package prompt

import (
	"embed"
	"fmt"
	"strings"
	"time"
)

//go:embed templates/*.md.gotmpl
var templateFS embed.FS

// GetSystemPrompt returns the system prompt for the specified agent and type.
// If a specific template exists for the agent, it uses that.
// Otherwise, it falls back to the default templates.
// Supported prompt types: review, range, dirty, address, design-review, run, security, lookahead, test-value
func GetSystemPrompt(agentName string, promptType string) string {
	return getSystemPrompt(agentName, promptType, time.Now)
}

// getSystemPrompt is the internal implementation that accepts a time provider
func getSystemPrompt(agentName string, promptType string, now func() time.Time) string {
	// Normalize agent name
	agentName = strings.ToLower(agentName)
	if agentName == "claude" {
		agentName = "claude-code"
	}

	// For review operations (review, range, dirty), use {agent}_review.md.gotmpl
	// These are all code reviews, just with different input formats
	// Security reviews use {agent}_security.md.gotmpl
	templateType := promptType
	if promptType == "range" || promptType == "dirty" {
		templateType = "review"
	}

	view := systemPromptView{
		ReviewSubject:                    reviewSubject(promptType),
		NoSkillsInstruction:              noSkillsInstruction,
		AntiTestSlopInstruction:          antiTestSlopInstruction,
		ToolchainVerificationInstruction: toolchainVerificationInstruction,
		CurrentDate:                      now().UTC().Format("2006-01-02"),
	}

	tmplName := fmt.Sprintf("%s_%s.md.gotmpl", agentName, templateType)
	if _, err := templateFS.ReadFile("templates/" + tmplName); err == nil {
		body, err := renderSystemPrompt(tmplName, view)
		if err == nil {
			return body
		}
	}

	// Fallback to default templates
	var fallbackName string
	switch promptType {
	case "review":
		fallbackName = "default_review.md.gotmpl"
	case "dirty":
		fallbackName = "default_dirty.md.gotmpl"
	case "range":
		fallbackName = "default_range.md.gotmpl"
	case "address":
		fallbackName = "default_address.md.gotmpl"
	case "security":
		fallbackName = "default_security.md.gotmpl"
	case "lookahead":
		fallbackName = "default_lookahead.md.gotmpl"
	case "test-value":
		fallbackName = "default_test_value.md.gotmpl"
	case "design-review":
		fallbackName = "default_design_review.md.gotmpl"
	case "run":
		// No default run preamble - return empty so raw prompts are used
		return ""
	default:
		fallbackName = "default_review.md.gotmpl"
	}

	body, err := renderSystemPrompt(fallbackName, view)
	if err != nil {
		return ""
	}
	return body
}

// reviewSubject names what a general code review covers, so the shared
// templates can say how to use the commit messages that come with it.
func reviewSubject(promptType string) string {
	switch promptType {
	case "review":
		return "commit"
	case "range", "dirty":
		return promptType
	default:
		return ""
	}
}
