package kata

import (
	"context"

	"go.kenn.io/roborev/internal/config"
)

// NewFromConfig returns the Kata client for workdir. With the daemon's
// [integrations.kata] connection enabled it is the API client; otherwise it is
// the kata CLI client with exactly the routing it has always had.
//
// A connection that is enabled but cannot be built (a bad endpoint, an
// unreadable credential file) returns a client whose every call fails with that
// error. It never falls back to the CLI, which would send the work to whatever
// Kata the process environment selects instead of the one configured.
func NewFromConfig(workdir string, cfg *config.Config) Client {
	if cfg == nil || !cfg.Integrations.Kata.Enabled {
		return NewCLIClient(workdir)
	}
	client, err := NewAPIClient(workdir, cfg.Integrations.Kata)
	if err != nil {
		return failedClient{err: err}
	}
	return client
}

// failedClient reports one construction error from every call.
type failedClient struct{ err error }

var _ Client = failedClient{}

func (c failedClient) Binding(context.Context) (Binding, error)        { return Binding{}, c.err }
func (c failedClient) List(context.Context, ListOpts) ([]Issue, error) { return nil, c.err }
func (c failedClient) Show(context.Context, string) (Issue, error)     { return Issue{}, c.err }
func (c failedClient) Create(context.Context, CreateReq) (CreateResult, error) {
	return CreateResult{}, c.err
}

func (c failedClient) Comment(context.Context, CommentReq) (CommentResult, error) {
	return CommentResult{}, c.err
}
func (c failedClient) Thread(context.Context, ThreadReq) (Thread, error) { return Thread{}, c.err }
