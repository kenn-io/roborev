//go:build kata

package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/client/generated"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// TestKataThroughTheDaemonE2E runs the CLI's Kata client against a real daemon
// server connected to a real Kata in a disposable home. The CLI's own config
// has no Kata settings: the daemon alone knows the socket.
func TestKataThroughTheDaemonE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the real Kata daemon E2E requires private Unix socket support")
	}

	ctx := t.Context()
	k := katatest.StartReal(t, ctx)
	producerConfig := config.KataIntegrationConfig{
		Enabled: true, Endpoint: k.SocketEndpoint(t, ctx), DefaultProject: "e2etest", Teammate: "roborev",
	}
	ep := kataDaemon(t, producerConfig)
	var probes atomic.Int32
	c := newKataClientAt(k.Repo, config.DefaultConfig(), fixedEndpoint(ep, &probes))

	b, err := c.Binding(ctx)
	require.NoError(t, err)
	assert.Equal(t, "e2etest", b.Project, "the workspace binding written by kata init")

	producer, err := kata.NewAPIClient(k.Repo, producerConfig)
	require.NoError(t, err)
	defer producer.CloseIdleConnections()
	req := kata.CreateReq{Title: "daemon read fixture", Body: "details", Labels: []string{"roborev"}, IdempotencyKey: "e2e:daemon:1"}
	created, err := producer.Create(ctx, req)
	require.NoError(t, err)
	require.NotEmpty(t, created.ShortID)
	assert.False(t, created.Reused)
	assert.Equal(t, "roborev", created.Attribution.Author)
	again, err := producer.Create(ctx, req)
	require.NoError(t, err)
	assert.True(t, again.Reused)

	shown, err := c.Show(ctx, created.ShortID)
	require.NoError(t, err)
	assert.Equal(t, "daemon read fixture", shown.Title)
	assert.Equal(t, "e2etest#"+created.ShortID, shown.QualifiedID)

	listed, err := c.List(ctx, kata.ListOpts{Unlimited: true})
	require.NoError(t, err)
	var found bool
	for _, issue := range listed {
		found = found || issue.ShortID == created.ShortID
	}
	assert.True(t, found)

	status, err := newDoctorDaemon(ep).KataStatus(ctx, kata.Binding{Project: "e2etest"})
	require.NoError(t, err)
	assert.Equal(t, "ready", status.State, status.Message)
	assert.Equal(t, "e2etest", status.Project)
	assert.True(t, status.SupportsReplies)
	assert.EqualValues(t, 1, probes.Load())
}

func TestKataTypedReplyThroughDaemonE2E(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real Kata uses a private Unix socket")
	}
	ctx := t.Context()
	k := katatest.StartReal(t, ctx)
	cfg := config.KataIntegrationConfig{Enabled: true, Endpoint: k.SocketEndpoint(t, ctx), DefaultProject: "e2etest", Teammate: "roborev"}
	client, err := kata.NewAPIClient(k.Repo, cfg)
	require.NoError(t, err)
	defer client.CloseIdleConnections()
	project, err := client.ResolvePublicationProject(ctx, "e2etest")
	require.NoError(t, err)
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
	publication, err := kata.PreparePublication(uuid.New(), strings.Repeat("a", 64), time.Now().UTC(), doc, "summary", "roborev")
	require.NoError(t, err)
	created, err := client.CreatePublicationIssue(ctx, project, publication)
	require.NoError(t, err)
	_, err = client.CreatePublicationRoot(ctx, project, created.Issue.UID, publication, 0)
	require.NoError(t, err)
	shown, err := client.ShowPublicationIssue(ctx, project, created.Issue.UID)
	require.NoError(t, err)
	roots := publication.Roots(shown)
	require.Len(t, roots, 1)
	require.Equal(t, "complete", roots[0].State)
	require.NotNil(t, roots[0].Comment)
	require.NotNil(t, roots[0].Comment.Handle)
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	db := testutil.OpenTestDB(t)
	daemonCfg := config.DefaultConfig()
	daemonCfg.Integrations.Kata = cfg
	server := daemon.NewServer(db, daemonCfg, "")
	t.Cleanup(func() { _ = server.Close() })
	handler := server.Handler()
	var drop atomic.Bool
	var posts atomic.Int32
	drop.Store(true)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/kata/response" {
			w.Header().Set("Connection", "close")
			handler.ServeHTTP(w, r)
			return
		}
		posts.Add(1)
		recorded := httptest.NewRecorder()
		handler.ServeHTTP(recorded, r)
		if recorded.Code == http.StatusOK && drop.CompareAndSwap(true, false) {
			panic(http.ErrAbortHandler)
		}
		for name, values := range recorded.Header() {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.Header().Set("Connection", "close")
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes())
	}))
	t.Cleanup(proxy.Close)
	oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
	t.Cleanup(func() { serverAddr, parsedServerEndpoint = oldAddr, oldEndpoint })
	args := []string{"--server", proxy.URL, "kata", "comment", created.Issue.UID, "--project", project.Name, "--reply", *roots[0].Comment.Handle, "--idempotency-key", "Typed-Reply-Key", "-m", "Evidence with 'quotes' and\nnewlines."}
	run := func() (string, error) {
		root := newRootCmd()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(args)
		err := root.Execute()
		return output.String(), err
	}
	first, err := run()
	require.Error(t, err)
	assert.Contains(t, first, "Retry: roborev --server")
	assert.EqualValues(t, 1, posts.Load(), "a lost response must not trigger an automatic resend")
	ep, err := daemon.ParseEndpoint(proxy.URL)
	require.NoError(t, err)
	full, err := ep.APIClient(0).KataShowIssue(ctx, &generated.KataShowIssueRequestOptions{Query: &generated.KataShowIssueQuery{Ref: created.Issue.UID, Project: &project.Name}})
	require.NoError(t, err)
	require.NotNil(t, full.DestinationID)
	assert.Contains(t, first, *full.DestinationID)
	assert.Contains(t, first, roots[0].Comment.UID)
	args = []string{"--server", proxy.URL, "kata", "comment", created.Issue.UID, "--project", project.Name, "--destination-id", *full.DestinationID, "--reply", roots[0].Comment.UID, "--idempotency-key", "Typed-Reply-Key", "-m", "Evidence with 'quotes' and\nnewlines."}
	replay, err := run()
	require.NoError(t, err)
	assert.Contains(t, replay, "Author: roborev")
	assert.Contains(t, replay, "Teammate: roborev")
	assert.Contains(t, replay, "Changed: false")
	assert.EqualValues(t, 2, posts.Load())
	shown, err = client.ShowPublicationIssue(ctx, project, created.Issue.UID)
	require.NoError(t, err)
	require.Len(t, shown.Comments, 2)
	assert.Equal(t, "Evidence with 'quotes' and\nnewlines.", shown.Comments[1].Body)
	assert.Equal(t, "roborev", shown.Comments[1].Author)
	require.NotNil(t, shown.Comments[1].ReplyToUID)
	assert.Equal(t, roots[0].Comment.UID, *shown.Comments[1].ReplyToUID)
}
