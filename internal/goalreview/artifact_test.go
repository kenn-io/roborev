package goalreview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeArtifact(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestResolveArtifactsRejectsNonUTF8Path(t *testing.T) {
	root := t.TempDir()
	name := "design-\xff.md"
	_, err := ResolveArtifacts(root, Selection{SpecFile: name})
	require.ErrorContains(t, err, "UTF-8")
}

const (
	specPath = "docs/superpowers/specs/feature-design.md"
	planPath = "docs/superpowers/plans/feature.md"
)

func TestResolveArtifacts(t *testing.T) {
	t.Run("long spec only then linked plan", func(t *testing.T) {
		root := t.TempDir()
		content := "# Feature\n" + strings.Repeat("x", 4001)
		writeArtifact(t, root, specPath, content)
		artifacts, err := ResolveArtifacts(root, Selection{})
		require.NoError(t, err)
		assert.Equal(t, Artifact{Kind: "spec", Path: specPath, Content: content}, artifacts.Spec)
		assert.Nil(t, artifacts.Plan)
		plan := "# Feature Plan\n\n**Spec:** `" + specPath + "`\n\n### Task 1: Change\n"
		writeArtifact(t, root, planPath, plan)
		artifacts, err = ResolveArtifacts(root, Selection{})
		require.NoError(t, err)
		require.NotNil(t, artifacts.Plan)
		assert.Equal(t, Artifact{Kind: "plan", Path: planPath, Content: plan}, *artifacts.Plan)
	})

	t.Run("custom spec path requires explicit selection", func(t *testing.T) {
		root := t.TempDir()
		writeArtifact(t, root, "design/custom.md", "# Custom design\n")
		writeArtifact(t, root, "docs/plans/custom.md", "**Spec:** [design](design/custom.md)\n")
		_, err := ResolveArtifacts(root, Selection{PlanFile: "docs/plans/custom.md"})
		require.ErrorContains(t, err, "--spec")
		artifacts, err := ResolveArtifacts(root, Selection{SpecFile: "design/custom.md", PlanFile: "docs/plans/custom.md"})
		require.NoError(t, err)
		assert.Equal(t, "design/custom.md", artifacts.Spec.Path)
		require.NotNil(t, artifacts.Plan)
		assert.Equal(t, "docs/plans/custom.md", artifacts.Plan.Path)
	})

	t.Run("plan cannot select an arbitrary checkout file", func(t *testing.T) {
		root := t.TempDir()
		writeArtifact(t, root, ".env", "CREDENTIAL=secret\n")
		writeArtifact(t, root, planPath, "**Spec:** `.env`\n")
		_, err := ResolveArtifacts(root, Selection{PlanFile: planPath})
		require.ErrorContains(t, err, "--spec")
	})

	t.Run("symlinked specs are rejected", func(t *testing.T) {
		root := t.TempDir()
		writeArtifact(t, root, ".env", "CREDENTIAL=secret\n")
		require.NoError(t, os.MkdirAll(filepath.Join(root, "docs/superpowers/specs"), 0o700))
		require.NoError(t, os.Symlink(filepath.Join(root, ".env"), filepath.Join(root, specPath)))
		writeArtifact(t, root, planPath, "**Spec:** `"+specPath+"`\n")
		_, err := ResolveArtifacts(root, Selection{PlanFile: planPath})
		require.Error(t, err)
	})

	t.Run("symlinked spec directories are rejected", func(t *testing.T) {
		root := t.TempDir()
		writeArtifact(t, root, "safe/feature-design.md", "# Feature\n")
		require.NoError(t, os.MkdirAll(filepath.Join(root, "docs/superpowers"), 0o700))
		require.NoError(t, os.Symlink(filepath.Join(root, "safe"), filepath.Join(root, "docs/superpowers/specs")))
		writeArtifact(t, root, planPath, "**Spec:** `"+specPath+"`\n")
		_, err := ResolveArtifacts(root, Selection{PlanFile: planPath})
		require.Error(t, err)
	})

	t.Run("older plan requires explicit spec", func(t *testing.T) {
		root := t.TempDir()
		writeArtifact(t, root, specPath, "# Feature\n")
		writeArtifact(t, root, planPath, "# Older plan\n")
		_, err := ResolveArtifacts(root, Selection{PlanFile: planPath})
		require.Error(t, err)
		_, err = ResolveArtifacts(root, Selection{SpecFile: specPath, PlanFile: planPath})
		require.NoError(t, err)
	})

	t.Run("ambiguous history never picks newest", func(t *testing.T) {
		root := t.TempDir()
		writeArtifact(t, root, specPath, "# Feature\n")
		writeArtifact(t, root, "docs/superpowers/specs/other-design.md", "# Other\n")
		_, err := ResolveArtifacts(root, Selection{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--spec")
		for _, name := range []string{planPath, "docs/superpowers/plans/other.md"} {
			writeArtifact(t, root, name, "**Spec:** "+specPath+"\n")
		}
		_, err = ResolveArtifacts(root, Selection{SpecFile: specPath})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--plan")
	})

	t.Run("unrelated plans are ignored", func(t *testing.T) {
		root := t.TempDir()
		writeArtifact(t, root, specPath, "# Feature\n")
		writeArtifact(t, root, planPath, "**Spec:** other.md\n")
		artifacts, err := ResolveArtifacts(root, Selection{SpecFile: specPath})
		require.NoError(t, err)
		assert.Nil(t, artifacts.Plan)
	})

	t.Run("goal and scratch are not artifact selectors", func(t *testing.T) {
		root := t.TempDir()
		writeArtifact(t, root, "GOAL.md", "# Goal\n")
		writeArtifact(t, root, ".superpowers/sdd/feature/plan-path", planPath)
		_, err := ResolveArtifacts(root, Selection{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--spec")
	})

	for name, header := range map[string]string{
		"fenced header ignored":       "```md\n**Spec:** wrong.md\n```\n**Spec:** `" + specPath + "`\n",
		"metadata after task ignored": "**Spec:** " + specPath + "\n### Task 1: Work\n**Spec:** wrong.md\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeArtifact(t, root, specPath, "# Feature\n")
			writeArtifact(t, root, planPath, header)
			artifacts, err := ResolveArtifacts(root, Selection{PlanFile: planPath})
			require.NoError(t, err)
			assert.Equal(t, specPath, artifacts.Spec.Path)
		})
	}
	for name, header := range map[string]string{
		"conflicting pair":     "**Spec:** other.md\n",
		"duplicate references": "**Spec:** " + specPath + "\n**Spec:** " + specPath + "\n",
		"URL reference":        "**Spec:** [design](https://example.com/spec.md)\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeArtifact(t, root, specPath, "# Feature\n")
			writeArtifact(t, root, "other.md", "# Other\n")
			writeArtifact(t, root, planPath, header)
			_, err := ResolveArtifacts(root, Selection{SpecFile: specPath, PlanFile: planPath})
			require.Error(t, err)
		})
	}
}

func TestResolveArtifactsInputBounds(t *testing.T) {
	for name, content := range map[string]string{
		"empty": "\n ", "invalid UTF8": "\xff",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeArtifact(t, root, specPath, content)
			_, err := ResolveArtifacts(root, Selection{SpecFile: specPath})
			require.Error(t, err)
		})
	}
	root := t.TempDir()
	writeArtifact(t, root, specPath, strings.Repeat("x", 1024*1024))
	_, err := ResolveArtifacts(root, Selection{SpecFile: filepath.Join(root, specPath)})
	require.NoError(t, err)
	_, err = ResolveArtifacts(root, Selection{SpecFile: "missing.md"})
	require.Error(t, err)
	outside := writeArtifact(t, t.TempDir(), "outside.md", "# Outside\n")
	_, err = ResolveArtifacts(root, Selection{SpecFile: outside})
	require.Error(t, err)
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape.md")))
	_, err = ResolveArtifacts(root, Selection{SpecFile: "escape.md"})
	require.Error(t, err)
}

func TestResolveArtifactsAcceptsAbsolutePathThroughCheckoutAlias(t *testing.T) {
	target := t.TempDir()
	writeArtifact(t, target, specPath, "# Feature\n")
	alias := filepath.Join(t.TempDir(), "checkout")
	require.NoError(t, os.Symlink(target, alias))

	artifacts, err := ResolveArtifacts(alias, Selection{SpecFile: filepath.Join(alias, specPath)})
	require.NoError(t, err)
	assert.Equal(t, specPath, artifacts.Spec.Path)
	assert.Equal(t, "# Feature\n", artifacts.Spec.Content)
}

func TestResolveArtifactsPreservesLargeInput(t *testing.T) {
	root := t.TempDir()
	content := strings.Repeat("complete requested intent\n", 50000) + "final captured requirement\n"
	writeArtifact(t, root, specPath, content)

	artifacts, err := ResolveArtifacts(root, Selection{SpecFile: specPath})
	require.NoError(t, err)
	assert.Equal(t, content, artifacts.Spec.Content)
	snapshot := Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []Artifact{artifacts.Spec}}
	decoded, err := ParseSnapshot(BuildPrompt(snapshot))
	require.NoError(t, err)
	assert.Equal(t, content, decoded.Artifacts[0].Content)
}
