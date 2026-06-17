package prompt

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type systemPromptTestCase struct {
	name            string
	agent           string
	command         string
	wantContains    []string
	wantNotContains []string
	wantExact       string // if set, checks for exact match
	wantEmpty       bool
}

func (tc *systemPromptTestCase) assert(t *testing.T, got string) {
	assert := assert.New(t)

	t.Helper()
	if tc.wantEmpty {
		assert.Empty(got, "got %q, want empty string", got)
		return
	}

	if tc.wantExact != "" && got != tc.wantExact {
		assert.Equal(tc.wantExact, got, "got %q, want %q", got, tc.wantExact)
	}

	for _, substr := range tc.wantContains {
		assert.Contains(got, substr, "got prompt missing %q", substr)
	}

	for _, substr := range tc.wantNotContains {
		assert.NotContains(got, substr, "prompt should NOT contain %q", substr)
	}
}

func TestGetSystemPrompt_Fallbacks(t *testing.T) {
	fixedTime := time.Date(2030, 6, 15, 0, 0, 0, 0, time.UTC)

	// Define a mock time provider
	mockNow := func() time.Time {
		return fixedTime
	}

	// Get the review prompt to verify fallbacks match exactly
	geminiReviewPrompt := getSystemPrompt("gemini", "review", mockNow)
	codexReviewPrompt := getSystemPrompt("codex", "review", mockNow)
	claudeReviewPrompt := getSystemPrompt("claude-code", "review", mockNow)

	tests := []systemPromptTestCase{
		{
			name:         "Codex Review",
			agent:        "codex",
			command:      "review",
			wantContains: []string{"## Review Findings", "Do not include any front matter", "Do NOT build the project, run the test suite, or execute the code while reviewing.", "finish all tool use before emitting the final review"},
		},
		{
			name:         "Claude Review",
			agent:        "claude-code",
			command:      "review",
			wantContains: []string{"## Review Findings", "Do not include any front matter", "Do NOT build the project, run the test suite, or execute the code while reviewing.", "finish all tool use before emitting the final review"},
		},
		{
			name:         "Gemini Review",
			agent:        "gemini",
			command:      "review",
			wantContains: []string{"Do NOT explain your process", "Do NOT build the project, run the test suite, or execute the code while reviewing.", "finish all tool use before emitting the final review"},
		},
		{
			name:      "Codex Range (Review Fallback)",
			agent:     "codex",
			command:   "range",
			wantExact: codexReviewPrompt,
		},
		{
			name:      "Claude Dirty (Review Fallback)",
			agent:     "claude-code",
			command:   "dirty",
			wantExact: claudeReviewPrompt,
		},
		{
			name:      "Gemini Range (Review Fallback)",
			agent:     "gemini",
			command:   "range",
			wantExact: geminiReviewPrompt,
		},
		{
			name:      "Gemini Dirty (Review Fallback)",
			agent:     "gemini",
			command:   "dirty",
			wantExact: geminiReviewPrompt,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getSystemPrompt(tc.agent, tc.command, mockNow)
			tc.assert(t, got)
		})
	}
}

func TestGetSystemPrompt_ReviewPromptsRequireFindingSeparators(t *testing.T) {
	fixedTime := time.Date(2030, 6, 15, 0, 0, 0, 0, time.UTC)
	mockNow := func() time.Time { return fixedTime }

	tests := []struct {
		name    string
		agent   string
		command string
	}{
		{name: "default review", agent: "test", command: "review"},
		{name: "default dirty", agent: "test", command: "dirty"},
		{name: "default range", agent: "test", command: "range"},
		{name: "default security", agent: "test", command: "security"},
		{name: "claude review", agent: "claude-code", command: "review"},
		{name: "codex review", agent: "codex", command: "review"},
		{name: "gemini review", agent: "gemini", command: "review"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getSystemPrompt(tt.agent, tt.command, mockNow)

			assert.Contains(t, got, "Separate multiple findings with `---` on its own line.")
		})
	}
}

func TestGetSystemPrompt_SecurityPromptRequiresExploitabilityGate(t *testing.T) {
	fixedTime := time.Date(2030, 6, 15, 0, 0, 0, 0, time.UTC)
	mockNow := func() time.Time { return fixedTime }

	got := getSystemPrompt("test", "security", mockNow)

	assert.Contains(t, got, "exploitability burden of proof")
	assert.Contains(t, got, "real trust boundary")
	assert.Contains(t, got, "same-user access is not an attacker boundary by itself")
	assert.Contains(t, got, "Do not report environment-variable exposure")
	assert.Contains(t, got, "newly exposes env vars across a trust boundary")
	assert.Contains(t, got, "Drop the finding if")
	assert.NotContains(t, got, "low**: Defense-in-depth improvement")
}

func TestGetSystemPrompt_DateInjection(t *testing.T) {
	fixedTime := time.Date(2030, 6, 15, 0, 0, 0, 0, time.UTC)
	fixedDateStr := "Current date: 2030-06-15 (UTC)"

	mockNow := func() time.Time {
		return fixedTime
	}

	tests := []systemPromptTestCase{
		{
			name:         "Gemini Run",
			agent:        "gemini",
			command:      "run",
			wantContains: []string{"Do NOT explain your process", fixedDateStr},
		},
		{
			name:         "Date Injection (Claude)",
			agent:        "claude-code",
			command:      "review",
			wantContains: []string{fixedDateStr},
		},
		{
			name:         "Date Injection (Gemini)",
			agent:        "gemini",
			command:      "review",
			wantContains: []string{fixedDateStr},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getSystemPrompt(tc.agent, tc.command, mockNow)
			tc.assert(t, got)
		})
	}
}

func TestGetSystemPrompt_Instructions(t *testing.T) {
	fixedTime := time.Date(2030, 6, 15, 0, 0, 0, 0, time.UTC)
	mockNow := func() time.Time { return fixedTime }

	tests := []systemPromptTestCase{
		{
			name:      "Non-Gemini Run (Claude)",
			agent:     "claude-code",
			command:   "run",
			wantEmpty: true,
		},
		{
			name:      "Non-Gemini Run (Claude alias)",
			agent:     "claude",
			command:   "run",
			wantEmpty: true,
		},
		{
			name:      "Non-Gemini Run (Unknown)",
			agent:     "unknown-agent",
			command:   "run",
			wantEmpty: true,
		},
		{
			name:         "Review includes no-skills instruction",
			agent:        "claude-code",
			command:      "review",
			wantContains: []string{"Do NOT use any external skills", "Do NOT include process narration", "put the final review only after the last tool call"},
		},
		{
			name:         "Security includes no-skills instruction",
			agent:        "claude-code",
			command:      "security",
			wantContains: []string{"Do NOT use any external skills", "Do NOT include process narration", "put the final review only after the last tool call"},
		},
		{
			name:         "Address includes no-skills instruction",
			agent:        "claude-code",
			command:      "address",
			wantContains: []string{"Do NOT use any external skills", "Do NOT include process narration", "put the final review only after the last tool call"},
		},
		{
			name:         "Fallback review includes no process narration instruction",
			agent:        "unknown-agent",
			command:      "review",
			wantContains: []string{"Do NOT include process narration", "put the final review only after the last tool call"},
		},
		{
			name:            "Gemini run excludes no-skills instruction",
			agent:           "gemini",
			command:         "run",
			wantContains:    []string{"Do NOT explain your process"},
			wantNotContains: []string{"Do NOT use any external skills"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getSystemPrompt(tc.agent, tc.command, mockNow)
			tc.assert(t, got)
		})
	}
}

func TestGetSystemPrompt_ToolchainVerificationInstruction(t *testing.T) {
	fixedTime := time.Date(2030, 6, 15, 0, 0, 0, 0, time.UTC)
	mockNow := func() time.Time { return fixedTime }

	const marker = "toolchain and dependency manifests"

	tests := []systemPromptTestCase{
		{
			name:         "Codex review includes toolchain-verification instruction",
			agent:        "codex",
			command:      "review",
			wantContains: []string{marker},
		},
		{
			name:         "Claude review includes toolchain-verification instruction",
			agent:        "claude-code",
			command:      "review",
			wantContains: []string{marker},
		},
		{
			name:         "Gemini review includes toolchain-verification instruction",
			agent:        "gemini",
			command:      "review",
			wantContains: []string{marker},
		},
		{
			name:         "Range inherits toolchain-verification instruction",
			agent:        "codex",
			command:      "range",
			wantContains: []string{marker},
		},
		{
			name:         "Dirty inherits toolchain-verification instruction",
			agent:        "claude-code",
			command:      "dirty",
			wantContains: []string{marker},
		},
		{
			name:         "Security includes toolchain-verification instruction",
			agent:        "claude-code",
			command:      "security",
			wantContains: []string{marker},
		},
		{
			name:         "Design review includes toolchain-verification instruction",
			agent:        "claude-code",
			command:      "design-review",
			wantContains: []string{marker},
		},
		{
			name:         "Default fallback review includes toolchain-verification instruction",
			agent:        "test",
			command:      "review",
			wantContains: []string{marker},
		},
		{
			name:            "Address (fix) excludes toolchain-verification instruction",
			agent:           "claude-code",
			command:         "address",
			wantNotContains: []string{marker},
		},
		{
			name:            "Gemini run excludes toolchain-verification instruction",
			agent:           "gemini",
			command:         "run",
			wantNotContains: []string{marker},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getSystemPrompt(tc.agent, tc.command, mockNow)
			tc.assert(t, got)
		})
	}
}

func TestGetSystemPrompt_Exported(t *testing.T) {
	assert := assert.New(t)
	before := time.Now().UTC().Truncate(24 * time.Hour)
	got := GetSystemPrompt("gemini", "review")
	after := time.Now().UTC().Truncate(24 * time.Hour)

	assert.NotEmpty(got, "GetSystemPrompt returned empty string for gemini review")

	beforeStr := fmt.Sprintf("Current date: %s (UTC)", before.Format("2006-01-02"))
	afterStr := fmt.Sprintf("Current date: %s (UTC)", after.Format("2006-01-02"))

	assert.Condition(func() bool {
		return strings.Contains(got, beforeStr) || strings.Contains(got, afterStr)
	}, "prompt missing expected date string. Looked for %q or %q", beforeStr, afterStr)
}

func TestGetSystemPrompt_DefaultFallbacksRenderFromTemplates(t *testing.T) {
	fixedTime := time.Date(2030, 6, 15, 0, 0, 0, 0, time.UTC)
	mockNow := func() time.Time { return fixedTime }

	got := getSystemPrompt("unknown-agent", "security", mockNow)

	assert.Contains(t, got, "You are a security code reviewer")
	assert.Contains(t, got, "Do NOT use any external skills")
	assert.Contains(t, got, "Current date: 2030-06-15 (UTC)")
}

func TestRenderSystemPrompt_AgentSpecificTemplates(t *testing.T) {
	body, err := renderSystemPrompt("claude-code_review.md.gotmpl", systemPromptView{
		NoSkillsInstruction: noSkillsInstruction,
		CurrentDate:         "2030-06-15",
	})
	require.NoError(t, err)
	assert.Contains(t, body, "You are a code reviewer")
}

func TestRenderSystemPrompt_AllEmbeddedAgentSpecificTemplates(t *testing.T) {
	matches, err := fs.Glob(templateFS, "templates/*.md.gotmpl")
	require.NoError(t, err)

	for _, match := range matches {
		name := strings.TrimPrefix(match, "templates/")
		if strings.HasPrefix(name, "default_") || strings.HasPrefix(name, "assembled_") || name == "prompt_sections.md.gotmpl" {
			continue
		}

		t.Run(name, func(t *testing.T) {
			tmpl := promptTemplates.Lookup(name)
			require.NotNil(t, tmpl, "embedded agent-specific template should be parsed into the template set")

			body, err := renderSystemPrompt(name, systemPromptView{
				NoSkillsInstruction: noSkillsInstruction,
				CurrentDate:         "2030-06-15",
			})
			require.NoError(t, err)
			assert.NotEmpty(t, body)
		})
	}
}
