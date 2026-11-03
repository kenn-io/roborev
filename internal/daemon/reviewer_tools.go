package daemon

import (
	"context"
	"crypto/rand"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/mcpserver"
	"go.kenn.io/roborev/internal/rebuttal"
	"go.kenn.io/roborev/internal/storage"
)

// reviewerGrants holds the bearer tokens for the reviewer MCP endpoint. Each
// token belongs to one running review attempt and lives only in memory.
type reviewerGrants struct {
	mu     sync.Mutex
	grants map[string]reviewerGrant
}

type reviewerGrant struct {
	scope   mcpserver.ReviewerScope
	expires time.Time
}

func newReviewerGrants() *reviewerGrants {
	return &reviewerGrants{grants: make(map[string]reviewerGrant)}
}

// issue mints a token for scope that expires at expires. The caller must call
// revoke when the attempt ends.
func (g *reviewerGrants) issue(scope mcpserver.ReviewerScope, expires time.Time) (token string, revoke func()) {
	token = rand.Text()
	g.mu.Lock()
	g.grants[token] = reviewerGrant{scope: scope, expires: expires}
	g.mu.Unlock()
	return token, func() {
		g.mu.Lock()
		delete(g.grants, token)
		g.mu.Unlock()
	}
}

// verify resolves a live token. Expired tokens are dropped.
func (g *reviewerGrants) verify(token string) (mcpserver.ReviewerScope, time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	grant, ok := g.grants[token]
	if !ok {
		return mcpserver.ReviewerScope{}, time.Time{}, false
	}
	if !time.Now().Before(grant.expires) {
		delete(g.grants, token)
		return mcpserver.ReviewerScope{}, time.Time{}, false
	}
	return grant.scope, grant.expires, true
}

// withReviewerEndpoint routes the reviewer MCP path to its own handler, which
// authenticates with reviewer tokens instead of the daemon API credentials.
func withReviewerEndpoint(api, reviewer http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == mcpserver.ReviewerHTTPPath {
			reviewer.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
}

// reviewerURL is the reviewer endpoint URL agents dial, or "" when agents
// cannot reach it: the API listener is not plain TCP.
func (s *Server) reviewerURL() string {
	s.endpointMu.Lock()
	ep := s.endpoint
	s.endpointMu.Unlock()
	if ep.Network != "tcp" || ep.Address == "" || s.tcpTLS {
		return ""
	}
	return ep.BaseURL() + mcpserver.ReviewerHTTPPath
}

// reviewerToolsEligible reports whether a job may get reviewer tools: a local
// review of one commit on a named branch whose prompt roborev builds itself.
// CI and other prebuilt prompts carry untrusted context and are excluded.
func reviewerToolsEligible(job *storage.ReviewJob) bool {
	return job.JobType == storage.JobTypeReview && job.Branch != "" && job.RepoID > 0 &&
		!job.PromptPrebuilt && job.Source != storage.JobSourceRebuttal && !job.IsSynthesisJob()
}

// reviewerToolsFor issues reviewer tools for one attempt when they are
// enabled, the job is eligible, and the agent can use them.
func (wp *WorkerPool) reviewerToolsFor(
	job *storage.ReviewJob, a agent.Agent, cfg *config.Config, expires time.Time,
) (agent.ReviewerTools, func(), bool) {
	if wp.reviewerGrants == nil || wp.reviewerURL == nil || !cfg.MCP.ReviewerTools || !reviewerToolsEligible(job) {
		return agent.ReviewerTools{}, nil, false
	}
	if _, ok := a.(agent.ReviewerToolsAgent); !ok {
		return agent.ReviewerTools{}, nil, false
	}
	url := wp.reviewerURL()
	if url == "" {
		return agent.ReviewerTools{}, nil, false
	}
	token, revoke := wp.reviewerGrants.issue(mcpserver.ReviewerScope{
		JobID: job.ID, RepoID: job.RepoID, RepoPath: job.RepoPath, Branch: job.Branch, SHA: job.GitRef,
	}, expires)
	return agent.ReviewerTools{URL: url, Token: token, ToolNames: mcpserver.ReviewerToolNames}, revoke, true
}

// reviewerBackend reads branch review history for the reviewer tools.
type reviewerBackend struct {
	db *storage.DB
}

func (b reviewerBackend) BranchReviews(
	ctx context.Context, repoID int64, branch string,
) ([]mcpserver.BranchReview, error) {
	jobs, err := b.db.ListJobs(string(storage.JobStatusDone), "", 0, 0,
		storage.WithRepoIDs([]int64{repoID}), storage.WithBranch(branch),
		storage.WithExcludePanelRole(storage.PanelRoleMember), storage.WithoutPrompt(),
	)
	if err != nil {
		return nil, mcpserver.NewError(mcpserver.ErrorCodeInternal, "list branch reviews failed")
	}
	reviews := make([]mcpserver.BranchReview, 0, len(jobs))
	// Rebuttal re-checks are task jobs on the same branch that name the
	// review they re-check as their parent.
	assessments := make(map[int64][]mcpserver.FindingAssessment)
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if job.Source == storage.JobSourceRebuttal && job.ParentJobID != nil {
			if a, ok := b.assessment(job.ID); ok {
				assessments[*job.ParentJobID] = append(assessments[*job.ParentJobID], a)
			}
			continue
		}
		// A panel's canonical review is its synthesis job; members are
		// excluded by the query.
		if job.JobType != storage.JobTypeReview && job.JobType != storage.JobTypeRange &&
			job.JobType != storage.JobTypeSynthesis {
			continue
		}
		review, err := b.db.GetReviewByJobID(job.ID)
		if err != nil {
			continue
		}
		comments, err := b.comments(job)
		if err != nil {
			return nil, mcpserver.NewError(mcpserver.ErrorCodeInternal, "load review comments failed")
		}
		reviews = append(reviews, mcpserver.BranchReview{
			Job: job, Review: *review, Comments: storage.PromptTrustedResponses(comments),
		})
	}
	for i := range reviews {
		// Jobs are newest first; show each review's re-checks oldest first.
		found := assessments[reviews[i].Job.ID]
		slices.Reverse(found)
		reviews[i].Assessments = found
	}
	return reviews, nil
}

// rebuttalFindingPrefix opens the prompt line that quotes the finding a
// rebuttal re-checks; humaRebuttal writes it with %q.
const rebuttalFindingPrefix = "Original finding: "

// assessment reads one finished rebuttal job: the finding excerpt from its
// prompt and the outcome from its output. Unreadable jobs are skipped.
func (b reviewerBackend) assessment(jobID int64) (mcpserver.FindingAssessment, bool) {
	job, err := b.db.GetJobByID(jobID)
	if err != nil {
		return mcpserver.FindingAssessment{}, false
	}
	var excerpt string
	for line := range strings.Lines(job.Prompt) {
		if quoted, ok := strings.CutPrefix(strings.TrimSpace(line), rebuttalFindingPrefix); ok {
			excerpt, _ = strconv.Unquote(quoted)
			break
		}
	}
	review, err := b.db.GetReviewByJobID(jobID)
	if err != nil {
		return mcpserver.FindingAssessment{}, false
	}
	result, err := rebuttal.Parse(review.Output)
	if err != nil {
		return mcpserver.FindingAssessment{}, false
	}
	return mcpserver.FindingAssessment{Finding: excerpt, Outcome: result.Outcome, Explanation: result.Explanation}, true
}

// comments returns the decisions recorded on a review: those on the job and,
// through `roborev job comment <sha>`, those on its commit. LegacyCommentLookupTarget
// leaves out synthesis jobs, but a local panel review of one commit is that
// commit's canonical review, so its commit comments belong to it too.
func (b reviewerBackend) comments(job storage.ReviewJob) ([]storage.Response, error) {
	commitID, fallbackSHA := job.LegacyCommentLookupTarget()
	if job.IsSynthesisJob() && job.CommitID != nil && !gitpkg.IsRange(job.GitRef) {
		commitID = *job.CommitID
	}
	return b.db.GetAllCommentsForJob(job.ID, commitID, fallbackSHA)
}
