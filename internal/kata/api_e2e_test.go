//go:build kata && !windows

package kata_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// TestAPIClientCurrentProducer checks the published Kata v0.23.0 contract.
// CI installs that exact producer so an incompatible schema cannot be skipped.
func TestAPIClientCurrentProducer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	k := katatest.StartReal(t, ctx)
	cfg := config.KataIntegrationConfig{
		Enabled: true, Endpoint: k.SocketEndpoint(t, ctx), DefaultProject: "e2etest",
	}
	c, err := kata.NewAPIClient(k.Repo, cfg)
	require.NoError(t, err)
	defer c.CloseIdleConnections()

	info, err := c.Info(ctx)
	require.NoError(t, err)
	assert.Equal(t, "0.43.0", info.APISchemaVersion)
}

// TestAPIClientE2E runs the API client against a real Kata daemon in a
// disposable home, reached over its private Unix socket. It covers the calls
// the connection makes: health and project checks, create with an idempotency
// key, list, and show.
func TestAPIClientE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	k := katatest.StartReal(t, ctx)
	cfg := config.KataIntegrationConfig{
		Enabled: true, Endpoint: k.SocketEndpoint(t, ctx), DefaultProject: "e2etest", Teammate: "roborev",
	}
	c, err := kata.NewAPIClient(k.Repo, cfg)
	require.NoError(t, err)
	defer c.CloseIdleConnections()

	_, err = c.Info(ctx)
	require.NoError(t, err)

	b, err := c.Binding(ctx)
	require.NoError(t, err)
	assert.Equal(t, "e2etest", b.Project)

	req := kata.CreateReq{
		Title: "roborev connection test", Body: "created through the API", Labels: []string{"roborev", "retry-check"},
		Priority: new(1), IdempotencyKey: "e2e:api:1",
	}
	created, err := c.Create(ctx, req)
	require.NoError(t, err)
	require.NotEmpty(t, created.ShortID)
	assert.False(t, created.Reused)
	assert.Equal(t, "roborev", created.Attribution.Author, "a local socket has no credential, so the requested actor is recorded")

	reused, err := c.Create(ctx, req)
	require.NoError(t, err)
	assert.True(t, reused.Reused)
	assert.Equal(t, created.ShortID, reused.ShortID)

	// Retry semantics belong to the real producer, including fields the old
	// hand-written fake omitted from its fingerprint.
	for _, change := range []struct {
		name  string
		apply func(*kata.CreateReq)
	}{
		{"title", func(r *kata.CreateReq) { r.Title = "different review" }},
		{"body", func(r *kata.CreateReq) { r.Body = "different body" }},
		{"labels", func(r *kata.CreateReq) { r.Labels = []string{"different"} }},
		{"priority", func(r *kata.CreateReq) { r.Priority = new(2) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := req
			change.apply(&changed)
			_, err := c.Create(ctx, changed)
			require.ErrorIs(t, err, kata.ErrConflict)
			assert.Equal(t, "idempotency_mismatch", kata.ErrorCode(err))
		})
	}

	// Register a second project in the same disposable daemon without changing
	// this client's workspace binding. Keys must be scoped to their project.
	initOther := exec.CommandContext(ctx, "kata", "init", "--project", "e2eother")
	initOther.Env, initOther.Dir = k.Env, t.TempDir()
	out, err := initOther.CombinedOutput()
	require.NoError(t, err, "register second disposable project: %s", out)
	other := req
	other.Project = "e2eother"
	elsewhere, err := c.Create(ctx, other)
	require.NoError(t, err)
	assert.False(t, elsewhere.Reused, "the same key in another project creates there")
	otherIssues, err := c.Show(ctx, "e2eother#"+elsewhere.ShortID)
	require.NoError(t, err)
	assert.Equal(t, "e2eother#"+elsewhere.ShortID, otherIssues.QualifiedID)

	shown, err := c.Show(ctx, created.ShortID)
	require.NoError(t, err)
	assert.Equal(t, "roborev connection test", shown.Title)
	assert.Equal(t, "e2etest#"+created.ShortID, shown.QualifiedID)
	assert.ElementsMatch(t, []string{"roborev", "retry-check"}, shown.Labels)

	listed, err := c.List(ctx, kata.ListOpts{Unlimited: true})
	require.NoError(t, err)
	var found bool
	for _, issue := range listed {
		found = found || issue.ShortID == created.ShortID
	}
	assert.True(t, found)

	_, err = c.Show(ctx, "e2etest#zzzz")
	require.ErrorIs(t, err, kata.ErrNotFound)
}

// dropCommittedReply delegates to the real producer, then loses exactly one
// successful reply response. It never implements idempotency itself.
type dropCommittedReply struct {
	transport *http.Transport
	armed     atomic.Bool
}

func (d *dropCommittedReply) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	req.Header.Del("Authorization") // The isolated Unix-socket daemon needs no token.
	resp, err := d.transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") && resp.StatusCode >= 200 && resp.StatusCode < 300 && d.armed.CompareAndSwap(true, false) {
		_, readErr := io.Copy(io.Discard, resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		return nil, errors.New("synthetic lost response after producer commit")
	}
	return resp, nil
}

func TestTypedReplyRealKataLostReceiptAndRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	k := katatest.StartReal(t, ctx)
	socket := strings.TrimPrefix(k.SocketEndpoint(t, ctx), "unix://")
	tr := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	dropped := &dropCommittedReply{transport: tr}
	cfg := config.KataIntegrationConfig{Enabled: true, Endpoint: "http://127.0.0.1:1", APIKey: "fixture-token", DefaultProject: "e2etest", Teammate: "roborev"}
	c, err := kata.NewAPIClient(k.Repo, cfg, kata.WithHTTPClient(&http.Client{Transport: dropped}))
	require.NoError(t, err)
	defer c.CloseIdleConnections()
	project, err := c.ResolvePublicationProject(ctx, "e2etest")
	require.NoError(t, err)
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Findings: []structuredreview.Finding{{Severity: "high", Problem: "first", Fix: "fix"}, {Severity: "medium", Problem: "second", Fix: "fix"}}}
	publication, err := kata.PreparePublication(uuid.New(), strings.Repeat("a", 64), time.Now().UTC(), doc, "summary", "roborev")
	require.NoError(t, err)
	created, err := c.CreatePublicationIssue(ctx, project, publication)
	require.NoError(t, err)
	_, err = c.CreatePublicationRoot(ctx, project, created.Issue.UID, publication, 0)
	require.NoError(t, err)
	_, err = c.CreatePublicationRoot(ctx, project, created.Issue.UID, publication, 1)
	require.NoError(t, err)
	shown, err := c.ShowPublicationIssue(ctx, project, created.Issue.UID)
	require.NoError(t, err)
	roots := publication.Roots(shown)
	require.Len(t, roots, 2)
	require.Equal(t, "complete", roots[0].State)
	require.Equal(t, "complete", roots[1].State)
	require.NotNil(t, roots[0].Comment)
	require.NotNil(t, roots[1].Comment)
	seed, err := c.Comment(ctx, project, created.Issue.UID, roots[0].Comment.UID, "reply", "Earlier reply", "Seed-Key", "roborev")
	require.NoError(t, err)
	target, other := seed.Comment.UID, roots[1].Comment.UID
	body, key := "  Evidence is preserved exactly, including whitespace.\nSecond line.  ", "Opaque-Case-Key"
	dropped.armed.Store(true)
	_, err = c.Comment(ctx, project, created.Issue.UID, target, "refute", body, key, "roborev")
	require.ErrorIs(t, err, kata.ErrUnreachable)
	shown, err = c.ShowPublicationIssue(ctx, project, created.Issue.UID)
	require.NoError(t, err)
	require.Len(t, shown.Comments, 4)
	selected, err := kata.ResolvePublicationTarget(project, shown, target)
	require.NoError(t, err)
	assert.Equal(t, target, selected)
	receipt, err := c.Comment(ctx, project, created.Issue.UID, target, "refute", body, key, "roborev")
	require.NoError(t, err)
	assert.False(t, receipt.Changed)
	assert.Equal(t, "roborev", receipt.Comment.Author)
	require.NotNil(t, receipt.Comment.Teammate)
	assert.Equal(t, "roborev", *receipt.Comment.Teammate)
	assert.Equal(t, body, receipt.Comment.Body)
	require.NotNil(t, receipt.Comment.ReplyToUID)
	assert.Equal(t, target, *receipt.Comment.ReplyToUID)
	for _, change := range []struct{ name, target, kind, body string }{
		{"body", target, "refute", body + " changed"},
		{"kind", target, "confirm", body},
		{"target", other, "refute", body},
	} {
		t.Run(change.name, func(t *testing.T) {
			_, err := c.Comment(ctx, project, created.Issue.UID, change.target, change.kind, change.body, key, "roborev")
			require.ErrorIs(t, err, kata.ErrConflict)
			assert.Equal(t, "idempotency_mismatch", kata.ErrorCode(err))
		})
	}
	_, err = c.Comment(ctx, project, created.Issue.UID, target, "refute", body, strings.ToLower(key), "roborev")
	require.ErrorIs(t, err, kata.ErrConflict)
	assert.Equal(t, "duplicate_reply", kata.ErrorCode(err), "a differently cased key is not a replay")
	shown, err = c.ShowPublicationIssue(ctx, project, created.Issue.UID)
	require.NoError(t, err)
	require.Len(t, shown.Comments, 4)
	assert.Equal(t, receipt.Comment.UID, shown.Comments[3].UID)
	assert.Equal(t, body, shown.Comments[3].Body)
	assert.Equal(t, "roborev", shown.Comments[3].Author)
}
