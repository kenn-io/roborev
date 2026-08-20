package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
)

// registerAgentCompletion registers shell completion for the --agent flag.
// Panics if the flag doesn't exist on the command (programming error).
func registerAgentCompletion(cmd *cobra.Command) {
	if err := cmd.RegisterFlagCompletionFunc("agent", func(cmd *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		cfg, err := config.LoadGlobal()
		if err != nil {
			return agent.Available(), cobra.ShellCompDirectiveNoFileComp
		}
		repoCfg, _, _ := loadCommandRepoConfig(cmd)
		return agent.AvailableNamesFromConfig(repoCfg, cfg), cobra.ShellCompDirectiveNoFileComp
	}); err != nil {
		panic(fmt.Sprintf("registering agent completion for %s: %v", cmd.Name(), err))
	}
}

// loadCommandRepoConfig resolves an explicit command --repo first, then the
// repository containing the current directory. Outside a repository it
// returns a nil repo config so callers naturally use global configuration.
func loadCommandRepoConfig(cmd *cobra.Command) (*config.RepoConfig, string, error) {
	repoPath := ""
	if cmd != nil && cmd.Flags().Lookup("repo") != nil {
		value, err := cmd.Flags().GetString("repo")
		if err != nil {
			return nil, "", err
		}
		repoPath = value
	}
	if repoPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, "", err
		}
		repoPath = cwd
	}
	root, err := findRepoRootFrom(repoPath)
	if err != nil {
		if errors.Is(err, errNotGitRepository) {
			return nil, repoPath, nil
		}
		return nil, "", err
	}
	repoPath = root
	repoCfg, err := config.LoadRepoConfig(repoPath)
	return repoCfg, repoPath, err
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
	if err := cmd.RegisterFlagCompletionFunc("type", func(cmd *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		globalCfg, _ := config.LoadGlobal()
		repoCfg, _, _ := loadCommandRepoConfig(cmd)
		types := config.ReviewTypesFromConfig(repoCfg, globalCfg)
		completions := make([]cobra.Completion, len(types))
		for i, t := range types {
			completions[i] = cobra.Completion(t)
		}
		return completions, cobra.ShellCompDirectiveNoFileComp
	}); err != nil {
		panic(fmt.Sprintf("registering review type completion for %s: %v", cmd.Name(), err))
	}
}
