package daemon

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/requestsigning"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/version"
)

type remoteScopeKey struct{}

func remoteScoped(ctx context.Context) bool {
	_, ok := ctx.Value(remoteScopeKey{}).(remoteGrant)
	return ok
}

func remoteAllowsJob(ctx context.Context, job *storage.ReviewJob) bool {
	grant, ok := ctx.Value(remoteScopeKey{}).(remoteGrant)
	return !ok || job != nil && (grant.allRepos || grant.repoIDs[job.RepoID])
}

func remoteJobOptions(ctx context.Context) []storage.ListJobsOption {
	grant, ok := ctx.Value(remoteScopeKey{}).(remoteGrant)
	if !ok || grant.allRepos {
		return nil
	}
	ids := make([]int64, 0, len(grant.repoIDs))
	for id := range grant.repoIDs {
		ids = append(ids, id)
	}
	return []storage.ListJobsOption{storage.WithRepoIDs(ids)}
}

type remoteGrant struct {
	read, events, allRepos bool
	repoIDs                map[int64]bool
}
type remoteHandler struct {
	server   *Server
	cfg      config.RemoteConfig
	external *url.URL
	keys     map[string]requestsigning.Key
	grants   map[string]remoteGrant
	nonces   *requestsigning.NonceCache
	slots    chan struct{}
	budget   time.Duration
}

func (s *Server) newRemoteHandler(cfg config.RemoteConfig) (*remoteHandler, error) {
	if err := config.ValidateRemote(cfg); err != nil {
		return nil, err
	}
	external, err := requestsigning.ValidateBase(cfg.ExternalURL)
	if err != nil {
		return nil, err
	}
	h := &remoteHandler{server: s, cfg: cfg, external: external, keys: map[string]requestsigning.Key{}, grants: map[string]remoteGrant{}, nonces: requestsigning.NewNonceCache(time.Now()), slots: make(chan struct{}, cfg.MaxConcurrent)}
	h.budget, _ = time.ParseDuration(cfg.RequestTimeout)
	for _, entry := range cfg.Keys {
		key, err := requestsigning.ReadKey(entry.ID, entry.SecretFile, entry.SecretEnv)
		if err != nil {
			return nil, err
		}
		h.keys[entry.ID] = key
		g := remoteGrant{allRepos: entry.AllRepos, repoIDs: map[int64]bool{}}
		for _, grant := range entry.Grants {
			g.read = g.read || grant == "history:read"
			g.events = g.events || grant == "history:events"
		}
		for _, id := range entry.RepoIDs {
			repo, err := s.db.GetRepoByID(id)
			if err != nil || repo == nil || repo.RootPath == "" {
				return nil, errors.New("remote grant references an unavailable repository")
			}
			g.repoIDs[id] = true
		}
		h.grants[entry.ID] = g
	}
	return h, nil
}

func remoteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// All messages are fixed, never request/header/content strings.
	_, _ = w.Write([]byte(`{"error":"` + message + `"}`))
}

func (h *remoteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	headers := len(r.Method) + len(r.Host) + len(r.URL.RequestURI())
	for name, values := range r.Header {
		for _, value := range values {
			headers += len(name) + len(value) + 4
		}
	}
	if headers > h.cfg.MaxHeaderBytes {
		remoteError(w, 431, "remote header budget exceeded")
		return
	}
	if !h.cfg.TrustedProxy && (r.TLS == nil || r.Host != h.external.Host) {
		remoteError(w, 401, "remote HTTPS origin required")
		return
	}
	if r.URL.IsAbs() || r.URL.Path == "" || !strings.HasPrefix(r.URL.Path, "/") {
		remoteError(w, 401, "invalid request target")
		return
	}
	target := *h.external
	target.Path = h.external.Path + r.URL.Path
	target.RawPath = ""
	target.RawQuery = r.URL.RawQuery
	target.ForceQuery = r.URL.ForceQuery
	// Reject escaped paths before reconstruction; rewriting ambiguous encodings
	// would otherwise produce a different signed component than the wire URI.
	if r.URL.RawPath != "" || r.RequestURI != "" && r.RequestURI != r.URL.RequestURI() {
		remoteError(w, 401, "ambiguous request target")
		return
	}
	v, err := requestsigning.VerifyHeaders(r, target.String(), h.keys, time.Now())
	if err != nil {
		remoteError(w, 401, "invalid or expired request signature")
		return
	}
	grant := h.grants[v.KeyID]
	clone := r.Clone(r.Context())
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		remoteError(w, 400, "invalid query")
		return
	}
	if !h.authorize(clone, q, grant) {
		remoteError(w, 403, "operation or repository denied")
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		remoteError(w, 503, "remote request capacity reached")
		return
	}
	ctx, cancel := context.WithTimeout(clone.Context(), h.budget)
	defer cancel()
	clone = clone.WithContext(context.WithValue(ctx, remoteScopeKey{}, grant))
	controller := http.NewResponseController(w)
	readBudget, _ := time.ParseDuration(h.cfg.ReadTimeout)
	_ = controller.SetReadDeadline(time.Now().Add(min(readBudget, h.budget)))
	_ = controller.SetWriteDeadline(time.Now().Add(h.budget))
	// Read the original request: net/http publishes late trailers there, while
	// Request.Clone copies the initial trailer map before body consumption.
	if err = requestsigning.VerifyEmptyBody(r); err != nil {
		status := 400
		if errors.Is(err, requestsigning.ErrBodyNotAllowed) {
			status = 413
		}
		remoteError(w, status, "signed body verification failed")
		return
	}
	// The read budget bounds body verification. net/http's background read
	// shares this connection deadline and cancels the request when it expires,
	// so clear it before execution or an idle stream would end prematurely.
	_ = controller.SetReadDeadline(time.Time{})
	if ctx.Err() != nil {
		remoteError(w, 401, "request signature expired before admission")
		return
	}
	if err = h.nonces.Admit(v, time.Now()); err != nil {
		remoteError(w, 401, "request replay admission denied")
		return
	}
	if clone.URL.Path == "/api/ping" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"service":"roborev","version":"` + version.Version + `"}`))
		return
	}
	// Query rewriting happens only after the original external URI and complete
	// body are authenticated. Repository IDs in the context scope SQL queries.
	clone.URL.RawQuery = q.Encode()
	if clone.URL.Path == "/api/comments" {
		id, _ := strconv.ParseInt(q.Get("job_id"), 10, 64)
		job, err := h.server.db.GetJobByID(id)
		if err != nil {
			remoteError(w, 404, "job not found")
			return
		}
		if !remoteAllowsJob(clone.Context(), job) {
			remoteError(w, 403, "job repository denied")
			return
		}
		responses, err := h.server.db.GetCommentsForJob(id)
		commitID, fallbackSHA := job.LegacyCommentLookupTarget()
		if err == nil && (commitID > 0 || fallbackSHA != "") {
			// Legacy SHA lookup must be repository-scoped, including when the
			// job has no commit ID. Missing legacy commits are ordinary here.
			var commit *storage.Commit
			if commitID > 0 {
				commit, err = h.server.db.GetCommitByID(commitID)
			} else {
				commit, err = h.server.db.GetCommitByRepoAndSHA(job.RepoID, fallbackSHA)
			}
			if errors.Is(err, sql.ErrNoRows) {
				err = nil
			} else if err == nil {
				if commit.RepoID != job.RepoID {
					remoteError(w, 403, "legacy comment repository denied")
					return
				}
				var legacy []storage.Response
				legacy, err = h.server.db.GetCommentsForCommit(commit.ID)
				responses = storage.MergeResponses(responses, legacy)
			}
		}
		if err != nil {
			remoteError(w, 500, "cannot read comments")
			return
		}
		writeRemoteJSON(w, map[string]any{"responses": responses})
		return
	}
	if clone.URL.Path == "/api/stream/events" {
		h.stream(w, clone, grant)
		return
	}
	if ctx.Err() != nil {
		remoteError(w, 408, "remote request canceled")
		return
	}
	h.server.httpServer.Handler.ServeHTTP(w, clone)
}

func (h *remoteHandler) authorize(r *http.Request, q url.Values, g remoteGrant) bool {
	if r.Method != http.MethodGet {
		return false
	}
	var allowed string
	switch r.URL.Path {
	case "/api/ping":
		allowed = ""
	case "/api/jobs":
		if !g.read {
			return false
		}
		allowed = " id status git_ref analysis_type branch branch_empty branch_include_empty closed job_type exclude_job_type hide_classify_jobs omit_prompt include_findings limit offset before cursor "
	case "/api/review", "/api/comments":
		if !g.read {
			return false
		}
		allowed = " job_id "
	case "/api/stream/events":
		if !g.events {
			return false
		}
		allowed = ""
	default:
		return false
	}
	for name, values := range q {
		if len(values) != 1 || !strings.Contains(allowed, " "+name+" ") {
			return false
		}
	}
	field := "job_id"
	if r.URL.Path == "/api/jobs" {
		field = "id"
	}
	if r.URL.Path == "/api/review" || r.URL.Path == "/api/comments" || q.Has("id") {
		id, err := strconv.ParseInt(q.Get(field), 10, 64)
		if err != nil || id <= 0 {
			return false
		}
		job, err := h.server.db.GetJobByID(id)
		if err != nil || !g.allRepos && !g.repoIDs[job.RepoID] {
			return false
		}
	}
	return true
}

func (h *remoteHandler) stream(w http.ResponseWriter, r *http.Request, g remoteGrant) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	controller := http.NewResponseController(w)
	// Bound each write by the configured request budget, without leaving a
	// socket deadline running while idle. On return, net/http still needs to
	// write the final HTTP frame after the stream context has expired.
	defer func() { _ = controller.SetWriteDeadline(time.Now().Add(h.budget)) }()
	_ = controller.SetWriteDeadline(time.Now().Add(h.budget))
	if controller.Flush() != nil {
		return
	}
	_ = controller.SetWriteDeadline(time.Time{})
	id, ch := h.server.broadcaster.Subscribe("")
	defer h.server.broadcaster.Unsubscribe(id)
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			// Resolve owning job server-side; events without an authorized job (including
			// global configuration/hook events) never cross the remote boundary.
			job, err := h.server.db.GetJobByID(event.JobID)
			if err != nil || !g.allRepos && !g.repoIDs[job.RepoID] {
				continue
			}
			event.WorktreePath = ""
			_ = controller.SetWriteDeadline(time.Now().Add(h.budget))
			if !writeHumaNDJSON(w, event) {
				return
			}
			if controller.Flush() != nil {
				return
			}
			_ = controller.SetWriteDeadline(time.Time{})
		}
	}
}

func (s *Server) startRemoteServer(cfg config.RemoteConfig) error {
	if !cfg.Enabled {
		return nil
	}
	handler, err := s.newRemoteHandler(cfg)
	if err != nil {
		return err
	}
	address := cfg.Listen
	if address == "" {
		address = "127.0.0.1:7375"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	headerBudget, _ := time.ParseDuration(cfg.ReadHeaderTimeout)
	readBudget, _ := time.ParseDuration(cfg.ReadTimeout)
	requestContext, cancelRequests := context.WithCancel(context.Background())
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: headerBudget, ReadTimeout: readBudget, MaxHeaderBytes: cfg.MaxHeaderBytes, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}, BaseContext: func(net.Listener) context.Context { return requestContext }}
	srv.RegisterOnShutdown(cancelRequests)
	if !cfg.TrustedProxy {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.TLSKeyFile)
		if err != nil {
			cancelRequests()
			_ = listener.Close()
			return errors.New("cannot load remote TLS identity")
		}
		srv.TLSConfig.Certificates = []tls.Certificate{cert}
		listener = tls.NewListener(listener, srv.TLSConfig)
	}
	s.browserMu.Lock()
	if s.browserStopping {
		s.browserMu.Unlock()
		cancelRequests()
		_ = listener.Close()
		return http.ErrServerClosed
	}
	s.remoteServer = srv
	s.remoteCancel = cancelRequests
	s.browserMu.Unlock()
	go func() {
		defer cancelRequests()
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Print("Remote HTTP server stopped unexpectedly")
		}
	}()
	return nil
}

func writeRemoteJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, value, responseJSONOptions)
}

func sanitizeRemoteJobs(jobs []storage.ReviewJob) {
	for i := range jobs {
		sanitizeRemoteJob(&jobs[i])
	}
}

func sanitizeRemoteJob(job *storage.ReviewJob) {
	job.PanelSummary = nil
	job.PanelMemberConfigJSON = ""
	job.CommandLine = ""
	job.WorktreePath = ""
	job.SessionID = ""
}

// abortRemoteServer tears down partial startup without leaving a listener
// behind. It only touches this Server's optional remote ingress.
func (s *Server) abortRemoteServer() {
	s.browserMu.Lock()
	srv, cancel := s.remoteServer, s.remoteCancel
	s.browserMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if srv != nil {
		_ = srv.Close()
	}
}
