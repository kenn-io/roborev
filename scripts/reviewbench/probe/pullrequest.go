//go:build !nopullrequest

package main

import "go.kenn.io/roborev/internal/prompt"

// withPullRequest shows the pull request's title and description to the
// reviewer, as roborev's CI reviews do.
func withPullRequest(b *prompt.Builder, pr pullRequest) *prompt.Builder {
	return b.WithPullRequest(prompt.PullRequest{Title: pr.Title, Description: pr.Body})
}
