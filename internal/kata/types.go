// Package kata integrates roborev with the kata task ledger via the kata CLI.
package kata

import (
	"context"
	"errors"
)

// RoborevLabel marks kata issues filed by roborev (e.g. the review-findings
// hook). Open-mode context resolution skips issues carrying it so review
// findings are not fed back into later review prompts as task intent.
const RoborevLabel = "roborev"

// Sentinel errors let callers tell "no kata here" apart from "kata failed".
var (
	// ErrUnavailable means the kata binary was not found on PATH.
	ErrUnavailable = errors.New("kata: binary not found on PATH")
	// ErrNoBinding means no .kata.toml binding was found for the workdir.
	ErrNoBinding = errors.New("kata: no .kata.toml binding for workdir")
)

// Issue is the subset of a kata issue roborev consumes. Field names match the
// kata daemon JSON wire schema. QualifiedID is only present on list rows.
type Issue struct {
	ShortID     string   `json:"short_id"`
	QualifiedID string   `json:"qualified_id"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Status      string   `json:"status"`
	Labels      []string `json:"labels"`
}

// Binding identifies the kata project a workspace is bound to.
type Binding struct {
	Project string
}

// ListOpts controls a List call.
type ListOpts struct {
	Status string // "open" (default) | "closed" | "all"
}

// CreateReq describes an issue to create.
type CreateReq struct {
	Title          string
	Body           string
	Project        string // optional; overrides the .kata.toml binding
	Labels         []string
	Priority       *int // nil = let kata decide
	IdempotencyKey string
}

// CreateResult reports the outcome of a Create. The create response carries
// short_id but not qualified_id.
type CreateResult struct {
	ShortID string
	Reused  bool
}

// Client is the kata interface roborev depends on.
type Client interface {
	Binding(ctx context.Context) (Binding, error)
	List(ctx context.Context, opts ListOpts) ([]Issue, error)
	Show(ctx context.Context, ref string) (Issue, error)
	Create(ctx context.Context, req CreateReq) (CreateResult, error)
}
