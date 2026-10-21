//go:build nopullrequest

package main

import "go.kenn.io/roborev/internal/prompt"

// withPullRequest leaves the prompt unchanged for roborev refs that predate
// pull request context; the eval script sets the nopullrequest tag for them.
func withPullRequest(b *prompt.Builder, _ pullRequest) *prompt.Builder {
	return b
}
