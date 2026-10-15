package prompt

import (
	"os"
	"path/filepath"
	"strings"
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

		section := buildToolchainSection(dir, "", []string{"main.go"})

		assert.Contains(t, section, "- `go.mod`: Go 1.27.0\n")
		assert.Contains(t, section, "## Toolchain")
	})

	t.Run("go.mod change alone triggers", func(t *testing.T) {
		dir := t.TempDir()
		writeGoMod(t, dir, "module example.com/app\n\ngo 1.22\n")

		section := buildToolchainSection(dir, "", []string{"go.mod"})

		assert.Contains(t, section, "- `go.mod`: Go 1.22\n")
	})

	t.Run("minor-only version", func(t *testing.T) {
		dir := t.TempDir()
		writeGoMod(t, dir, "module example.com/app\n\ngo 1.21\n")

		section := buildToolchainSection(dir, "", []string{"pkg/pkg.go"})

		assert.Contains(t, section, "- `go.mod`: Go 1.21\n")
	})

	t.Run("non-go change omitted", func(t *testing.T) {
		dir := t.TempDir()
		writeGoMod(t, dir, "module example.com/app\n\ngo 1.27.0\n")

		assert.Empty(t, buildToolchainSection(dir, "", []string{"notes.md", "main_test.ts"}))
	})

	t.Run("missing go.mod omitted", func(t *testing.T) {
		assert.Empty(t, buildToolchainSection(t.TempDir(), "", []string{"main.go"}))
	})

	t.Run("go.mod without go directive omitted", func(t *testing.T) {
		dir := t.TempDir()
		writeGoMod(t, dir, "module example.com/app\n\nrequire example.com/dep v1.0.0\n")

		assert.Empty(t, buildToolchainSection(dir, "", []string{"main.go"}))
	})

	t.Run("malformed go.mod omitted", func(t *testing.T) {
		dir := t.TempDir()
		writeGoMod(t, dir, "this is not a go.mod\n\x00\n")

		assert.Empty(t, buildToolchainSection(dir, "", []string{"main.go"}))
	})

	t.Run("nested module without root module", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o755))
		writeGoMod(t, filepath.Join(dir, "nested"), "module example.com/nested\n\ngo 1.21\n")

		assert.Contains(t, buildToolchainSection(dir, "", []string{"nested/go.mod"}), "- `nested/go.mod`: Go 1.21\n")
	})

	t.Run("unreadable nested manifest does not inherit root version", func(t *testing.T) {
		dir := t.TempDir()
		writeGoMod(t, dir, "module example.com/root\n\ngo 1.27.0\n")
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "nested", "go.mod"), 0o755))

		assert.Empty(t, buildToolchainSection(dir, "", []string{"nested/main.go"}))
	})

	for _, content := range []string{"module example.com/nested\n", "invalid go.mod\n"} {
		t.Run("nested module does not inherit root version/"+content, func(t *testing.T) {
			dir := t.TempDir()
			writeGoMod(t, dir, "module example.com/root\n\ngo 1.27.0\n")
			require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o755))
			writeGoMod(t, filepath.Join(dir, "nested"), content)

			assert.Empty(t, buildToolchainSection(dir, "", []string{"nested/main.go"}))
		})
	}
}

func TestPromptToolchainUsesReviewedRevision(t *testing.T) {
	r := newGoldenTestRepo(t)
	base := r.commitFile("go.mod", "module example.com/app\n\ngo 1.21\n", "add module")
	target := r.commitFile("main.go", "package main\nfunc main() {}\n", "add main")
	r.commitFile("go.mod", "module example.com/app\n\ngo 1.26\n", "update module")
	r.writeFile("go.mod", "module example.com/app\n\ngo 1.27.0\n")
	b := NewBuilder(nil).ForRepo(r.dir, 0)

	for _, tc := range []struct {
		name, ref, want string
	}{
		{"single", target, "- `go.mod`: Go 1.21\n"},
		{"range", base + ".." + target, "- `go.mod`: Go 1.21\n"},
		{"three-dot range", base + "..." + target, "- `go.mod`: Go 1.21\n"},
		{"implicit HEAD", base + "..", "- `go.mod`: Go 1.26\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := b.Build(tc.ref, 0, "test", "", "")
			require.NoError(t, err)
			assert.Contains(t, p, tc.want)
			assert.NotContains(t, p, "Go 1.27.0")
		})
	}

	p, err := b.BuildDirtyWithFiles("", []string{"go.mod"}, 0, "test", "", "")
	require.NoError(t, err)
	assert.Contains(t, p, "- `go.mod`: Go 1.27.0\n")
}

func TestPromptToolchainUsesOwningModules(t *testing.T) {
	assert := assert.New(t)
	r := newGoldenTestRepo(t)
	r.commitFile("go.mod", "module example.com/root\n\ngo 1.27.0\n", "root module")
	require.NoError(t, os.MkdirAll(filepath.Join(r.dir, "nested", "pkg"), 0o755))
	base := r.commitFile("nested/go.mod", "module example.com/nested\n\ngo 1.21\n", "nested module")
	r.writeFile("main.go", "package main\n")
	r.writeFile("nested/pkg/a.go", "package pkg\n")
	target := r.commitFile("nested/pkg/b.go", "package pkg\n", "nested source")
	b := NewBuilder(nil).ForRepo(r.dir, 0)

	// The checkout no longer has the nested module, but the reviewed revision does.
	require.NoError(t, os.Remove(filepath.Join(r.dir, "nested", "go.mod")))
	for _, ref := range []string{target, base + ".." + target} {
		p, err := b.Build(ref, 0, "test", "", "")
		require.NoError(t, err)
		assert.Contains(p, "- `nested/go.mod`: Go 1.21\n")
		assert.NotContains(p, "Go 1.27.0")
	}

	r.writeFile("nested/go.mod", "module example.com/nested\n\ngo 1.22\n")
	p, err := b.BuildDirtyWithFiles("", []string{"main.go", "nested/pkg/a.go", "nested/pkg/b.go"}, 0, "test", "", "")
	require.NoError(t, err)
	assert.Contains(p, "- `go.mod`: Go 1.27.0\n- `nested/go.mod`: Go 1.22\n")
	assert.Equal(1, strings.Count(p, "- `nested/go.mod`:"))
}
