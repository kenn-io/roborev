package prompt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildToolchainSection(t *testing.T) {
	writeGoMod := func(t *testing.T, dir, content string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644))
	}

	t.Run("go file change states declared version", func(t *testing.T) {
		dir := t.TempDir()
		writeGoMod(t, dir, "module example.com/app\n\ngo 1.27.0\n")

		section := buildToolchainSection(dir, []string{"main.go"})

		assert.Contains(t, section, "This project uses Go 1.27.0")
		assert.Contains(t, section, "## Toolchain")
	})

	t.Run("go.mod change alone triggers", func(t *testing.T) {
		dir := t.TempDir()
		writeGoMod(t, dir, "module example.com/app\n\ngo 1.22\n")

		section := buildToolchainSection(dir, []string{"nested/go.mod"})

		assert.Contains(t, section, "This project uses Go 1.22")
	})

	t.Run("minor-only version", func(t *testing.T) {
		dir := t.TempDir()
		writeGoMod(t, dir, "module example.com/app\n\ngo 1.21\n")

		section := buildToolchainSection(dir, []string{"pkg/pkg.go"})

		assert.Contains(t, section, "This project uses Go 1.21")
	})

	t.Run("non-go change omitted", func(t *testing.T) {
		dir := t.TempDir()
		writeGoMod(t, dir, "module example.com/app\n\ngo 1.27.0\n")

		assert.Empty(t, buildToolchainSection(dir, []string{"notes.md", "main_test.ts"}))
	})

	t.Run("missing go.mod omitted", func(t *testing.T) {
		assert.Empty(t, buildToolchainSection(t.TempDir(), []string{"main.go"}))
	})

	t.Run("go.mod without go directive omitted", func(t *testing.T) {
		dir := t.TempDir()
		writeGoMod(t, dir, "module example.com/app\n\nrequire example.com/dep v1.0.0\n")

		assert.Empty(t, buildToolchainSection(dir, []string{"main.go"}))
	})

	t.Run("malformed go.mod omitted", func(t *testing.T) {
		dir := t.TempDir()
		writeGoMod(t, dir, "this is not a go.mod\n\x00\n")

		assert.Empty(t, buildToolchainSection(dir, []string{"main.go"}))
	})
}
