package analyze

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetType(t *testing.T) {
	tests := []struct {
		name    string
		want    string
		wantNil bool
	}{
		{"test-fixtures", "test-fixtures", false},
		{"duplication", "duplication", false},
		{"refactor", "refactor", false},
		{"complexity", "complexity", false},
		{"api-design", "api-design", false},
		{"dead-code", "dead-code", false},
		{"architecture", "architecture", false},
		{"security", "security", false},
		{"invalid", "", true},
		{"", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetType(tt.name)
			if tt.wantNil {
				assert.Nil(t, got, "GetType(%q) = %v, want nil", tt.name, got)
				return
			}
			require.NotNil(t, got, "GetType(%q) = nil, want %q", tt.name, tt.want)
			assert.Equal(t, tt.want, got.Name,
				"GetType(%q).Name = %q, want %q", tt.name, got.Name, tt.want)
		})
	}
}

func TestAllTypesHavePrompts(t *testing.T) {
	for _, at := range AllTypes {
		t.Run(at.Name, func(t *testing.T) {
			prompt, err := at.GetPrompt()
			require.NoError(t, err, "GetPrompt() error = %v", err)
			assert.NotEmpty(t, prompt, "GetPrompt() returned empty string")
			assert.GreaterOrEqual(t, len(prompt), 50,
				"GetPrompt() returned suspiciously short prompt: %q", prompt)
		})
	}
}

func TestSecurityPromptRequiresActionableFindings(t *testing.T) {
	prompt, err := Security.GetPrompt()
	require.NoError(t, err, "GetPrompt()")

	assert.Contains(t, prompt, "file path", "prompt should require file paths")
	assert.Contains(t, prompt, "evidence", "prompt should require evidence")
	assert.Contains(t, prompt, "severity", "prompt should require severity")
	assert.Contains(t, prompt, "confidence", "prompt should require confidence")
	assert.Contains(t, prompt, "trust boundary", "prompt should require trust-boundary reasoning")
	assert.Contains(t, prompt, "suggested fix", "prompt should require mitigations")
	assert.Contains(t, prompt, "exploitable", "prompt should require exploitability assessment")
	assert.Contains(t, prompt, "concrete", "prompt should discourage generic findings")
}

func TestSecurityPromptDiscouragesSecurityTheater(t *testing.T) {
	prompt, err := Security.GetPrompt()
	require.NoError(t, err, "GetPrompt()")

	assert.Contains(t, prompt, "exploitability burden of proof")
	assert.Contains(t, prompt, "same-user access is not an attacker boundary by itself")
	assert.Contains(t, prompt, "Do not report environment-variable exposure")
	assert.Contains(t, prompt, "newly exposes env vars across a trust boundary")
	assert.Contains(t, prompt, "Do not emit low-severity defense-in-depth findings")
	assert.Contains(t, prompt, "Drop the finding if")
}

func TestBuildPrompt(t *testing.T) {
	files := map[string]string{
		"b.go": "package b\n",
		"a.go": "package a\n",
	}

	prompt, err := TestFixtures.BuildPrompt(files)
	require.NoError(t, err, "BuildPrompt() error = %v", err)

	// Check header
	assert.Contains(t, prompt, "## Analysis Request", "prompt missing '## Analysis Request' header")
	assert.Contains(t, prompt, "**Type:** test-fixtures", "prompt missing type")

	// Check files are listed in sorted order
	assert.Contains(t, prompt, "**Files:** a.go, b.go", "prompt should list files in sorted order")

	// Check file contents appear
	assert.Contains(t, prompt, "### a.go", "prompt missing a.go section")
	assert.Contains(t, prompt, "### b.go", "prompt missing b.go section")
	assert.Contains(t, prompt, "package a", "prompt missing a.go content")
	assert.Contains(t, prompt, "package b", "prompt missing b.go content")

	// Check instructions section
	assert.Contains(t, prompt, "## Instructions", "prompt missing '## Instructions' header")

	// Verify a.go comes before b.go in the content (sorted order)
	aIdx := strings.Index(prompt, "### a.go")
	bIdx := strings.Index(prompt, "### b.go")
	assert.Less(t, aIdx, bIdx, "files should be in sorted order (a.go before b.go)")
}

func TestBuildPromptAddsMissingNewline(t *testing.T) {
	files := map[string]string{
		"test.go": "package test", // No trailing newline
	}

	prompt, err := TestFixtures.BuildPrompt(files)
	require.NoError(t, err, "BuildPrompt() error = %v", err)

	// Should have added a newline before the closing ```
	assert.Contains(t, prompt, "package test\n```", "prompt should add missing newline before closing code fence")
}

func TestBuildPromptEmptyFiles(t *testing.T) {
	files := map[string]string{}

	prompt, err := TestFixtures.BuildPrompt(files)
	require.NoError(t, err, "BuildPrompt() error = %v", err)

	// Should still have headers
	assert.Contains(t, prompt, "## Analysis Request", "prompt missing header")
	assert.Contains(t, prompt, "**Files:** \n", "prompt should have empty files list")
}

func TestBuildPromptWithPaths(t *testing.T) {
	repoRoot := t.TempDir()
	filePaths := []string{
		filepath.Join(repoRoot, "b.go"),
		filepath.Join(repoRoot, "a.go"),
	}

	prompt, err := TestFixtures.BuildPromptWithPaths(repoRoot, filePaths)
	require.NoError(t, err, "BuildPromptWithPaths() error = %v", err)

	// Check header
	assert.Contains(t, prompt, "## Analysis Request", "prompt missing '## Analysis Request' header")
	assert.Contains(t, prompt, "**Type:** test-fixtures", "prompt missing type")
	assert.Contains(t, prompt, "**Repository:** "+repoRoot, "prompt missing repository path")
	// Files header should list paths (sorted)
	expectedFilesHeader := "**Files:** " + filepath.Join(repoRoot, "a.go") + ", " + filepath.Join(repoRoot, "b.go")
	assert.Contains(t, prompt, expectedFilesHeader, "prompt missing expected files header, want %q", expectedFilesHeader)

	// Check file paths are listed (should be sorted)
	assert.Contains(t, prompt, "- `"+filepath.Join(repoRoot, "a.go")+"`", "prompt missing a.go path")
	assert.Contains(t, prompt, "- `"+filepath.Join(repoRoot, "b.go")+"`", "prompt missing b.go path")

	// Check paths are in sorted order
	aIdx := strings.Index(prompt, "a.go")
	bIdx := strings.Index(prompt, "b.go")
	assert.Less(t, aIdx, bIdx, "paths should be in sorted order (a.go before b.go)")

	// Check for the "too large" message
	assert.Contains(t, prompt, "too large to embed", "prompt missing 'too large' explanation")

	// Check instructions section
	assert.Contains(t, prompt, "## Instructions", "prompt missing '## Instructions' header")
}
