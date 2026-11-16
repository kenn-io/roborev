package agent

import "context"

type untrustedCheckoutContextKey struct{}

// WithUntrustedCheckout marks agent work whose working directory is a checkout
// written by someone else, such as the head of a pull request under review.
//
// Agent CLIs read project configuration from their working directory, and
// some of it runs commands: Claude Code runs hooks from .claude/settings.json
// and starts the stdio MCP servers in .mcp.json, even under -p. A pull request
// author who adds those files would run code in the review process, which
// holds the provider credentials. Adapters that see this mark load only user
// configuration. WithCIReview sets it as well.
func WithUntrustedCheckout(ctx context.Context) context.Context {
	return context.WithValue(ctx, untrustedCheckoutContextKey{}, true)
}

func untrustedCheckout(ctx context.Context) bool {
	untrusted, _ := ctx.Value(untrustedCheckoutContextKey{}).(bool)
	return untrusted
}
