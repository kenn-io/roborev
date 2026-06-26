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
