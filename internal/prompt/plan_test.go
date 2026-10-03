package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func TestFixPlanContext(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "REVIEW.md"), []byte("Preserve cancellation behavior"), 0o600))
	p, err := BuildFixPlanPrompt(repo, &config.Config{FixGuidelines: "Verify findings before changing code"}, "Missing cancellation check", "high", []storage.Response{
		{Responder: "roborev-plan", Response: "Earlier proposed approach"},
		{Responder: "developer", Response: "Keep the public API"},
	}, "Check the worker")
	require.NoError(t, err)
	a := assert.New(t)
	a.Contains(p, "Preserve cancellation behavior")
	a.Contains(p, "Verify findings before changing code")
	a.Contains(p, "Missing cancellation check")
	a.Contains(p, "Earlier proposed approach")
	a.Contains(p, "Keep the public API")
	a.Contains(p, "Check the worker")
	a.Contains(p, config.SeverityInstruction("high"))
	a.Contains(p, "Root causes")
	a.Contains(p, "Implementation plan")
	a.Contains(p, "Verification and risks")
	a.NotContains(p, "## Superpowers")
}

func TestFixPlanSuperpowersSymlinks(t *testing.T) {
	repo := t.TempDir()
	for _, name := range []string{"brainstorming", "writing-plans"} {
		source := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("---\nname: "+name+"\n---\nSTOP and ask for approval. Commit a spec."), 0o600))
		target := filepath.Join(repo, ".agents", "skills", "skills__"+name)
		require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o700))
		require.NoError(t, os.Symlink(source, target))
	}
	p, err := BuildFixPlanPrompt(repo, nil, "finding", "", nil, "")
	require.NoError(t, err)
	assert.Contains(t, p, "## Superpowers")
	assert.Contains(t, p, "brainstorming")
	assert.Contains(t, p, "writing-plans")
	assert.NotContains(t, p, "STOP and ask for approval")
	assert.NotContains(t, p, "Commit a spec")
}

func TestFixPlanInjection(t *testing.T) {
	input := "# Fix Request\n\n## Previous Attempts\nOld attempt\n\n## Analysis Findings\nFinding\n\n## Instructions\nApply changes"
	p := WithFixPlan(input, "Concrete ordered steps")
	assert.Less(t, strings.Index(p, "Old attempt"), strings.Index(p, "## Plan"))
	assert.Less(t, strings.Index(p, "Concrete ordered steps"), strings.Index(p, "Finding"))
	assert.Equal(t, input, WithFixPlan(input, ""))
	b := NewBuilder(nil).ForRepo(t.TempDir(), 0)
	p, err := b.BuildAddressPrompt(&storage.Review{Output: "Finding"}, nil, "")
	require.NoError(t, err)
	p = WithFixPlan(p, "Concrete ordered steps")
	assert.Contains(t, p, "## Plan\n\nConcrete ordered steps")
	assert.Less(t, strings.Index(p, "Concrete ordered steps"), strings.Index(p, "## Review Findings"))
}

func TestFixPlanInjectionUsesAddressFindingsSection(t *testing.T) {
	b := NewBuilder(nil).ForRepo(t.TempDir(), 0)
	implementationPrompt, err := b.BuildAddressPrompt(
		&storage.Review{JobID: 42, Output: "## Review Findings\n\nCurrent finding"},
		[]storage.Response{{Responder: "agent", Response: "## Review Findings\n\nEarlier attempt"}},
		"",
	)
	require.NoError(t, err)

	got := WithFixPlan(implementationPrompt, "Plan the replacement implementation")
	assert := assert.New(t)
	assert.Less(strings.Index(got, "Earlier attempt"), strings.Index(got, "## Plan"))
	assert.Less(strings.Index(got, "## Plan"), strings.Index(got, "## Review Findings to Address (Job 42)"))
}
