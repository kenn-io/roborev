package agenthook

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

func TestValidateDroidHooksPathRejectsProjectConfig(t *testing.T) {
	repo := testutil.NewGitRepo(t)

	err := validateDroidHooksPath(filepath.Join(repo.Path(), ".factory", "hooks.json"))

	require.Error(t, err)
	assert.ErrorContains(t, err, "project-scoped")
}

func TestValidateDroidHooksPathAllowsUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	assert.NoError(t, validateDroidHooksPath(filepath.Join(home, ".factory", "hooks.json")))
}
