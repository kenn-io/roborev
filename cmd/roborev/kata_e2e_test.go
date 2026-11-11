//go:build kata

package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/google/shlex"
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
	isolateDaemonSelection(t)
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "1")
	k := katatest.StartReal(t, ctx)
	cfg := config.KataIntegrationConfig{Enabled: true, Endpoint: k.SocketEndpoint(t, ctx), DefaultProject: "e2etest", Teammate: "roborev"}
	client, err := kata.NewAPIClient(k.Repo, cfg)
	require.NoError(t, err)
	defer client.CloseIdleConnections()
	project, err := client.ResolvePublicationProject(ctx, "e2etest")
	require.NoError(t, err)
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary"}
	for range 7 {
		doc.Findings = append(doc.Findings, structuredreview.Finding{Severity: "high", Problem: "problem", Fix: "fix"})
	}
	publication, err := kata.PreparePublication(uuid.New(), strings.Repeat("a", 64), time.Now().UTC(), doc, "summary", "roborev")
	require.NoError(t, err)
	created, err := client.CreatePublicationIssue(ctx, project, publication)
	require.NoError(t, err)
	for position := range 7 {
		_, err = client.CreatePublicationRoot(ctx, project, created.Issue.UID, publication, position)
		require.NoError(t, err)
	}
	shown, err := client.ShowPublicationIssue(ctx, project, created.Issue.UID)
	require.NoError(t, err)
	roots := publication.Roots(shown)
	require.Len(t, roots, 7)
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
	assert.Contains(t, first, "--expected-teammate ")
	args = []string{"--server", proxy.URL, "kata", "comment", created.Issue.UID, "--project", project.Name, "--destination-id", *full.DestinationID, "--expected-teammate", "roborev", "--reply", roots[0].Comment.UID, "--idempotency-key", "Typed-Reply-Key", "-m", "Evidence with 'quotes' and\nnewlines."}
	replay, err := run()
	require.NoError(t, err)
	assert.Contains(t, replay, "Author: roborev")
	assert.Contains(t, replay, "Teammate: roborev")
	assert.Contains(t, replay, "Changed: false")
	assert.EqualValues(t, 2, posts.Load())
	shown, err = client.ShowPublicationIssue(ctx, project, created.Issue.UID)
	require.NoError(t, err)
	require.Len(t, shown.Comments, 8)
	assert.Equal(t, "Evidence with 'quotes' and\nnewlines.", shown.Comments[7].Body)
	assert.Equal(t, "roborev", shown.Comments[7].Author)
	require.NotNil(t, shown.Comments[7].ReplyToUID)
	assert.Equal(t, roots[0].Comment.UID, *shown.Comments[7].ReplyToUID)

	// Discover A, lose a committed reply, then execute the printed shell tuple
	// after discovery points at a fresh daemon transport B.
	currentURL := proxy.URL
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		u, parseErr := url.Parse(currentURL)
		if parseErr != nil {
			return nil, parseErr
		}
		return &daemon.RuntimeInfo{Network: "tcp", Address: u.Host}, nil
	}
	drop.Store(true)
	beforePosts := posts.Load()
	args = []string{"kata", "comment", created.Issue.UID, "--project", project.Name, "--supersede", roots[0].Comment.UID, "--idempotency-key", "Restart-Key", "-m", "Automatic 'quoted'\nbody."}
	first, err = run()
	require.Error(t, err)
	assert.EqualValues(t, beforePosts+1, posts.Load())
	committed, err := client.ShowPublicationIssue(ctx, project, created.Issue.UID)
	require.NoError(t, err)
	var committedUID string
	for _, comment := range committed.Comments {
		if comment.Body == "Automatic 'quoted'\nbody." {
			committedUID = comment.UID
		}
	}
	require.NotEmpty(t, committedUID)
	_, retryText, ok := strings.Cut(first, "Retry: ")
	require.True(t, ok)
	assert.NotContains(t, retryText, "--server")
	assert.Contains(t, retryText, *full.DestinationID)
	assert.Contains(t, retryText, created.Issue.UID)
	assert.Contains(t, retryText, roots[0].Comment.UID)
	assert.Contains(t, retryText, "Restart-Key")
	assert.Contains(t, retryText, "--expected-teammate 'roborev'")
	retryArgs, splitErr := shlex.Split(strings.TrimSuffix(retryText, "\n"))
	require.NoError(t, splitErr)
	require.Equal(t, "roborev", retryArgs[0])
	args = retryArgs[1:]
	proxy.Close()
	require.NoError(t, server.Close())

	second := daemon.NewServer(db, daemonCfg, "")
	t.Cleanup(func() { _ = second.Close() })
	var secondPosts atomic.Int32
	secondProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/kata/response" {
			secondPosts.Add(1)
		}
		second.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(secondProxy.Close)
	currentURL = secondProxy.URL
	replay, err = run()
	require.NoError(t, err)
	assert.Contains(t, replay, "Changed: false")
	assert.Contains(t, replay, "Comment: "+committedUID)
	assert.EqualValues(t, 1, secondPosts.Load())
	shown, err = client.ShowPublicationIssue(ctx, project, created.Issue.UID)
	require.NoError(t, err)
	var found int
	for _, comment := range shown.Comments {
		if comment.Body == "Automatic 'quoted'\nbody." {
			found++
		}
	}
	assert.Equal(t, 1, found)

	// A changed teammate is detected from a fresh daemon snapshot before POST.
	changedCfg := config.DefaultConfig()
	changedCfg.Integrations.Kata = cfg
	changedCfg.Integrations.Kata.Teammate = "different"
	changed := daemon.NewServer(db, changedCfg, "")
	t.Cleanup(func() { _ = changed.Close() })
	var changedPosts atomic.Int32
	changedProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/kata/response" {
			changedPosts.Add(1)
		}
		changed.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(changedProxy.Close)
	currentURL = changedProxy.URL
	_, err = run()
	require.ErrorContains(t, err, "teammate changed")
	assert.Zero(t, changedPosts.Load())
	// Relay the same isolated producer through a different configured endpoint.
	// Its project and issue remain readable, but the destination fingerprint differs.
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(cfg.Endpoint, "unix://"))
	}}
	t.Cleanup(transport.CloseIdleConnections)
	relay := httptest.NewServer(&httputil.ReverseProxy{Transport: transport, Director: func(r *http.Request) {
		r.URL.Scheme, r.URL.Host = "http", "kata"
		r.Header.Del("Authorization")
	}})
	t.Cleanup(relay.Close)
	destinationCfg := config.DefaultConfig()
	destinationCfg.Integrations.Kata = cfg
	destinationCfg.Integrations.Kata.Endpoint = relay.URL
	destinationCfg.Integrations.Kata.APIKey = "fixture-token"
	destinationServer := daemon.NewServer(db, destinationCfg, "")
	t.Cleanup(func() { _ = destinationServer.Close() })
	var destinationPosts atomic.Int32
	destinationProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/kata/response" {
			destinationPosts.Add(1)
		}
		destinationServer.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(destinationProxy.Close)
	currentURL = destinationProxy.URL
	_, err = run()
	require.ErrorContains(t, err, "destination changed")
	assert.Zero(t, destinationPosts.Load())

	// Explicit endpoints remain pinned even while discovery advertises another daemon.
	currentURL = changedProxy.URL
	args = append([]string{"--server", secondProxy.URL}, retryArgs[1:]...)
	explicit, err := run()
	require.NoError(t, err)
	assert.Contains(t, explicit, "Retry: roborev --server '"+strings.TrimPrefix(secondProxy.URL, "http://")+"'")
	assert.EqualValues(t, 2, secondPosts.Load())
	assert.Zero(t, changedPosts.Load())
	args = append([]string{"--server", proxy.URL}, retryArgs[1:]...)
	_, err = run()
	require.Error(t, err)
	assert.EqualValues(t, 2, secondPosts.Load())
}
