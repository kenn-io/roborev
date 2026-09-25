package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	gitcmd "go.kenn.io/kit/git/cmd"

	"go.kenn.io/roborev/internal/storage"
)

// RemotePackPath receives git packs of commits the daemon clone lacks.
const RemotePackPath = "/api/remote/pack"

// remoteAPIRoutes lists every route the remote listener serves and the
// minimum access level each needs. Everything else is refused.
var remoteAPIRoutes = map[routeKey]RemoteAccess{
	{http.MethodGet, "/api/ping"}:          RemoteAccessRead,
	{http.MethodGet, "/api/status"}:        RemoteAccessRead,
	{http.MethodGet, "/api/health"}:        RemoteAccessRead,
	{http.MethodGet, "/api/jobs"}:          RemoteAccessRead,
	{http.MethodGet, "/api/review"}:        RemoteAccessRead,
	{http.MethodGet, "/api/search"}:        RemoteAccessRead,
	{http.MethodGet, "/api/comments"}:      RemoteAccessRead,
	{http.MethodGet, "/api/repos"}:         RemoteAccessRead,
	{http.MethodGet, "/api/branches"}:      RemoteAccessRead,
	{http.MethodGet, "/api/summary"}:       RemoteAccessRead,
	{http.MethodGet, "/api/cost"}:          RemoteAccessRead,
	{http.MethodGet, "/api/activity"}:      RemoteAccessRead,
	{http.MethodGet, "/api/job/output"}:    RemoteAccessRead,
	{http.MethodGet, "/api/job/log"}:       RemoteAccessRead,
	{http.MethodGet, "/api/stream/events"}: RemoteAccessRead,
	{http.MethodPost, "/api/jobs/batch"}:   RemoteAccessRead,
	{http.MethodPost, "/api/enqueue"}:      RemoteAccessQueue,
	{http.MethodPost, "/api/job/cancel"}:   RemoteAccessQueue,
	{http.MethodPost, "/api/job/rerun"}:    RemoteAccessQueue,
	{http.MethodPost, "/api/review/close"}: RemoteAccessQueue,
	{http.MethodPost, "/api/comment"}:      RemoteAccessQueue,
	{http.MethodPost, RemotePackPath}:      RemoteAccessQueue,
}

type remoteCallerContextKey struct{}

// RemoteCallerFromContext reports the tailnet caller of a request that
// arrived on the remote listener.
func RemoteCallerFromContext(ctx context.Context) (RemoteCaller, bool) {
	caller, ok := ctx.Value(remoteCallerContextKey{}).(RemoteCaller)
	return caller, ok
}

// remoteConnAuth caches a successful whois result per TCP connection, so a
// revoked grant takes effect on the peer's next connection. A failed whois is
// not cached: the next request on the connection runs whois again.
type remoteConnAuth struct {
	mu     sync.Mutex
	caller *RemoteCaller
}

type remoteConnAuthKey struct{}

func remoteConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, remoteConnAuthKey{}, &remoteConnAuth{})
}

type remoteError struct {
	status int
	msg    string
}

func (e *remoteError) Error() string { return e.msg }

func newRemoteError(status int, format string, args ...any) *remoteError {
	return &remoteError{status: status, msg: fmt.Sprintf(format, args...)}
}

func writeRemoteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, body)
}

// decodeRemoteBody decodes a request body with the same encoding/json/v2
// defaults the core Huma handlers use: member names match case-sensitively,
// duplicate names are an error, and unknown members are ignored. The gates
// then forward only the re-encoded, checked value (see forwardRemoteBody),
// so the core handler never reads bytes the gate did not check.
func decodeRemoteBody(r *http.Request, what string, v any) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read %s request: %w", what, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return newRemoteError(http.StatusBadRequest, "decode %s request: %v", what, err)
	}
	return nil
}

// forwardRemoteBody replaces the request body with the encoding of the
// value a gate checked.
func forwardRemoteBody(r *http.Request, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return nil
}

func writeRemoteError(w http.ResponseWriter, err error) {
	if re, ok := errors.AsType[*remoteError](err); ok {
		writeRemoteJSON(w, re.status, ErrorResponse{Error: re.msg})
		return
	}
	writeRemoteJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
}

func authenticateRemote(r *http.Request, whois whoisFunc) (RemoteCaller, error) {
	auth, ok := r.Context().Value(remoteConnAuthKey{}).(*remoteConnAuth)
	if !ok {
		return RemoteCaller{}, errors.New("remote connection has no identity state")
	}
	auth.mu.Lock()
	defer auth.mu.Unlock()
	if auth.caller != nil {
		return *auth.caller, nil
	}
	caller, err := whois(r.Context(), r.RemoteAddr)
	if err != nil {
		return RemoteCaller{}, err
	}
	auth.caller = &caller
	return caller, nil
}

func (s *Server) newRemoteHandler(core http.Handler, whois whoisFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := authenticateRemote(r, whois)
		if err != nil {
			writeRemoteError(w, newRemoteError(http.StatusForbidden, "%v", err))
			return
		}
		if caller.Access == RemoteAccessNone {
			writeRemoteError(w, newRemoteError(http.StatusForbidden,
				"tailnet policy grants this node no roborev access"))
			return
		}
		need, found := remoteAPIRoutes[routeKey{r.Method, r.URL.Path}]
		if !found {
			writeRemoteError(w, newRemoteError(http.StatusForbidden,
				"%s is not available over the remote API; run it on the daemon host", r.URL.Path))
			return
		}
		if caller.Access < need {
			writeRemoteError(w, newRemoteError(http.StatusForbidden,
				"%s requires %s access; this node has %s access", r.URL.Path, need, caller.Access))
			return
		}
		if r.Method != http.MethodGet {
			log.Printf("remote %s %s by node=%s login=%s tags=%v access=%s",
				r.Method, r.URL.Path, caller.Node, caller.Login, caller.Tags, caller.Access)
		}
		r = r.WithContext(context.WithValue(r.Context(), remoteCallerContextKey{}, caller))
		switch r.URL.Path {
		case "/api/enqueue":
			s.serveRemoteEnqueue(w, r, core)
		case "/api/job/rerun":
			s.serveRemoteRerun(w, r, core)
		case RemotePackPath:
			s.serveRemotePack(w, r)
		default:
			if err := s.rewriteRemoteRepoFilters(r); err != nil {
				writeRemoteError(w, err)
				return
			}
			core.ServeHTTP(w, r)
		}
	})
}

// resolveRemoteRepo maps a repo identity to the single registered checkout
// with that identity.
func (s *Server) resolveRemoteRepo(identity string) (*storage.Repo, error) {
	if identity == "" {
		return nil, newRemoteError(http.StatusBadRequest, "repo identity is required")
	}
	repos, err := s.db.FindReposByIdentity(identity)
	if err != nil {
		return nil, fmt.Errorf("look up repo %s: %w", identity, err)
	}
	switch len(repos) {
	case 0:
		return nil, newRemoteError(http.StatusNotFound,
			"repo %s is not registered on the daemon host; run roborev init there, or add a matching .roborev-id",
			identity)
	case 1:
		return &repos[0], nil
	default:
		paths := make([]string, len(repos))
		for i, repo := range repos {
			paths[i] = repo.RootPath
		}
		return nil, newRemoteError(http.StatusConflict,
			"repo %s matches several daemon checkouts: %s", identity, strings.Join(paths, ", "))
	}
}

// rewriteRemoteRepoFilters turns identity values of the repo filter into
// daemon root paths. Exact registered root paths (which the caller got from
// /api/repos) pass through unchanged.
func (s *Server) rewriteRemoteRepoFilters(r *http.Request) error {
	q := r.URL.Query()
	if q.Has("repo_prefix") || (r.URL.Path == "/api/repos" && q.Has("prefix")) {
		return newRemoteError(http.StatusBadRequest,
			"path-prefix filters are not available over the remote API; filter by repo identity")
	}
	values := q["repo"]
	if len(values) == 0 {
		return nil
	}
	rewritten := make([]string, 0, len(values))
	for _, value := range values {
		// Only absolute values can be daemon root paths. GetRepoByPath would
		// resolve a relative identity against the daemon's working directory.
		if filepath.IsAbs(value) {
			repo, err := s.db.GetRepoByPath(value)
			switch {
			case err == nil && repo.RootPath != repo.Identity:
				rewritten = append(rewritten, repo.RootPath)
				continue
			case err != nil && !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("look up repo path %s: %w", value, err)
			}
		}
		repo, err := s.resolveRemoteRepo(value)
		if err != nil {
			return err
		}
		rewritten = append(rewritten, repo.RootPath)
	}
	q["repo"] = rewritten
	r.URL.RawQuery = q.Encode()
	return nil
}

func validateRemoteEnqueue(req *EnqueueRequest) error {
	if req.RepoPath != "" {
		return newRemoteError(http.StatusBadRequest,
			"repo_path is not accepted over the remote API; send repo_identity")
	}
	if req.RepoIdentity == "" {
		return newRemoteError(http.StatusBadRequest, "repo_identity is required")
	}
	kind := ""
	switch {
	case req.CustomPrompt != "":
		kind = storage.JobTypeTask
	case req.Agentic:
		kind = "agentic"
	case req.GitRef == "dirty" || req.DiffContent != "" || len(req.DirtyFiles) > 0:
		kind = storage.JobTypeDirty
	case req.Since != "" || req.JobType == storage.JobTypeInsights:
		kind = storage.JobTypeInsights
	case req.AnalysisType != "" || len(req.AnalysisFiles) > 0 || req.AnalysisCommitSHA != "":
		kind = "analyze"
	case req.JobType != "" && req.JobType != storage.JobTypeReview && req.JobType != storage.JobTypeRange:
		kind = req.JobType
	}
	if kind != "" {
		return newRemoteError(http.StatusForbidden, "%s reviews need a local daemon", kind)
	}
	return nil
}

// validBranchName reports whether name is a literal branch name. The "@{"
// check rejects @{-N} shorthands, which check-ref-format --branch would
// expand against the daemon clone's checkout history.
func validBranchName(ctx context.Context, repoRoot, name string) bool {
	if strings.HasPrefix(name, "-") || strings.Contains(name, "@{") {
		return false
	}
	_, _, err := gitcmd.New().Run(ctx, repoRoot, nil, "check-ref-format", "--branch", name)
	return err == nil
}

func (s *Server) serveRemoteEnqueue(w http.ResponseWriter, r *http.Request, core http.Handler) {
	var req EnqueueRequest
	if err := decodeRemoteBody(r, "enqueue", &req); err != nil {
		writeRemoteError(w, err)
		return
	}
	if req.GitRef == "" {
		req.GitRef, req.CommitSHA = req.CommitSHA, ""
	}
	if err := validateRemoteEnqueue(&req); err != nil {
		writeRemoteError(w, err)
		return
	}
	shas, err := parseRemoteGitRef(req.GitRef)
	if err != nil {
		writeRemoteError(w, newRemoteError(http.StatusBadRequest, "%v", err))
		return
	}
	repo, err := s.resolveRemoteRepo(req.RepoIdentity)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	if req.Branch != "" && !validBranchName(r.Context(), repo.RootPath, req.Branch) {
		writeRemoteError(w, newRemoteError(http.StatusBadRequest, "invalid branch name %q", req.Branch))
		return
	}
	missing, err := ensureRemoteCommits(r.Context(), repo.RootPath, shas)
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	if len(missing) > 0 {
		haves, err := daemonHaves(r.Context(), repo.RootPath)
		if err != nil {
			writeRemoteError(w, err)
			return
		}
		writeRemoteJSON(w, http.StatusConflict, MissingCommitsResponse{
			Error:   "commits are missing from the daemon clone; upload them and retry",
			Code:    MissingCommitsCode,
			Missing: missing,
			Have:    haves,
		})
		return
	}
	req.RepoPath, req.RepoIdentity = repo.RootPath, ""
	if err := forwardRemoteBody(r, req); err != nil {
		writeRemoteError(w, err)
		return
	}
	core.ServeHTTP(w, r)
}

func remoteReviewJob(job *storage.ReviewJob) bool {
	return job.IsReviewJob() && !job.Agentic &&
		job.JobType != storage.JobTypeDirty && job.GitRef != "dirty" && job.DiffContent == nil
}

func (s *Server) remoteRerunAllowed(job *storage.ReviewJob) (bool, error) {
	if !job.IsSynthesisJob() {
		return remoteReviewJob(job), nil
	}
	if job.PanelRunUUID == nil {
		return false, nil
	}
	members, err := s.db.GetPanelMembers(*job.PanelRunUUID)
	if err != nil {
		return false, fmt.Errorf("load panel members: %w", err)
	}
	for i := range members {
		if !remoteReviewJob(&members[i]) {
			return false, nil
		}
	}
	return len(members) > 0, nil
}

func (s *Server) serveRemoteRerun(w http.ResponseWriter, r *http.Request, core http.Handler) {
	var req RerunJobRequest
	if err := decodeRemoteBody(r, "rerun", &req); err != nil {
		writeRemoteError(w, err)
		return
	}
	// A missing job falls through to the core handler's 404. Any other
	// lookup failure stops here: the core handler reads the job again and
	// must never see a job this gate did not check.
	job, err := s.db.GetJobByID(req.JobID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		writeRemoteError(w, fmt.Errorf("look up job %d: %w", req.JobID, err))
		return
	default:
		allowed, err := s.remoteRerunAllowed(job)
		if err != nil {
			writeRemoteError(w, err)
			return
		}
		if !allowed {
			writeRemoteError(w, newRemoteError(http.StatusForbidden,
				"rerunning job %d needs a local daemon: only non-agentic commit and range reviews rerun remotely", req.JobID))
			return
		}
	}
	if err := forwardRemoteBody(r, req); err != nil {
		writeRemoteError(w, err)
		return
	}
	core.ServeHTTP(w, r)
}

func (s *Server) serveRemotePack(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tips := q["tip"]
	if len(tips) == 0 {
		writeRemoteError(w, newRemoteError(http.StatusBadRequest, "at least one tip is required"))
		return
	}
	for _, tip := range tips {
		if !isFullSHA(tip) {
			writeRemoteError(w, newRemoteError(http.StatusBadRequest, "tip %q is not a full commit SHA", tip))
			return
		}
	}
	repo, err := s.resolveRemoteRepo(q.Get("repo_identity"))
	if err != nil {
		writeRemoteError(w, err)
		return
	}
	if err := importPack(r.Context(), repo.RootPath, r.Body, tips); err != nil {
		writeRemoteError(w, classifyPackError(err))
		return
	}
	writeRemoteJSON(w, http.StatusOK, map[string][]string{"pinned": tips})
}

// classifyPackError maps an importPack failure to a response: 409 when the
// clone lacks the pack's base commits, 400 when git rejected the pack or a
// tip is not a full SHA, and 500 for daemon-side failures such as exec or
// I/O errors and cancellation.
func classifyPackError(err error) error {
	if _, ok := errors.AsType[*missingBaseError](err); ok {
		return newRemoteError(http.StatusConflict, "%v", err)
	}
	if _, ok := errors.AsType[*badPackError](err); ok || errors.Is(err, errRemoteRefNotSHA) {
		return newRemoteError(http.StatusBadRequest, "%v", err)
	}
	return err
}
