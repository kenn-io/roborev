// Package katafake is an in-memory Kata server for tests. It answers with the
// generated response types from Kata v0.19.0, so a reply that stops matching
// Kata's OpenAPI shapes fails to compile instead of passing against a
// hand-written JSON string. It imports nothing from roborev, so tests in any
// package can use it.
package katafake

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	gen "go.kenn.io/kata/pkg/client/generated"
)

// Server is a small in-memory Kata server.
type Server struct {
	t   *testing.T
	srv *httptest.Server

	// Override, when set, sees every request after the credential check; it
	// returns true when it has answered the request itself.
	Override func(w http.ResponseWriter, r *http.Request) bool

	mu       sync.Mutex
	requests []Request

	// APISchema is reported by /api/v1/health.
	APISchema string
	// Token, when set, is the bearer credential every request must carry.
	Token string
	// Principal models an authenticated credential: Kata attributes writes to
	// it whatever actor the request names. Empty models a static token, which
	// needs the request to name an actor.
	Principal string

	Projects    []gen.ProjectOut
	issues      map[int64][]*Issue
	nextID      int64
	nextComment int64
	keys        map[string]keyed
	// ListPages, when set, replaces the issue list with canned pages keyed by
	// the cursor query value ("" is the first page).
	ListPages map[string]gen.ListIssuesResponseBody
}

// Issue is a stored issue; tests edit Out, Comments, and Links directly.
type Issue struct {
	Out      gen.IssueOut
	Comments []gen.CommentOut
	Links    []gen.LinkOut
}

type keyed struct {
	fingerprint string
	issue       *Issue
	comment     *gen.Comment
}

// Request is a recorded request.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// JSON decodes the request body as a JSON object.
func (r Request) JSON(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.Body, &out); err != nil {
		t.Fatalf("request body is not a JSON object: %q", r.Body)
	}
	return out
}

// New starts a server with projects "reviews" (7) and "other" (8). It stops when
// the test ends.
func New(t *testing.T) *Server {
	t.Helper()
	f := &Server{
		t:         t,
		APISchema: "0.28.0",
		issues:    map[int64][]*Issue{},
		keys:      map[string]keyed{},
		nextID:    1,
		// Comment UIDs are 26 characters, like Kata's ULIDs.
		nextComment: 1,
	}
	f.AddProject(7, "reviews")
	f.AddProject(8, "other")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", f.Health)
	mux.HandleFunc("GET /api/v1/projects", f.ListProjects)
	mux.HandleFunc("GET /api/v1/projects/{pid}/issues", f.listIssues)
	mux.HandleFunc("POST /api/v1/projects/{pid}/issues", f.createIssue)
	mux.HandleFunc("GET /api/v1/projects/{pid}/issues/{ref}", f.showIssue)
	mux.HandleFunc("POST /api/v1/projects/{pid}/issues/{ref}/comments", f.createComment)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		f.mu.Lock()
		f.requests = append(f.requests, Request{r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone(), body})
		token := f.Token
		f.mu.Unlock()
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			f.Fail(w, http.StatusUnauthorized, "unauthorized", "bad credential")
			return
		}
		f.mu.Lock()
		override := f.Override
		f.mu.Unlock()
		if override != nil && override(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the server's base URL.
func (f *Server) URL() string { return f.srv.URL }

// Requests returns the requests received so far, excluding health probes.
func (f *Server) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Request
	for _, r := range f.requests {
		if r.Path != "/api/v1/health" {
			out = append(out, r)
		}
	}
	return out
}

// AllRequests returns every request received so far.
func (f *Server) AllRequests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// AddProject registers an active project.
func (f *Server) AddProject(id int64, name string) {
	f.Projects = append(f.Projects, gen.ProjectOut{ID: id, Name: name, UID: fmt.Sprintf("P%025d", id), Active: true})
}

// AddIssue stores an open issue and returns it for further edits.
func (f *Server) AddIssue(projectID int64, short, title, body string, labels ...string) *Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	project, projectUID := "", ""
	for _, p := range f.Projects {
		if p.ID == projectID {
			project, projectUID = p.Name, p.UID
		}
	}
	issue := &Issue{Out: gen.IssueOut{
		ID: f.nextID, ProjectID: projectID, ProjectUID: &projectUID, ShortID: short, QualifiedID: project + "#" + short,
		UID: fmt.Sprintf("I%025d", f.nextID), Title: title, Body: body, Status: "open", Labels: labels,
		Metadata: map[string]any{}, Revision: 1, Author: "someone",
	}}
	f.nextID++
	f.issues[projectID] = append(f.issues[projectID], issue)
	return issue
}

// Fail writes Kata's error envelope.
func (f *Server) Fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, map[string]any{"status": status, "error": map[string]any{"code": code, "message": message}})
}

// FailData is Fail with the error envelope's data member.
func (f *Server) FailData(w http.ResponseWriter, status int, code, message string, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, map[string]any{"status": status, "error": map[string]any{"code": code, "message": message, "data": data}})
}

// Reply writes value as a JSON response.
func (f *Server) Reply(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, value); err != nil {
		f.t.Errorf("fake kata cannot encode %T: %v", value, err)
	}
}

// Health answers like /api/v1/health.
func (f *Server) Health(w http.ResponseWriter, _ *http.Request) {
	schema := f.APISchema
	h := gen.HealthResponseBody{Ok: true, Version: "v0.19.0", Uptime: "1s", StartedAt: time.Unix(0, 0).UTC(), SchemaVersion: 32}
	if schema != "" {
		h.APISchemaVersion = &schema
	}
	f.Reply(w, h)
}

// ListProjects answers like GET /api/v1/projects.
func (f *Server) ListProjects(w http.ResponseWriter, _ *http.Request) {
	f.Reply(w, gen.ListProjectsResponseBody{Projects: f.Projects})
}

func (f *Server) project(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("pid"), 10, 64)
	for _, p := range f.Projects {
		if err == nil && p.ID == id {
			return id, true
		}
	}
	f.Fail(w, http.StatusNotFound, "project_not_found", "no such project")
	return 0, false
}

// actor models Kata's attribution: an authenticated credential overrides the
// requested actor, and a static token has to name one.
func (f *Server) actor(w http.ResponseWriter, requested *string) (string, bool) {
	if f.Principal != "" {
		return f.Principal, true
	}
	if requested == nil || *requested == "" {
		f.Fail(w, http.StatusBadRequest, "actor_required", "actor is required")
		return "", false
	}
	return *requested, true
}

func (f *Server) listIssues(w http.ResponseWriter, r *http.Request) {
	pid, ok := f.project(w, r)
	if !ok {
		return
	}
	if f.ListPages != nil {
		page, found := f.ListPages[r.URL.Query().Get("cursor")]
		if !found {
			f.Fail(w, http.StatusBadRequest, "bad_cursor", "unknown cursor")
			return
		}
		f.Reply(w, page)
		return
	}
	status := r.URL.Query().Get("status")
	var out []gen.IssueOut
	for _, issue := range f.issues[pid] {
		if status == "" || issue.Out.Status == status {
			out = append(out, issue.Out)
		}
	}
	f.Reply(w, gen.ListIssuesResponseBody{Issues: out, Complete: true})
}

func (f *Server) find(pid int64, ref string) *Issue {
	for _, issue := range f.issues[pid] {
		if strings.EqualFold(issue.Out.ShortID, ref) || issue.Out.UID == ref {
			return issue
		}
	}
	return nil
}

func (f *Server) showIssue(w http.ResponseWriter, r *http.Request) {
	pid, ok := f.project(w, r)
	if !ok {
		return
	}
	issue := f.find(pid, r.PathValue("ref"))
	if issue == nil {
		f.Fail(w, http.StatusNotFound, "issue_not_found", "no such issue")
		return
	}
	out := issue.Out
	show := gen.ShowIssueOut{
		Author: out.Author, Body: out.Body, ID: out.ID, Labels: out.Labels, Metadata: out.Metadata,
		ProjectID: out.ProjectID, ProjectUID: out.ProjectUID, Revision: out.Revision, ShortID: out.ShortID, Status: out.Status,
		Title: out.Title, UID: out.UID,
	}
	labels := make([]gen.IssueLabel, 0, len(out.Labels))
	for _, l := range out.Labels {
		labels = append(labels, gen.IssueLabel{Label: l})
	}
	body := gen.ShowIssueResponseBody{Issue: show, Labels: labels, Links: append([]gen.LinkOut{}, issue.Links...), Comments: append([]gen.CommentOut{}, issue.Comments...)}
	f.Reply(w, body)
}

func (f *Server) createIssue(w http.ResponseWriter, r *http.Request) {
	pid, ok := f.project(w, r)
	if !ok {
		return
	}
	var req gen.CreateIssueRequestBody
	if err := json.UnmarshalRead(r.Body, &req); err != nil {
		f.Fail(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	author, ok := f.actor(w, req.Actor)
	if !ok {
		return
	}
	fingerprint := fmt.Sprintf("%s|%s|%v", req.Title, deref(req.Body), req.Labels)
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		f.mu.Lock()
		prior, seen := f.keys[key]
		f.mu.Unlock()
		if seen {
			if prior.fingerprint != fingerprint {
				f.Fail(w, http.StatusConflict, "idempotency_mismatch", "key reused with a different request")
				return
			}
			reused := true
			f.Reply(w, gen.MutationResponseBody{Issue: asIssue(prior.issue.Out), Reused: &reused})
			return
		}
	}
	short := fmt.Sprintf("n%03d", f.nextID)
	issue := f.AddIssue(pid, short, req.Title, deref(req.Body), req.Labels...)
	issue.Out.Author = author
	issue.Out.Priority = req.Priority
	if req.Metadata != nil {
		issue.Out.Metadata = req.Metadata
	}
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		f.mu.Lock()
		f.keys[key] = keyed{fingerprint: fingerprint, issue: issue}
		f.mu.Unlock()
	}
	reused := false
	f.Reply(w, gen.MutationResponseBody{Changed: true, Issue: asIssue(issue.Out), Reused: &reused})
}

func asIssue(out gen.IssueOut) gen.Issue {
	return gen.Issue{
		Author: out.Author, Body: out.Body, ID: out.ID, Metadata: out.Metadata, ProjectID: out.ProjectID, ProjectUID: out.ProjectUID,
		Revision: out.Revision, ShortID: out.ShortID, Status: out.Status, Title: out.Title, UID: out.UID,
		Priority: out.Priority, Owner: out.Owner, CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
	}
}

// minEvidenceRunes mirrors internal/db/comment_reply_write.go:49 at v0.19.0. It
// is deliberately written out again here instead of imported from roborev's
// client, so a test of that client's rule checks it against an independent copy.
const minEvidenceRunes = 40

// findComment returns the project's comment with uid, and the issue holding it.
func (f *Server) findComment(pid int64, uid string) (gen.CommentOut, *Issue, bool) {
	for _, issue := range f.issues[pid] {
		for _, c := range issue.Comments {
			if strings.EqualFold(c.UID, uid) {
				return c, issue, true
			}
		}
	}
	return gen.CommentOut{}, nil, false
}

// createComment answers POST .../comments with Kata v0.19.0's rules for a plain
// comment and for a typed reply, in Kata's order: replay by idempotency key,
// then body, kind and target pairing, a closed issue, a missing target, too
// little evidence, and a duplicate (internal/daemon/handlers_comments.go and
// internal/db/comment_reply_write.go).
func (f *Server) createComment(w http.ResponseWriter, r *http.Request) {
	pid, ok := f.project(w, r)
	if !ok {
		return
	}
	issue := f.find(pid, r.PathValue("ref"))
	if issue == nil {
		f.Fail(w, http.StatusNotFound, "issue_not_found", "no such issue")
		return
	}
	var req gen.CommentRequestBody
	if err := json.UnmarshalRead(r.Body, &req); err != nil {
		f.Fail(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	author, ok := f.actor(w, req.Actor)
	if !ok {
		return
	}
	kind, replyTo, teammate := "", deref(req.ReplyTo), deref(req.Teammate)
	if req.Kind != nil {
		kind = string(*req.Kind)
	}
	fingerprint := fmt.Sprintf("%s|%s|%s|%s|%s|%s", issue.Out.UID, author, req.Body, kind, replyTo, teammate)
	key := r.Header.Get("Idempotency-Key")

	f.mu.Lock()
	defer f.mu.Unlock()
	if prior, seen := f.keys[key]; key != "" && seen {
		if prior.comment == nil || prior.fingerprint != fingerprint {
			f.Fail(w, http.StatusConflict, "idempotency_mismatch", "idempotency key matched a prior comment with a different fingerprint")
			return
		}
		f.Reply(w, gen.CommentResponseBody{Changed: false, Comment: *prior.comment, Issue: asIssue(issue.Out)})
		return
	}
	if strings.TrimSpace(req.Body) == "" || (replyTo == "") != (kind == "") || (kind != "" && !slices.Contains([]string{"reply", "confirm", "refute", "supersede"}, kind)) {
		f.Fail(w, http.StatusBadRequest, "validation", "reply_to and a valid kind must be supplied together")
		return
	}
	if replyTo != "" {
		if issue.Out.Status != "open" {
			f.Fail(w, http.StatusConflict, "issue_closed", "reopen the reply issue before adding a typed reply")
			return
		}
		target, _, found := f.findComment(pid, replyTo)
		if !found {
			f.Fail(w, http.StatusNotFound, "comment_not_found", "comment not found")
			return
		}
		replyTo = target.UID
		if (kind == "confirm" || kind == "refute") && utf8.RuneCountInString(strings.Join(strings.Fields(req.Body), " ")) < minEvidenceRunes {
			f.Fail(w, http.StatusBadRequest, "validation", kind+" body needs at least 40 characters after whitespace normalization")
			return
		}
		for _, other := range f.issues[pid] {
			for _, c := range other.Comments {
				if c.ReplyToUID != nil && *c.ReplyToUID == replyTo && deref(c.ReplyKind) == kind && c.Author == author && deref(c.Teammate) == teammate {
					f.FailData(w, http.StatusConflict, "duplicate_reply", "a reply of this kind already exists for this author and teammate",
						map[string]any{"existing_uid": c.UID, "existing_handle": deref(c.Handle)})
					return
				}
			}
		}
	}
	uid := fmt.Sprintf("C%025d", f.nextComment)
	handle := "c:" + strings.ToLower(uid[len(uid)-6:])
	created := time.Unix(f.nextComment, 0).UTC()
	f.nextComment++
	out := gen.CommentOut{
		ID: int64(len(issue.Comments) + 1), IssueID: issue.Out.ID, UID: uid, Handle: &handle, Author: author, Body: req.Body,
		CreatedAt: created, IssueUID: &issue.Out.UID, IssueShortID: &issue.Out.ShortID, ProjectUID: issue.Out.ProjectUID,
	}
	comment := gen.Comment{ID: out.ID, IssueID: out.IssueID, UID: uid, Author: author, Body: req.Body, CreatedAt: created}
	if teammate != "" {
		out.Teammate, comment.Teammate = &teammate, &teammate
	}
	if kind != "" {
		out.ReplyKind, comment.ReplyKind = &kind, &kind
		out.ReplyToUID, comment.ReplyToUID = &replyTo, &replyTo
	}
	issue.Comments = append(issue.Comments, out)
	if key != "" {
		f.keys[key] = keyed{fingerprint: fingerprint, comment: &comment}
	}
	f.Reply(w, gen.CommentResponseBody{Changed: true, Comment: comment, Issue: asIssue(issue.Out)})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
