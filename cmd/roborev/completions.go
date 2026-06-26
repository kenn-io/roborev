package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
)

// registerAgentCompletion registers shell completion for the --agent flag.
// Panics if the flag doesn't exist on the command (programming error).
func registerAgentCompletion(cmd *cobra.Command) {
	if err := cmd.RegisterFlagCompletionFunc("agent", func(_ *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return agent.Available(), cobra.ShellCompDirectiveNoFileComp
	}); err != nil {
		panic(fmt.Sprintf("registering agent completion for %s: %v", cmd.Name(), err))
	}
}

// registerReasoningCompletion registers shell completion for the --reasoning flag.
// Panics if the flag doesn't exist on the command (programming error).
func registerReasoningCompletion(cmd *cobra.Command) {
	if err := cmd.RegisterFlagCompletionFunc("reasoning", func(_ *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return agent.ReasoningLevels(), cobra.ShellCompDirectiveNoFileComp
	}); err != nil {
		panic(fmt.Sprintf("registering reasoning completion for %s: %v", cmd.Name(), err))
	}
}

// registerReviewTypeCompletion registers shell completion for the --type flag.
// Panics if the flag doesn't exist on the command (programming error).
func registerReviewTypeCompletion(cmd *cobra.Command) {
	if err := cmd.RegisterFlagCompletionFunc("type", func(_ *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return []cobra.Completion{config.ReviewTypeSecurity, config.ReviewTypeDesign, config.ReviewTypeLookahead}, cobra.ShellCompDirectiveNoFileComp
	}); err != nil {
		panic(fmt.Sprintf("registering review type completion for %s: %v", cmd.Name(), err))
	}
}
