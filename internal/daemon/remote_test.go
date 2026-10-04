package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/requestsigning"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

func remoteFixture(t *testing.T) (*Server, config.RemoteConfig, requestsigning.Key) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("durable remote ingress requires POSIX owner-only files and directory fsync")
	}
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	s := newAuthTestServer(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	dir := t.TempDir()
	secret := make([]byte, 64)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	key := requestsigning.Key{ID: "reader", Secret: secret}
	keyfile := filepath.Join(dir, "signing.key")
	require.NoError(t, os.WriteFile(keyfile, []byte(hex.EncodeToString(secret)), 0o600))
	replay := filepath.Join(dir, "replay.json")
	require.NoError(t, requestsigning.InitializeReplay(replay))
	cfg := config.RemoteConfig{Enabled: true, TrustedProxy: true, ExternalURL: "https://reviews.example.com/history", ReplayFile: replay, MaxBodyBytes: 1024, MaxHeaderBytes: 8192, MaxConcurrent: 2, ReplayCapacity: 100, ReadHeaderTimeout: "5s", ReadTimeout: "30s", RequestTimeout: "1m", Keys: []config.RemoteSigningKey{{ID: key.ID, SecretFile: keyfile, Grants: []string{"history:read"}, AllRepos: true}}}
	return s, cfg, key
}

func remoteRequest(t *testing.T, key requestsigning.Key, path string) *http.Request {
	t.Helper()
	r, err := http.NewRequest("GET", "https://reviews.example.com/history"+path, nil)
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	require.NoError(t, requestsigning.Sign(r, key, time.Now()))
	// Trusted ingress strips exactly the configured prefix. Forwarded values
	// never select authority or prefix at the verification boundary.
	r.URL.Scheme = ""
	r.URL.Host = ""
	r.URL.Path = r.URL.Path[len("/history"):]
	r.Host = "internal.example"
	r.RequestURI = r.URL.RequestURI()
	return r
}

func TestRemoteRequiredSigningAndDeniedRoutes(t *testing.T) {
	s, cfg, key := remoteFixture(t)
	remote, err := s.newRemoteHandler(cfg)
	require.NoError(t, err)
	defer remote.Close()
	r := remoteRequest(t, key, "/api/ping")
	out := httptest.NewRecorder()
	remote.ServeHTTP(out, r)
	assert.Equal(t, 200, out.Code, out.Body.String())
	r = remoteRequest(t, key, "/api/ping")
	r.Header.Del("Signature")
	out = httptest.NewRecorder()
	remote.ServeHTTP(out, r)
	assert.Equal(t, 401, out.Code)
	for _, path := range []string{"/api/shutdown", "/api/enqueue", "/api/config", "/mcp", "/debug/pprof/", "/api/events", "/api/jobs?panel_run=synthetic"} {
		r = remoteRequest(t, key, path)
		out = httptest.NewRecorder()
		remote.ServeHTTP(out, r)
		assert.Equal(t, 403, out.Code, path+": "+out.Body.String())
	}
	r = remoteRequest(t, key, "/api/ping")
	r.Header.Set("X-Forwarded-Host", "evil.example")
	r.Header.Set("X-Forwarded-Prefix", "/evil")
	out = httptest.NewRecorder()
	remote.ServeHTTP(out, r)
	assert.Equal(t, 200, out.Code)
}

func TestRemoteScopesIDListAndReplay(t *testing.T) {
	s, cfg, key := remoteFixture(t)
	repo, err := s.db.GetOrCreateRepo("/synthetic/allowed")
	require.NoError(t, err)
	denied, err := s.db.GetOrCreateRepo("/synthetic/denied")
	require.NoError(t, err)
	for _, id := range []int64{repo.ID, denied.ID} {
		_, err = s.db.EnqueueJob(storage.EnqueueOpts{RepoID: id, GitRef: "task", Agent: "test"})
		require.NoError(t, err)
	}
	cfg.Keys[0].AllRepos = false
	cfg.Keys[0].RepoIDs = []int64{repo.ID}
	remote, err := s.newRemoteHandler(cfg)
	require.NoError(t, err)
	r := remoteRequest(t, key, "/api/jobs")
	replay := r.Clone(r.Context())
	out := httptest.NewRecorder()
	remote.ServeHTTP(out, r)
	require.Equal(t, 200, out.Code, out.Body.String())
	assert.NotContains(t, out.Body.String(), denied.RootPath)
	assert.Contains(t, out.Body.String(), repo.RootPath)
	out = httptest.NewRecorder()
	remote.ServeHTTP(out, replay)
	assert.Equal(t, 401, out.Code)
	jobs, err := s.db.ListJobs("", denied.RootPath, 1, 0)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	for _, path := range []string{fmt.Sprintf("/api/jobs?id=%d", jobs[0].ID), fmt.Sprintf("/api/review?job_id=%d", jobs[0].ID), fmt.Sprintf("/api/comments?job_id=%d", jobs[0].ID)} {
		out = httptest.NewRecorder()
		remote.ServeHTTP(out, remoteRequest(t, key, path))
		assert.Equal(t, 403, out.Code)
	}
	require.NoError(t, remote.Close())
	remote, err = s.newRemoteHandler(cfg)
	require.NoError(t, err)
	defer remote.Close()
	out = httptest.NewRecorder()
	remote.ServeHTTP(out, replay)
	assert.Equal(t, 401, out.Code)
}

type unreadBody struct{ reads int }

func (b *unreadBody) Read([]byte) (int, error) { b.reads++; return 0, fmt.Errorf("must not read") }
func (b *unreadBody) Close() error             { return nil }
func TestRemoteRejectsBeforeReadingAndBoundsBodies(t *testing.T) {
	s, cfg, key := remoteFixture(t)
	handler, err := s.newRemoteHandler(cfg)
	require.NoError(t, err)
	defer handler.Close()
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("Authorization") },
		func(r *http.Request) { r.Header.Del("Signature") },
		func(r *http.Request) { r.URL.Path = "/api/shutdown" },
	} {
		r := remoteRequest(t, key, "/api/ping")
		mutate(r)
		body := &unreadBody{}
		r.Body = body
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		assert.NotEqual(t, 200, out.Code)
		assert.Zero(t, body.reads)
	}
	r := remoteRequest(t, key, "/api/ping")
	r.ContentLength = cfg.MaxBodyBytes + 1
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, r)
	assert.Equal(t, 413, out.Code)
	r = remoteRequest(t, key, "/api/ping")
	r.Header.Set("Content-Encoding", "gzip")
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, r)
	assert.Equal(t, 401, out.Code)
}

func TestRemoteProductionClientBehindHTTPSPrefix(t *testing.T) {
	s, cfg, key := remoteFixture(t)
	var handler *remoteHandler
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/history")
		r.RequestURI = r.URL.RequestURI()
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	cfg.ExternalURL = server.URL + "/history"
	var err error
	handler, err = s.newRemoteHandler(cfg)
	require.NoError(t, err)
	defer handler.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), fmt.Appendf(nil, "auth_key = '%s'\n[remote_client]\nexternal_url='%s'\nkey_id='%s'\nsecret_file='%s'\nca_file='%s'\n", s.authKey, cfg.ExternalURL, key.ID, cfg.Keys[0].SecretFile, caFile), 0o600))
	ep, err := ParseEndpoint(server.URL + "/history")
	require.NoError(t, err)
	api := ep.APIClient(time.Second)
	resp, err := api.ListJobsRaw(t.Context(), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	_, err = ProbeDaemon(ep, time.Second)
	require.NoError(t, err)
	resp, err = api.EnqueueJobRaw(t.Context(), nil, roborevclient.WithBody([]byte(`{}`)))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, 403, resp.StatusCode)
}

type delayedEmptyBody struct{ delay time.Duration }

func (b delayedEmptyBody) Read([]byte) (int, error) { time.Sleep(b.delay); return 0, io.EOF }
func (b delayedEmptyBody) Close() error             { return nil }
func TestRemoteSlowBodyExpiresBeforeExecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, cfg, key := remoteFixture(t)
		handler, err := s.newRemoteHandler(cfg)
		require.NoError(t, err)
		defer handler.Close()
		r := remoteRequest(t, key, "/api/ping")
		r.Body = delayedEmptyBody{delay: 31 * time.Second}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		assert.Equal(t, 401, out.Code)
	})
}

func TestRemoteStreamReconnectAndRepositoryFilter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, cfg, key := remoteFixture(t)
		allowed, err := s.db.GetOrCreateRepo("/synthetic/allowed")
		require.NoError(t, err)
		denied, err := s.db.GetOrCreateRepo("/synthetic/denied")
		require.NoError(t, err)
		job, err := s.db.EnqueueJob(storage.EnqueueOpts{RepoID: allowed.ID, GitRef: "task", Agent: "test"})
		require.NoError(t, err)
		other, err := s.db.EnqueueJob(storage.EnqueueOpts{RepoID: denied.ID, GitRef: "task", Agent: "test"})
		require.NoError(t, err)
		cfg.Keys[0].AllRepos = false
		cfg.Keys[0].RepoIDs = []int64{allowed.ID}
		cfg.Keys[0].Grants = []string{"history:events"}
		handler, err := s.newRemoteHandler(cfg)
		require.NoError(t, err)
		defer handler.Close()
		var nonces []string
		for range 2 {
			ctx, cancel := context.WithCancel(t.Context())
			r := remoteRequest(t, key, "/api/stream/events").WithContext(ctx)
			nonces = append(nonces, r.Header.Get("Signature-Input"))
			out := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { handler.ServeHTTP(out, r); close(done) }()
			synctest.Wait()
			s.broadcaster.Broadcast(Event{Type: "job.done", JobID: other.ID, Repo: denied.RootPath, Findings: "denied-content"})
			s.broadcaster.Broadcast(Event{Type: "job.done", JobID: job.ID, Repo: allowed.RootPath, Findings: "allowed-content"})
			synctest.Wait()
			cancel()
			<-done
			assert.Contains(t, out.Body.String(), "allowed-content")
			assert.NotContains(t, out.Body.String(), "denied-content")
		}
		assert.NotEqual(t, nonces[0], nonces[1])
	})
}

type remotePipeListener struct {
	conn     net.Conn
	accepted bool
	closed   chan struct{}
	once     sync.Once
}

func (l *remotePipeListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *remotePipeListener) Close() error {
	l.once.Do(func() { close(l.closed); _ = l.conn.Close() })
	return nil
}

func (*remotePipeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
}

func TestRemoteIdleTLSStreamUsesRequestBudget(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", http2), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, cfg, key := remoteFixture(t)
				cfg.Keys[0].Grants = []string{"history:events"}
				var handler *remoteHandler
				done := make(chan struct{})
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(done)
					r.URL.Path = strings.TrimPrefix(r.URL.Path, "/history")
					r.RequestURI = r.URL.RequestURI()
					handler.ServeHTTP(w, r)
				}))
				// net.Pipe keeps the real TLS/net/http deadline machinery inside the
				// synctest bubble, so the configured 30s/1m budgets use virtual time.
				require.NoError(t, server.Listener.Close())
				peer, conn := net.Pipe()
				server.Listener = &remotePipeListener{conn: conn, closed: make(chan struct{})}
				server.EnableHTTP2 = http2
				server.StartTLS()
				defer server.Close()
				defer peer.Close()
				cfg.ExternalURL = server.URL + "/history"
				var err error
				handler, err = s.newRemoteHandler(cfg)
				require.NoError(t, err)
				defer handler.Close()
				transport := server.Client().Transport.(*http.Transport).Clone()
				transport.DialContext = func(context.Context, string, string) (net.Conn, error) { return peer, nil }
				defer transport.CloseIdleConnections()
				client, err := requestsigning.HTTPClient(cfg.ExternalURL, &http.Client{Transport: transport}, func() (requestsigning.Key, error) { return key, nil })
				require.NoError(t, err)
				r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, cfg.ExternalURL+"/api/stream/events", nil)
				require.NoError(t, err)
				r.Header.Set("Authorization", "Bearer "+s.authKey)
				resp, err := client.Do(r)
				require.NoError(t, err)
				defer resp.Body.Close()
				require.Equal(t, http.StatusOK, resp.StatusCode)
				if http2 {
					require.Equal(t, 2, resp.ProtoMajor)
				} else {
					require.Equal(t, 1, resp.ProtoMajor)
				}
				time.Sleep(31 * time.Second)
				synctest.Wait()
				assert.Len(t, handler.slots, 1, "idle stream must remain admitted beyond the body read budget")
				time.Sleep(30 * time.Second)
				synctest.Wait()
				assert.Empty(t, handler.slots, "request budget must release the stream admission")
				<-done
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err, "planned expiry must finish HTTP framing so clients can reconnect")
				assert.Empty(t, body)
			})
		})
	}
}

type blockingEmptyBody struct{ release <-chan struct{} }

func (b blockingEmptyBody) Read([]byte) (int, error) { <-b.release; return 0, io.EOF }
func (b blockingEmptyBody) Close() error             { return nil }
func TestRemoteConcurrentRequestBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, cfg, key := remoteFixture(t)
		handler, err := s.newRemoteHandler(cfg)
		require.NoError(t, err)
		defer handler.Close()
		release := make(chan struct{})
		var wg sync.WaitGroup
		for range cfg.MaxConcurrent {
			r := remoteRequest(t, key, "/api/ping")
			r.Body = blockingEmptyBody{release: release}
			wg.Go(func() { handler.ServeHTTP(httptest.NewRecorder(), r) })
		}
		synctest.Wait()
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, remoteRequest(t, key, "/api/ping"))
		assert.Equal(t, 503, out.Code)
		close(release)
		wg.Wait()
	})
}

func TestRemoteKeyRevocationAndNewIDRotation(t *testing.T) {
	s, cfg, key := remoteFixture(t)
	first, err := s.newRemoteHandler(cfg)
	require.NoError(t, err)
	r := remoteRequest(t, key, "/api/ping")
	out := httptest.NewRecorder()
	first.ServeHTTP(out, r)
	require.Equal(t, 200, out.Code)
	require.NoError(t, first.Close())
	fresh := make([]byte, 64)
	_, err = rand.Read(fresh)
	require.NoError(t, err)
	newKey := requestsigning.Key{ID: "reader-next", Secret: fresh}
	file := filepath.Join(t.TempDir(), "next.key")
	require.NoError(t, os.WriteFile(file, []byte(hex.EncodeToString(fresh)), 0o600))
	cfg.Keys[0].ID = newKey.ID
	cfg.Keys[0].SecretFile = file
	second, err := s.newRemoteHandler(cfg)
	require.NoError(t, err)
	defer second.Close()
	out = httptest.NewRecorder()
	second.ServeHTTP(out, remoteRequest(t, key, "/api/ping"))
	assert.Equal(t, 401, out.Code)
	out = httptest.NewRecorder()
	second.ServeHTTP(out, remoteRequest(t, newKey, "/api/ping"))
	assert.Equal(t, 200, out.Code)
}

func TestRemoteListenerShutdownCancelsRequests(t *testing.T) {
	s, cfg, _ := remoteFixture(t)
	cfg.Listen = "127.0.0.1:0"
	require.NoError(t, s.startRemoteServer(cfg))
	defer s.abortRemoteServer()
	require.NotNil(t, s.remoteServer.BaseContext)
	ctx := s.remoteServer.BaseContext(nil)
	require.NoError(t, s.remoteServer.Shutdown(t.Context()))
	<-ctx.Done()
	assert.ErrorIs(t, ctx.Err(), context.Canceled)
}

func TestRemoteRejectsUnannouncedWireTrailers(t *testing.T) {
	s, cfg, key := remoteFixture(t)
	handler, err := s.newRemoteHandler(cfg)
	require.NoError(t, err)
	defer handler.Close()
	server := httptest.NewServer(handler)
	defer server.Close()
	r, err := http.NewRequest("GET", cfg.ExternalURL+"/api/ping", strings.NewReader("body"))
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer "+s.authKey)
	require.NoError(t, requestsigning.Sign(r, key, time.Now()))
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err)
	defer conn.Close()
	_, err = io.WriteString(conn, "GET /api/ping HTTP/1.1\r\nHost: reviews.example.com\r\nConnection: close\r\nTransfer-Encoding: chunked\r\n")
	require.NoError(t, err)
	for name, values := range r.Header {
		_, err = fmt.Fprintf(conn, "%s: %s\r\n", name, values[0])
		require.NoError(t, err)
	}
	_, err = io.WriteString(conn, "\r\n4\r\nbody\r\n0\r\nX-Late: synthetic\r\n\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), r)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 400, resp.StatusCode)
}

func TestRemoteRepositoryGrantSurvivesPathReuse(t *testing.T) {
	s, cfg, key := remoteFixture(t)
	allowed, err := s.db.GetOrCreateRepo("/synthetic/allowed")
	require.NoError(t, err)
	denied, err := s.db.GetOrCreateRepo("/synthetic/denied")
	require.NoError(t, err)
	job, err := s.db.EnqueueJob(storage.EnqueueOpts{RepoID: allowed.ID, GitRef: "allowed-history", Agent: "test"})
	require.NoError(t, err)
	_, err = s.db.EnqueueJob(storage.EnqueueOpts{RepoID: denied.ID, GitRef: "denied-history", Agent: "test"})
	require.NoError(t, err)
	cfg.Keys[0].AllRepos = false
	cfg.Keys[0].RepoIDs = []int64{allowed.ID}
	handler, err := s.newRemoteHandler(cfg)
	require.NoError(t, err)
	defer handler.Close()
	require.NoError(t, s.db.MoveRepo(allowed.ID, "/synthetic/moved", ""))
	require.NoError(t, s.db.MoveRepo(denied.ID, allowed.RootPath, ""))
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, remoteRequest(t, key, "/api/jobs?limit=1"))
	require.Equal(t, 200, out.Code)
	assert.Contains(t, out.Body.String(), "allowed-history")
	assert.NotContains(t, out.Body.String(), "denied-history")
	setJobStatus(t, s.db, job.ID, storage.JobStatusRunning)
	require.NoError(t, testutil.CompleteReviewFixture(s.db, job.ID, "test", "synthetic prompt", "allowed review"))
	_, err = s.db.AddCommentToJob(job.ID, "reader", "allowed comment")
	require.NoError(t, err)
	for path, content := range map[string]string{fmt.Sprintf("/api/review?job_id=%d", job.ID): "allowed review", fmt.Sprintf("/api/comments?job_id=%d", job.ID): "allowed comment"} {
		out = httptest.NewRecorder()
		handler.ServeHTTP(out, remoteRequest(t, key, path))
		require.Equal(t, 200, out.Code, out.Body.String())
		assert.Contains(t, out.Body.String(), content)
	}
}

func TestRemoteRepositoryGrantDoesNotTransferAfterDeletion(t *testing.T) {
	s, cfg, key := remoteFixture(t)
	repo, err := s.db.GetOrCreateRepo("/synthetic/project-a")
	require.NoError(t, err)
	job, err := s.db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "original-history", Agent: "test"})
	require.NoError(t, err)
	cfg.Keys[0].AllRepos = false
	cfg.Keys[0].RepoIDs = []int64{repo.ID}
	handler, err := s.newRemoteHandler(cfg)
	require.NoError(t, err)
	defer handler.Close()
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, remoteRequest(t, key, fmt.Sprintf("/api/jobs?id=%d", job.ID)))
	require.Equal(t, http.StatusOK, out.Code, out.Body.String())
	assert.Contains(t, out.Body.String(), "original-history")
	require.NoError(t, s.db.DeleteRepo(repo.ID, true))
	replacement, err := s.db.GetOrCreateRepo("/synthetic/project-b")
	require.NoError(t, err)
	job, err = s.db.EnqueueJob(storage.EnqueueOpts{RepoID: replacement.ID, GitRef: "replacement-history", Agent: "test"})
	require.NoError(t, err)
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, remoteRequest(t, key, fmt.Sprintf("/api/jobs?id=%d", job.ID)))
	assert.Equal(t, http.StatusForbidden, out.Code, out.Body.String())
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, remoteRequest(t, key, "/api/jobs"))
	require.Equal(t, http.StatusOK, out.Code, out.Body.String())
	assert.NotContains(t, out.Body.String(), "replacement-history")
}

func TestRemoteLegacyCommentsUseOwningRepository(t *testing.T) {
	s, cfg, key := remoteFixture(t)
	allowed, err := s.db.GetOrCreateRepo("/synthetic/allowed")
	require.NoError(t, err)
	denied, err := s.db.GetOrCreateRepo("/synthetic/denied")
	require.NoError(t, err)
	sha := strings.Repeat("a", 40)
	job, err := s.db.EnqueueJob(storage.EnqueueOpts{RepoID: allowed.ID, GitRef: sha, Agent: "test"})
	require.NoError(t, err)
	commit, err := s.db.GetOrCreateCommit(denied.ID, sha, "author", "subject", time.Now())
	require.NoError(t, err)
	_, err = s.db.AddComment(commit.ID, "reader", "denied legacy comment")
	require.NoError(t, err)
	cfg.Keys[0].AllRepos = false
	cfg.Keys[0].RepoIDs = []int64{allowed.ID}
	handler, err := s.newRemoteHandler(cfg)
	require.NoError(t, err)
	defer handler.Close()
	path := fmt.Sprintf("/api/comments?job_id=%d", job.ID)
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, remoteRequest(t, key, path))
	require.Equal(t, 200, out.Code, out.Body.String())
	assert.NotContains(t, out.Body.String(), "denied legacy comment")
	commit, err = s.db.GetOrCreateCommit(allowed.ID, sha, "author", "subject", time.Now())
	require.NoError(t, err)
	_, err = s.db.AddComment(commit.ID, "reader", "allowed legacy comment")
	require.NoError(t, err)
	out = httptest.NewRecorder()
	handler.ServeHTTP(out, remoteRequest(t, key, path))
	require.Equal(t, 200, out.Code, out.Body.String())
	assert.Contains(t, out.Body.String(), "allowed legacy comment")
	assert.NotContains(t, out.Body.String(), "denied legacy comment")
}

func TestRemoteLegacyCommentsMatchJobSubject(t *testing.T) {
	s, cfg, key := remoteFixture(t)
	repo, err := s.db.GetOrCreateRepo("/synthetic/project")
	require.NoError(t, err)
	sha := strings.Repeat("a", 40)
	commit, err := s.db.GetOrCreateCommit(repo.ID, sha, "author", "subject", time.Now())
	require.NoError(t, err)
	_, err = s.db.AddComment(commit.ID, "reader", "commit feedback")
	require.NoError(t, err)
	handler, err := s.newRemoteHandler(cfg)
	require.NoError(t, err)
	defer handler.Close()
	for _, tc := range []struct {
		name       string
		opts       storage.EnqueueOpts
		wantLegacy bool
	}{
		{"commit", storage.EnqueueOpts{GitRef: sha, CommitID: commit.ID}, true},
		{"commit_without_id", storage.EnqueueOpts{GitRef: sha}, true},
		{"dirty", storage.EnqueueOpts{GitRef: "dirty", JobType: storage.JobTypeDirty, CommitID: commit.ID}, false},
		{"task", storage.EnqueueOpts{GitRef: sha, JobType: storage.JobTypeTask, Prompt: "Explain this subsystem"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.RepoID, tc.opts.Agent = repo.ID, "test"
			job, err := s.db.EnqueueJob(tc.opts)
			require.NoError(t, err)
			_, err = s.db.AddCommentToJob(job.ID, "reader", "job feedback")
			require.NoError(t, err)
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, remoteRequest(t, key, fmt.Sprintf("/api/comments?job_id=%d", job.ID)))
			require.Equal(t, http.StatusOK, out.Code, out.Body.String())
			var result struct {
				Responses []storage.Response `json:"responses"`
			}
			testutil.DecodeJSON(t, out, &result)
			want := []string{"job feedback"}
			if tc.wantLegacy {
				want = append(want, "commit feedback")
			}
			var got []string
			for _, response := range result.Responses {
				got = append(got, response.Response)
			}
			assert.ElementsMatch(t, want, got)
		})
	}
}

type scopeChangingBody struct{ change func() }

func (b *scopeChangingBody) Read([]byte) (int, error) { b.change(); return 0, io.EOF }
func (b *scopeChangingBody) Close() error             { return nil }

func TestRemoteRechecksJobScopeAfterBody(t *testing.T) {
	s, cfg, key := remoteFixture(t)
	allowed, err := s.db.GetOrCreateRepo("/synthetic/allowed")
	require.NoError(t, err)
	denied, err := s.db.GetOrCreateRepo("/synthetic/denied")
	require.NoError(t, err)
	cfg.Keys[0].AllRepos = false
	cfg.Keys[0].RepoIDs = []int64{allowed.ID}
	handler, err := s.newRemoteHandler(cfg)
	require.NoError(t, err)
	defer handler.Close()
	for _, route := range []string{"/api/jobs?id=", "/api/review?job_id=", "/api/comments?job_id="} {
		job, err := s.db.EnqueueJob(storage.EnqueueOpts{RepoID: allowed.ID, GitRef: "task", Agent: "test"})
		require.NoError(t, err)
		setJobStatus(t, s.db, job.ID, storage.JobStatusRunning)
		require.NoError(t, testutil.CompleteReviewFixture(s.db, job.ID, "test", "prompt", "review"))
		r := remoteRequest(t, key, route+strconv.FormatInt(job.ID, 10))
		r.Body = &scopeChangingBody{change: func() {
			_, err := s.db.Exec("UPDATE review_jobs SET repo_id = ? WHERE id = ?", denied.ID, job.ID)
			require.NoError(t, err)
		}}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		assert.Equal(t, 403, out.Code, route+": "+out.Body.String())
	}
}
