package mcpserver

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// ReviewerHTTPPath is where the reviewer-only MCP endpoint is served. It is
// separate from HTTPPath and accepts only per-job reviewer tokens.
const ReviewerHTTPPath = "/mcp/review"

const defaultGitLogCount = 20

// Reviewer tool names. Agents allow exactly these tools.
const (
	ReviewerToolBranchFindings = "branch_findings"
	ReviewerToolPriorReview    = "prior_review"
	ReviewerToolGitLog         = "git_log"
	ReviewerToolGitBlame       = "git_blame"
)

// ReviewerToolNames lists every tool the reviewer endpoint serves.
var ReviewerToolNames = []string{
	ReviewerToolBranchFindings, ReviewerToolPriorReview, ReviewerToolGitLog, ReviewerToolGitBlame,
}

// ReviewerScope is what one reviewer token grants: read access to finished
// reviews of one branch's lineage in one repository, and git history at one
// commit. Tools take every scope value from here, never from arguments.
type ReviewerScope struct {
	JobID    int64
	RepoID   int64
	RepoPath string
	Branch   string
	SHA      string
}

// ReviewerVerifier resolves a reviewer token to its scope and expiry.
type ReviewerVerifier func(token string) (ReviewerScope, time.Time, bool)

// BranchReview is one finished review with its prompt-trusted comments.
type BranchReview struct {
	Job      storage.ReviewJob
	Review   storage.Review
	Comments []storage.Response
}

// ReviewerBackend loads review data for the reviewer tools.
type ReviewerBackend interface {
	// BranchReviews returns finished, non-member reviews recorded on branch
	// in the repository, newest first, with prompt-trusted comments only.
	BranchReviews(ctx context.Context, repoID int64, branch string) ([]BranchReview, error)
}

// Reviewer is the reviewer-only MCP server.
type Reviewer struct {
	backend ReviewerBackend
	mcp     *mcp.Server
}

// NewReviewer builds the reviewer-only MCP server. backend must not be nil.
func NewReviewer(backend ReviewerBackend, version string) *Reviewer {
	if backend == nil {
		panic("mcpserver: reviewer backend is required")
	}
	r := &Reviewer{backend: backend}
	r.mcp = mcp.NewServer(
		&mcp.Implementation{Name: "roborev-review", Version: version},
		&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}},
	)
	r.registerTools()
	return r
}

// HTTPHandler serves the reviewer tools at ReviewerHTTPPath to callers that
// present a bearer token verify accepts.
func (r *Reviewer) HTTPHandler(verify ReviewerVerifier) http.Handler {
	stream := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return r.mcp },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	verifier := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		scope, expires, ok := verify(token)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{Expiration: expires, Extra: map[string]any{reviewerScopeKey: scope}}, nil
	}
	authed := auth.RequireBearerToken(verifier, nil)(stream)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != ReviewerHTTPPath {
			http.NotFound(w, req)
			return
		}
		authed.ServeHTTP(w, req)
	})
}

const reviewerScopeKey = "roborev_reviewer_scope"

type branchFindingsInput struct{}

type reviewerFinding struct {
	Severity string `json:"severity"`
	Location string `json:"location,omitempty"`
	Claim    string `json:"claim"`
}

type reviewerComment struct {
	Author string `json:"author"`
	Text   string `json:"text"`
}

type reviewerReviewRow struct {
	JobID      int64             `json:"job_id"`
	Commit     string            `json:"commit"`
	Subject    string            `json:"subject,omitempty"`
	ReviewType string            `json:"review_type"`
	Verdict    string            `json:"verdict,omitempty"`
	Closed     bool              `json:"closed"`
	Findings   []reviewerFinding `json:"findings"`
	Note       string            `json:"note,omitempty"`
	Comments   []reviewerComment `json:"comments,omitempty"`
}

type branchFindingsOutput struct {
	Branch  string              `json:"branch"`
	Reviews []reviewerReviewRow `json:"reviews"`
}

type priorReviewInput struct {
	JobID  int64  `json:"job_id,omitempty" jsonschema:"job_id from branch_findings; takes precedence over commit"`
	Commit string `json:"commit,omitempty" jsonschema:"commit SHA or prefix of at least 7 characters"`
}

type priorReviewRow struct {
	JobID      int64             `json:"job_id"`
	Commit     string            `json:"commit"`
	Subject    string            `json:"subject,omitempty"`
	ReviewType string            `json:"review_type"`
	Verdict    string            `json:"verdict,omitempty"`
	Closed     bool              `json:"closed"`
	Output     string            `json:"output"`
	Comments   []reviewerComment `json:"comments,omitempty"`
}

type priorReviewOutput struct {
	Reviews []priorReviewRow `json:"reviews"`
}

type gitLogInput struct {
	Path      string `json:"path" jsonschema:"file path relative to the repository root"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"first line of a range to trace; omit for the whole file"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"last line of the range; defaults to start_line"`
	MaxCount  int    `json:"max_count,omitempty" jsonschema:"maximum commits to return; default 20"`
}

type historyCommit struct {
	Commit   string  `json:"commit"`
	Date     string  `json:"date"`
	Author   string  `json:"author"`
	Subject  string  `json:"subject"`
	OnBranch bool    `json:"on_branch" jsonschema:"true when the commit is part of the reviewed branch's own history"`
	FixFor   []int64 `json:"fix_for_jobs,omitempty" jsonschema:"earlier review job_ids whose comments name this commit, usually as the fix for a finding"`
}

type gitLogOutput struct {
	Commits []historyCommit `json:"commits"`
}

type gitBlameInput struct {
	Path      string `json:"path" jsonschema:"file path relative to the repository root"`
	StartLine int    `json:"start_line" jsonschema:"first line to blame"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"last line to blame; defaults to start_line"`
}

type blameHunk struct {
	Lines string `json:"lines"`
	historyCommit
}

type gitBlameOutput struct {
	Hunks []blameHunk `json:"hunks"`
}

func (r *Reviewer) registerTools() {
	mcp.AddTool(r.mcp, &mcp.Tool{
		Name:        ReviewerToolBranchFindings,
		Annotations: reviewerToolAnnotations(),
		Description: "List findings from earlier finished reviews of this branch, newest first, with each " +
			"review's closed state and the comments recording what was decided (fixed, declined, invalid). " +
			"Call this before reporting a finding on code a recent commit changed: if the same issue was " +
			"already raised and decided, do not raise it again unless the code now defeats that decision, " +
			"and do not ask to undo a change that fixed an earlier finding.",
	}, reviewerTool(r.branchFindings))
	mcp.AddTool(r.mcp, &mcp.Tool{
		Name:        ReviewerToolPriorReview,
		Annotations: reviewerToolAnnotations(),
		Description: "Return the full text and comments of an earlier review of this branch, by job_id " +
			"from branch_findings or by commit. Use it when a one-line claim is not enough to tell " +
			"whether your finding repeats or reverses an earlier one.",
	}, reviewerTool(r.priorReview))
	mcp.AddTool(r.mcp, &mcp.Tool{
		Name:        ReviewerToolGitLog,
		Annotations: reviewerToolAnnotations(),
		Description: "List the commits that changed a file, or a line range of it, at the reviewed " +
			"commit, newest first. fix_for_jobs names earlier reviews whose comments cite the commit, " +
			"which usually means it was written to fix one of their findings.",
	}, reviewerTool(r.gitLog))
	mcp.AddTool(r.mcp, &mcp.Tool{
		Name:        ReviewerToolGitBlame,
		Annotations: reviewerToolAnnotations(),
		Description: "Show which commit last changed each line in a range at the reviewed commit. " +
			"Use it before flagging a line to learn whether a fix for an earlier finding wrote it; " +
			"if so, read that finding with prior_review and hold the line to the defect bar rather " +
			"than asking to reverse the fix.",
	}, reviewerTool(r.gitBlame))
}

// reviewerToolAnnotations marks every reviewer tool read-only and closed
// world, so clients that gate tool calls on these hints can run them unprompted.
func reviewerToolAnnotations() *mcp.ToolAnnotations {
	no := false
	return &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &no, IdempotentHint: true, OpenWorldHint: &no}
}

// reviewerTool adapts a scoped handler, taking the scope from the verified
// bearer token of the HTTP request that carried the call.
func reviewerTool[In, Out any](
	fn func(context.Context, ReviewerScope, In) (Out, error),
) mcp.ToolHandlerFor[In, Out] {
	return wrapToolWithRequest(func(ctx context.Context, req *mcp.CallToolRequest, in In) (Out, error) {
		var scope ReviewerScope
		var ok bool
		if req.Extra != nil && req.Extra.TokenInfo != nil {
			scope, ok = req.Extra.TokenInfo.Extra[reviewerScopeKey].(ReviewerScope)
		}
		if !ok {
			var zero Out
			return zero, NewError(ErrorCodeUnavailable, "reviewer scope is missing")
		}
		return fn(ctx, scope, in)
	})
}

func wrapToolWithRequest[In, Out any](
	fn func(context.Context, *mcp.CallToolRequest, In) (Out, error),
) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := fn(ctx, req, in)
		if err == nil {
			return nil, out, nil
		}
		result, resultErr := toolErrorResult(err)
		return result, out, resultErr
	}
}

// scopedHistory is the review history a scope may read.
type scopedHistory struct {
	reviews []BranchReview
	lineage *gitpkg.BranchLineageMatcher
}

func (r *Reviewer) history(ctx context.Context, scope ReviewerScope) (scopedHistory, error) {
	lineage, err := gitpkg.NewBranchLineageMatcherCtx(ctx, scope.RepoPath, scope.Branch, scope.SHA)
	if err != nil {
		return scopedHistory{}, NewError(ErrorCodeUnavailable, "branch history is unavailable")
	}
	all, err := r.backend.BranchReviews(ctx, scope.RepoID, scope.Branch)
	if err != nil {
		return scopedHistory{}, err
	}
	refs := make([]string, 0, len(all))
	for _, review := range all {
		refs = append(refs, review.Job.GitRef)
	}
	if err := lineage.ResolveRefs(ctx, refs); err != nil {
		return scopedHistory{}, NewError(ErrorCodeUnavailable, "branch history is unavailable")
	}
	h := scopedHistory{lineage: lineage}
	for _, review := range all {
		// The backend filters by repository and branch; the lineage check
		// drops reviews of commits this branch no longer contains, such as
		// ones from before a rebase or of a reused branch name.
		if review.Job.RepoID != scope.RepoID || review.Job.Branch != scope.Branch ||
			review.Job.ID == scope.JobID || !lineage.Matches(review.Job.GitRef) {
			continue
		}
		h.reviews = append(h.reviews, review)
	}
	return h, nil
}

func (r *Reviewer) branchFindings(
	ctx context.Context, scope ReviewerScope, _ branchFindingsInput,
) (branchFindingsOutput, error) {
	h, err := r.history(ctx, scope)
	if err != nil {
		return branchFindingsOutput{}, err
	}
	out := branchFindingsOutput{Branch: scope.Branch, Reviews: make([]reviewerReviewRow, 0, len(h.reviews))}
	for _, review := range h.reviews {
		row := reviewerReviewRow{
			JobID:      review.Job.ID,
			Commit:     shortRef(review.Job.GitRef),
			Subject:    review.Job.CommitSubject,
			ReviewType: reviewTypeLabel(review.Job.ReviewType),
			Verdict:    verdictLabel(review.Job.Verdict),
			Closed:     review.Review.Closed,
			Findings:   []reviewerFinding{},
			Comments:   reviewerComments(review.Comments),
		}
		doc, ok := decodeReviewDocument(review.Review.StructuredOutput)
		if !ok {
			row.Note = "findings are not structured; read this review with prior_review"
		}
		for _, finding := range doc.Findings {
			row.Findings = append(row.Findings, reviewerFinding{
				Severity: finding.Severity,
				Location: finding.Location,
				Claim:    shortClaim(finding.Problem),
			})
		}
		out.Reviews = append(out.Reviews, row)
	}
	return out, nil
}

func (r *Reviewer) priorReview(
	ctx context.Context, scope ReviewerScope, in priorReviewInput,
) (priorReviewOutput, error) {
	commit := strings.ToLower(strings.TrimSpace(in.Commit))
	if in.JobID <= 0 && len(commit) < 7 {
		return priorReviewOutput{}, NewError(ErrorCodeInvalidArgument,
			"job_id or a commit of at least 7 characters is required")
	}
	h, err := r.history(ctx, scope)
	if err != nil {
		return priorReviewOutput{}, err
	}
	out := priorReviewOutput{Reviews: []priorReviewRow{}}
	for _, review := range h.reviews {
		if in.JobID > 0 {
			if review.Job.ID != in.JobID {
				continue
			}
		} else if !strings.HasPrefix(strings.ToLower(endRef(review.Job.GitRef)), commit) {
			continue
		}
		out.Reviews = append(out.Reviews, priorReviewRow{
			JobID:      review.Job.ID,
			Commit:     shortRef(review.Job.GitRef),
			Subject:    review.Job.CommitSubject,
			ReviewType: reviewTypeLabel(review.Job.ReviewType),
			Verdict:    verdictLabel(review.Job.Verdict),
			Closed:     review.Review.Closed,
			Output:     review.Review.Output,
			Comments:   reviewerComments(review.Comments),
		})
	}
	if len(out.Reviews) == 0 {
		return priorReviewOutput{}, NewError(ErrorCodeNotFound,
			"no finished review on this branch matches")
	}
	return out, nil
}

func (r *Reviewer) gitLog(ctx context.Context, scope ReviewerScope, in gitLogInput) (gitLogOutput, error) {
	file, err := repoRelativePath(in.Path)
	if err != nil {
		return gitLogOutput{}, err
	}
	start, end := in.StartLine, in.EndLine
	if start != 0 || end != 0 {
		if start, end, err = lineRange(start, end); err != nil {
			return gitLogOutput{}, err
		}
	}
	count := in.MaxCount
	if count <= 0 {
		count = defaultGitLogCount
	}
	h, err := r.history(ctx, scope)
	if err != nil {
		return gitLogOutput{}, err
	}
	commits, err := gitpkg.PathLog(ctx, scope.RepoPath, scope.SHA, file, start, end, count)
	if err != nil {
		return gitLogOutput{}, NewError(ErrorCodeInvalidArgument, "path or line range not found at the reviewed commit")
	}
	out := gitLogOutput{Commits: make([]historyCommit, 0, len(commits))}
	for _, commit := range commits {
		out.Commits = append(out.Commits, h.describe(commit))
	}
	return out, nil
}

func (r *Reviewer) gitBlame(ctx context.Context, scope ReviewerScope, in gitBlameInput) (gitBlameOutput, error) {
	file, err := repoRelativePath(in.Path)
	if err != nil {
		return gitBlameOutput{}, err
	}
	start, end, err := lineRange(in.StartLine, in.EndLine)
	if err != nil {
		return gitBlameOutput{}, err
	}
	h, err := r.history(ctx, scope)
	if err != nil {
		return gitBlameOutput{}, err
	}
	hunks, err := gitpkg.BlameLines(ctx, scope.RepoPath, scope.SHA, file, start, end)
	if err != nil {
		return gitBlameOutput{}, NewError(ErrorCodeInvalidArgument, "path or line range not found at the reviewed commit")
	}
	out := gitBlameOutput{Hunks: make([]blameHunk, 0, len(hunks))}
	for _, hunk := range hunks {
		lines := strconv.Itoa(hunk.StartLine)
		if hunk.EndLine != hunk.StartLine {
			lines += "-" + strconv.Itoa(hunk.EndLine)
		}
		out.Hunks = append(out.Hunks, blameHunk{Lines: lines, historyCommit: h.describe(hunk.Commit)})
	}
	return out, nil
}

// describe annotates a commit with its place in the branch and the earlier
// reviews whose comments cite it.
func (h scopedHistory) describe(commit gitpkg.HistoryCommit) historyCommit {
	out := historyCommit{
		Commit:   shortSHA(commit.SHA),
		Date:     commit.Date,
		Author:   commit.Author,
		Subject:  commit.Subject,
		OnBranch: h.lineage.Matches(commit.SHA),
	}
	sha := strings.ToLower(commit.SHA)
	for _, review := range h.reviews {
		if commentsCite(review.Comments, sha) {
			out.FixFor = append(out.FixFor, review.Job.ID)
		}
	}
	return out
}

// commentsCite reports whether any comment contains a hex word of at least
// seven characters that abbreviates sha.
func commentsCite(comments []storage.Response, sha string) bool {
	for _, comment := range comments {
		words := strings.FieldsFunc(strings.ToLower(comment.Response), func(r rune) bool {
			return (r < '0' || r > '9') && (r < 'a' || r > 'f')
		})
		for _, word := range words {
			if len(word) >= 7 && strings.HasPrefix(sha, word) {
				return true
			}
		}
	}
	return false
}

func decodeReviewDocument(output storage.StructuredOutput) (structuredreview.Document, bool) {
	if len(output) == 0 {
		return structuredreview.Document{}, false
	}
	raw, err := json.Marshal(output)
	if err != nil {
		return structuredreview.Document{}, false
	}
	doc, err := structuredreview.Decode(raw)
	if err != nil || doc.Legacy != nil {
		return structuredreview.Document{}, false
	}
	return doc, true
}

// shortClaim keeps the first sentence of a finding's problem text.
func shortClaim(problem string) string {
	claim, _, _ := strings.Cut(strings.TrimSpace(problem), "\n")
	if i := strings.Index(claim, ". "); i >= 0 {
		claim = claim[:i+1]
	}
	return strings.TrimSpace(claim)
}

func reviewerComments(responses []storage.Response) []reviewerComment {
	comments := make([]reviewerComment, 0, len(responses))
	for _, response := range responses {
		comments = append(comments, reviewerComment{Author: response.Responder, Text: response.Response})
	}
	return comments
}

// repoRelativePath accepts only a clean path inside the repository. Git reads
// the file from the reviewed commit's tree, never from the working tree.
func repoRelativePath(raw string) (string, error) {
	p := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	clean := path.Clean(p)
	if p == "" || strings.ContainsRune(p, 0) || path.IsAbs(clean) || filepath.VolumeName(p) != "" ||
		clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", NewError(ErrorCodeInvalidArgument, "path must be a file path relative to the repository root")
	}
	return clean, nil
}

func lineRange(start, end int) (int, int, error) {
	if end == 0 {
		end = start
	}
	if start < 1 || end < start {
		return 0, 0, NewError(ErrorCodeInvalidArgument, "start_line must be at least 1 and end_line at least start_line")
	}
	return start, end, nil
}

func reviewTypeLabel(reviewType string) string {
	if reviewType == "" {
		return "default"
	}
	return reviewType
}

func endRef(ref string) string {
	if _, end, ok := gitpkg.ParseRange(ref); ok {
		return end
	}
	return ref
}

func shortRef(ref string) string {
	if start, end, ok := gitpkg.ParseRange(ref); ok {
		return shortSHA(start) + ".." + shortSHA(end)
	}
	return shortSHA(ref)
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
