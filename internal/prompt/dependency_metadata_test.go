package prompt

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildDependencyMetadataSectionReportsMissingCompanions(t *testing.T) {
	section := buildDependencyMetadataSection([]string{
		"frontend/package.json",
		"go.mod",
	})

	assert.Contains(t, section, "frontend/package.json changed; no JavaScript lockfile change detected")
	assert.Contains(t, section, "go.mod changed; no go.sum change detected")
}

func TestBuildDependencyMetadataSectionTreatsWorkspaceLockfileAsCompanion(t *testing.T) {
	section := buildDependencyMetadataSection([]string{
		"packages/api/package.json",
		"pnpm-lock.yaml",
	})

	assert.Contains(t, section, "packages/api/package.json changed")
	assert.NotContains(t, section, "no JavaScript lockfile change detected")
	assert.Contains(t, section, "pnpm-lock.yaml changed")
}
