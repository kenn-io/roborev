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
