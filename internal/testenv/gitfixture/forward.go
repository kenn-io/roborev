package gitfixture

import (
	"context"
	"os"
)

// Forward applies fixture config defaults and runs Git with the supplied arguments.
func Forward(ctx context.Context, metadata Metadata, args []string) int {
	if _, explicit := os.LookupEnv("GIT_CONFIG_GLOBAL"); !explicit &&
		os.Getenv("HOME") == metadata.Home &&
		os.Getenv("USERPROFILE") == metadata.UserProfile {
		_ = os.Setenv("GIT_CONFIG_GLOBAL", metadata.Global)
	}
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return runFixtureGit(ctx, metadata.RealGit, args)
}
