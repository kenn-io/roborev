package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewTypeFlagCompletion(t *testing.T) {
	cmd := reviewCmd()

	completion, ok := cmd.GetFlagCompletionFunc("type")
	require.True(t, ok)

	got, directive := completion(cmd, nil, "")

	assert.ElementsMatch(t, []cobra.Completion{"security", "design", "lookahead"}, got)
	assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, directive)
}

func TestAgentFlagCompletionIncludesNamedACPAgents(t *testing.T) {
	env := setupConfigEnv(t,
		"[acp.goose]\ncommand = 'goose'\n",
		"[acp.foo]\ncommand = 'foo-acp'\n",
	)
	t.Chdir(env.RepoDir)
	cmd := reviewCmd()

	completion, ok := cmd.GetFlagCompletionFunc("agent")
	require.True(t, ok)
	got, directive := completion(cmd, nil, "")

	assert.Contains(t, got, cobra.Completion("goose"))
	assert.Contains(t, got, cobra.Completion("foo"))
	assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, directive)
}

func TestAgentFlagCompletionDiscoversRepoACPAgentFromNestedDirectory(t *testing.T) {
	env := setupConfigEnv(t, "", "[acp.goose]\ncommand = 'goose'\n")
	nested := filepath.Join(env.RepoDir, "nested", "directory")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	t.Chdir(nested)
	cmd := reviewCmd()

	completion, ok := cmd.GetFlagCompletionFunc("agent")
	require.True(t, ok)
	got, directive := completion(cmd, nil, "")

	assert.Contains(t, got, cobra.Completion("goose"))
	assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, directive)
}

func TestAgentFlagCompletionUsesCommandRepoFlag(t *testing.T) {
	env := setupConfigEnv(t, "", "[acp.goose]\ncommand = 'goose'\n")
	other := setupConfigEnv(t, "", "[acp.foo]\ncommand = 'foo-acp'\n")
	t.Chdir(env.RepoDir)
	cmd := reviewCmd()
	require.NoError(t, cmd.Flags().Set("repo", other.RepoDir))

	completion, ok := cmd.GetFlagCompletionFunc("agent")
	require.True(t, ok)
	got, directive := completion(cmd, nil, "")

	assert.Contains(t, got, cobra.Completion("foo"))
	assert.NotContains(t, got, cobra.Completion("goose"))
	assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, directive)
}
