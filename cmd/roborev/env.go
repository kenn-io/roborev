package main

import "go.kenn.io/roborev/internal/procutil"

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
