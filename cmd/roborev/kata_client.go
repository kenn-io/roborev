package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/kata"
	roborevclient "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

// newKataClient returns the Kata client for a CLI command running in workdir.
//
// When the daemon's [integrations.kata] connection is enabled, reads and
// writes go through the daemon: it holds the credential and the endpoint, so a
// CLI that tunnels to another machine's local daemon API needs neither.
// Signed HTTPS remote access does not expose Kata operations. Otherwise the
// kata CLI is run exactly as before. Which applies is decided on first use,
// so commands that never need Kata contact nothing.
func newKataClient(workdir string, cfg *config.Config) kata.Client {
	return newKataClientAt(workdir, cfg, resolveDaemonEndpoint)
}

// newKataClientAt is newKataClient with the daemon endpoint supplied, for
// tests.
func newKataClientAt(workdir string, cfg *config.Config, endpoint func() (daemon.DaemonEndpoint, error)) kata.Client {
	return &lazyKataClient{workdir: workdir, cfg: cfg, endpoint: endpoint}
}

// lazyKataClient picks between the daemon's connection and the kata CLI the
// first time Kata is used.
type lazyKataClient struct {
	workdir  string
	cfg      *config.Config
	endpoint func() (daemon.DaemonEndpoint, error)
	// statusProbe replaces the initial request in tests that simulate a stalled
	// daemon.
	statusProbe func(context.Context) (*generated.KataStatusResponse, error)

	once   sync.Once
	chosen kata.Client
}

// The daemon status route makes two serial Kata requests: server info and a
// project lookup. Each uses Kata v0.19.0's five-second non-streaming budget
// (cmd/kata/client.go defaultHTTPTimeout, from internal/client/client.go
// DefaultHTTPTimeout). Bound the one-time probe to both requests, while
// leaving later API calls under their caller's contexts.
const kataStatusProbeTimeout = 2 * kata.DefaultRequestTimeout

var _ kata.Client = (*lazyKataClient)(nil)

// resolve decides once. The daemon is asked, never started: a CLI that has no
// daemon running behaves as it always has unless its own config enables the
// connection, in which case Kata is unavailable rather than silently run
// through the kata CLI.
func (c *lazyKataClient) resolve(ctx context.Context) kata.Client {
	c.once.Do(func() {
		localEnabled := c.cfg != nil && c.cfg.Integrations.Kata.Enabled
		var (
			api       *roborevclient.Client
			status    *generated.KataStatusResponse
			reachErr  error
			daemonOff bool
		)
		if ep, err := c.endpoint(); err != nil {
			reachErr = err
		} else {
			api = ep.APIClient(0)
			probeCtx, cancel := context.WithTimeout(ctx, kataStatusProbeTimeout)
			if c.statusProbe != nil {
				status, reachErr = c.statusProbe(probeCtx)
			} else {
				status, reachErr = api.KataStatus(probeCtx, &generated.KataStatusRequestOptions{})
			}
			probeTimedOut := errors.Is(probeCtx.Err(), context.DeadlineExceeded)
			cancel()
			switch {
			case ctx.Err() != nil:
				reachErr = ctx.Err()
			case probeTimedOut && errors.Is(reachErr, context.DeadlineExceeded):
				// The optional status check has its own budget. Do not make its
				// deadline look like cancellation of the caller's review context.
				reachErr = errors.New("kata status probe timed out")
			}
			if reachErr == nil {
				daemonOff = !status.Enabled
			}
		}
		switch {
		case reachErr == nil && !daemonOff:
			c.chosen = &daemonKataClient{api: api, workdir: c.workdir, defaultProject: deref(status.DefaultProject)}
		case !localEnabled:
			c.chosen = kata.NewCLIClient(c.workdir)
		case reachErr != nil:
			c.chosen = failedKataClient{err: kataProbeFailure(reachErr)}
		default:
			c.chosen = failedKataClient{err: errors.New("[integrations.kata] is enabled in this config but disabled on the daemon; enable it there")}
		}
	})
	return c.chosen
}

// kataProbeFailure says why the daemon could not provide the connection. A
// daemon that answers with an error is running: one that predates the status
// endpoint has no such route and answers 404, usually as plain text, and needs
// a restart, which "could not be reached" would hide.
func kataProbeFailure(err error) error {
	status, answered := daemonAnswerStatus(err)
	switch {
	case !answered:
		return fmt.Errorf("the Kata connection runs in the daemon, which could not be reached: %w", err)
	case status == http.StatusNotFound:
		return errors.New("the daemon predates the Kata connection; restart it so it runs this version of roborev")
	}
	if _, ok := errors.AsType[generated.ErrorModel](err); ok {
		return daemonRequestError("the daemon answered the Kata status request with an error", err)
	}
	return fmt.Errorf("the daemon answered the Kata status request with HTTP %d", status)
}

// daemonAnswerStatus returns the HTTP status of a reply the generated client
// could not use, whether the body was the API's error model or something else.
func daemonAnswerStatus(err error) (int, bool) {
	if problem, ok := errors.AsType[generated.ErrorModel](err); ok && problem.Status != nil {
		return int(*problem.Status), true
	}
	if decode, ok := errors.AsType[*runtime.ResponseDecodeError](err); ok {
		return decode.StatusCode, true
	}
	return 0, false
}

func (c *lazyKataClient) Binding(ctx context.Context) (kata.Binding, error) {
	return c.resolve(ctx).Binding(ctx)
}

func (c *lazyKataClient) List(ctx context.Context, opts kata.ListOpts) ([]kata.Issue, error) {
	return c.resolve(ctx).List(ctx, opts)
}

func (c *lazyKataClient) Show(ctx context.Context, ref string) (kata.Issue, error) {
	return c.resolve(ctx).Show(ctx, ref)
}

func (c *lazyKataClient) Create(ctx context.Context, req kata.CreateReq) (kata.CreateResult, error) {
	return c.resolve(ctx).Create(ctx, req)
}

// failedKataClient reports one error from every call.
type failedKataClient struct{ err error }

func (c failedKataClient) Binding(context.Context) (kata.Binding, error) {
	return kata.Binding{}, c.err
}

func (c failedKataClient) List(context.Context, kata.ListOpts) ([]kata.Issue, error) {
	return nil, c.err
}

func (c failedKataClient) Show(context.Context, string) (kata.Issue, error) {
	return kata.Issue{}, c.err
}

func (c failedKataClient) Create(context.Context, kata.CreateReq) (kata.CreateResult, error) {
	return kata.CreateResult{}, c.err
}

// daemonKataClient implements kata.Client through the daemon's Kata endpoints.
// The workspace binding is read here, because the daemon may be on another
// host; the daemon applies its default project when there is none.
type daemonKataClient struct {
	api            *roborevclient.Client
	workdir        string
	defaultProject string
}

var _ kata.Client = (*daemonKataClient)(nil)

func (c *daemonKataClient) Binding(ctx context.Context) (kata.Binding, error) {
	b, err := localKataBinding(ctx, c.workdir)
	if err == nil {
		return b, nil
	}
	if errors.Is(err, kata.ErrNoBinding) && c.defaultProject != "" {
		return kata.Binding{Project: c.defaultProject}, nil
	}
	return kata.Binding{}, err
}

func (c *daemonKataClient) List(ctx context.Context, opts kata.ListOpts) ([]kata.Issue, error) {
	b, err := c.Binding(ctx)
	if err != nil {
		return nil, err
	}
	query := &generated.KataListIssuesQuery{Project: &b.Project}
	if opts.Status != "" {
		query.Status = &opts.Status
	}
	if opts.Unlimited {
		query.Unlimited = &opts.Unlimited
	}
	resp, err := c.api.KataListIssues(ctx, &generated.KataListIssuesRequestOptions{Query: query})
	if err != nil {
		return nil, kataDaemonError("list Kata issues through the daemon", err)
	}
	issues := make([]kata.Issue, 0, len(resp.Issues))
	for _, issue := range resp.Issues {
		issues = append(issues, kataIssueFromDaemon(issue))
	}
	return issues, nil
}

func (c *daemonKataClient) Show(ctx context.Context, ref string) (kata.Issue, error) {
	query := &generated.KataShowIssueQuery{Ref: ref}
	if !strings.Contains(ref, "#") {
		b, err := c.Binding(ctx)
		if err != nil {
			return kata.Issue{}, err
		}
		query.Project = &b.Project
	}
	resp, err := c.api.KataShowIssue(ctx, &generated.KataShowIssueRequestOptions{Query: query})
	if err != nil {
		return kata.Issue{}, kataDaemonError("show a Kata issue through the daemon", err)
	}
	return kataIssueFromDaemon(*resp), nil
}

func (c *daemonKataClient) Create(ctx context.Context, req kata.CreateReq) (kata.CreateResult, error) {
	if len(req.Metadata) > 0 {
		return kata.CreateResult{}, kata.ErrMetadataUnsupported
	}
	project := req.Project
	if project == "" {
		b, err := c.Binding(ctx)
		if err != nil {
			return kata.CreateResult{}, err
		}
		project = b.Project
	}
	body := generated.KataCreateRequest{Title: req.Title, Project: &project, Labels: req.Labels}
	if req.Body != "" {
		body.Body = &req.Body
	}
	if req.Priority != nil {
		body.Priority = new(int64(*req.Priority))
	}
	if req.IdempotencyKey != "" {
		body.IdempotencyKey = &req.IdempotencyKey
	}
	resp, err := c.api.KataCreateIssue(ctx, &generated.KataCreateIssueRequestOptions{Body: &body})
	if err != nil {
		return kata.CreateResult{}, kataDaemonError("file a Kata issue through the daemon", err)
	}
	return kata.CreateResult{
		ShortID: resp.ShortID, Reused: resp.Reused,
		Attribution: kata.Attribution{Author: deref(resp.Author)},
	}, nil
}

// localKataBinding reads the repository's .kata.local.toml or .kata.toml
// binding on this machine. It reads files only: it runs nothing and contacts
// nothing.
func localKataBinding(ctx context.Context, workdir string) (kata.Binding, error) {
	return kata.NewCLIClient(workdir).Binding(ctx)
}

// kataDaemonError keeps the daemon's explanation, which already carries
// Kata's own error code, and marks a missing issue or project.
func kataDaemonError(action string, err error) error {
	wrapped := daemonRequestError(action, err)
	if problem, ok := errors.AsType[generated.ErrorModel](err); ok && problem.Status != nil && *problem.Status == 404 {
		return fmt.Errorf("%w: %w", kata.ErrNotFound, wrapped)
	}
	return wrapped
}

func kataIssueFromDaemon(issue generated.KataIssue) kata.Issue {
	out := kata.Issue{
		ShortID: issue.ShortID, QualifiedID: issue.QualifiedID, Title: issue.Title, Body: issue.Body,
		Status: issue.Status, Labels: issue.Labels, Parent: kataPeerFromDaemon(issue.Parent),
		Blocks: kataPeersFromDaemon(issue.Blocks), BlockedBy: kataPeersFromDaemon(issue.BlockedBy),
		Related: kataPeersFromDaemon(issue.Related),
	}
	return out
}

func kataPeerFromDaemon(peer *generated.KataLinkPeer) *kata.LinkPeer {
	if peer == nil {
		return nil
	}
	return &kata.LinkPeer{ShortID: peer.ShortID, Project: deref(peer.Project), QualifiedID: deref(peer.QualifiedID), Status: deref(peer.Status)}
}

func kataPeersFromDaemon(peers []generated.KataLinkPeer) []kata.LinkPeer {
	var out []kata.LinkPeer
	for i := range peers {
		out = append(out, *kataPeerFromDaemon(&peers[i]))
	}
	return out
}
