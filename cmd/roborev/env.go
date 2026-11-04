package main

import (
	"strings"

	"go.kenn.io/roborev/internal/procutil"
)

func isGitRepoEnvKey(entry string) bool {
	return procutil.IsGitRepoEnvKey(entry)
}

func filterGitEnv(env []string) []string {
	return procutil.FilterGitEnv(env)
}

func isGoTestBinaryPath(exePath string) bool {
	return procutil.IsGoTestBinaryPath(exePath)
}

func shouldRefuseAutoStartDaemon(exePath string) bool {
	return procutil.ShouldRefuseAutoStartDaemon(exePath)
}

// Detached daemons must not inherit the service identity of the calling CLI.
func filterDaemonEnv(env []string) []string {
	filtered := filterGitEnv(env)
	result := make([]string, 0, len(filtered))
	for _, entry := range filtered {
		if !strings.HasPrefix(entry, "XPC_SERVICE_NAME=") {
			result = append(result, entry)
		}
	}
	return result
}
