package agent

import "context"

// ReadOnlyReviewAgent guarantees that Review and, when implemented,
// ReviewWithSchema honor WithReadOnlyReview even when unsafe agents are enabled.
// Adapters must retain session resume and exclude mutating tools.
type ReadOnlyReviewAgent interface {
	Agent
	SupportsReadOnlyReview() bool
}

// WithReadOnlyReview forces repository inspection permissions for this invocation.
// It neither clears session IDs nor changes the process-wide unsafe setting.
// Callers must first check ReadOnlyReviewAgent; unsupported adapters must not run.
func WithReadOnlyReview(ctx context.Context) context.Context {
	return context.WithValue(ctx, planningContextKey{}, true)
}

func (*CodexAgent) SupportsReadOnlyReview() bool  { return true }
func (*ClaudeAgent) SupportsReadOnlyReview() bool { return true }
func (*PiAgent) SupportsReadOnlyReview() bool     { return true }
func (*GrokAgent) SupportsReadOnlyReview() bool   { return true }
func (*TestAgent) SupportsReadOnlyReview() bool   { return true }
