package main

import (
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
