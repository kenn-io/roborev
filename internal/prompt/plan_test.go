package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"

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

func TestStoredPlansAreNotImplementationAttempts(t *testing.T) {
	assert := assert.New(t)
	b := NewBuilder(nil).ForRepo(t.TempDir(), 0)
	responses := []storage.Response{{Responder: "roborev-plan", Response: "Proposed cancellation check"}}
	review := &storage.Review{JobID: 7, Output: "Missing cancellation check"}
	address, err := b.BuildAddressPrompt(review, responses, "")
	require.NoError(t, err)
	planning, err := b.BuildPlanPrompt(review, responses, "")
	require.NoError(t, err)
	for _, body := range []string{address, planning} {
		assert.Contains(body, "## Previous Plans")
		assert.Contains(body, "Proposed cancellation check")
		assert.NotContains(body, "## Previous Addressing Attempts")
		assert.NotContains(body, "## User Comments")
	}
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
	assert.Equal(t, "## Plan\n\nConcrete ordered steps\n\n"+input, p)
	assert.Less(t, strings.Index(p, "Concrete ordered steps"), strings.Index(p, "Finding"))
	assert.Equal(t, input, WithFixPlan(input, ""))
	b := NewBuilder(nil).ForRepo(t.TempDir(), 0)
	p, err := b.BuildAddressPrompt(&storage.Review{Output: "Finding"}, nil, "")
	require.NoError(t, err)
	p = WithFixPlan(p, "Concrete ordered steps")
	assert.Contains(t, p, "## Plan\n\nConcrete ordered steps")
	assert.Less(t, strings.Index(p, "Concrete ordered steps"), strings.Index(p, "## Review Findings"))
}

func TestFixPlanPreservesEntireAddressPrompt(t *testing.T) {
	b := NewBuilder(nil).ForRepo(t.TempDir(), 0)
	implementationPrompt, err := b.BuildAddressPrompt(
		&storage.Review{JobID: 42, Output: "## Review Findings\n\nCurrent finding"},
		[]storage.Response{{Responder: "agent", Response: "## Review Findings\n\nEarlier attempt"}},
		"",
	)
	require.NoError(t, err)

	got := WithFixPlan(implementationPrompt, "Plan the replacement implementation")
	assert := assert.New(t)
	assert.Equal("## Plan\n\nPlan the replacement implementation\n\n"+implementationPrompt, got)
	assert.Less(strings.Index(got, "## Plan"), strings.Index(got, "## Review Findings to Address (Job 42)"))
}

func TestFixPlanPreservesArbitraryImplementationContext(t *testing.T) {
	property := func(input, strategy string) bool {
		plan := "Proposed strategy: " + strategy
		got := WithFixPlan(input, plan)
		// Exact equality protects even empty input and user-authored headings.
		return assert.Equal(t, "## Plan\n\n"+plan+"\n\n"+input, got)
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}

func TestFixPlanEmptyPlanIsExactIdentity(t *testing.T) {
	property := func(input string) bool {
		return assert.Equal(t, input, WithFixPlan(input, ""))
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}

func TestFixPlanPreservesFindingWithQuotedAddressHeading(t *testing.T) {
	findings := "**Problem:** The renderer prints this literal heading:\n\n" +
		"## Review Findings to Address\n\n" +
		"**Fix:** Preserve the heading as quoted content while correcting the renderer."
	b := NewBuilder(nil).WithContext(t.Context()).ForRepo(t.TempDir(), 0)
	input, err := b.BuildAddressPrompt(&storage.Review{JobID: 42, Output: findings}, nil, "")
	require.NoError(t, err)

	got := WithFixPlan(input, "Repair the renderer without changing its documented heading")
	assert.Contains(t, got, findings, "adding a plan must not split the original finding")
}
