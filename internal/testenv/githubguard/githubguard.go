// Package githubguard installs the test-only GitHub API HTTP guard.
package githubguard

import "go.kenn.io/roborev/internal/testenv"

func init() {
	testenv.InstallGitHubAPIGuard()
}
