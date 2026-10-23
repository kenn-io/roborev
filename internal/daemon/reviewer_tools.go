package daemon

import (
	"context"
	"crypto/rand"
	"net/http"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/mcpserver"
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
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if job.JobType != storage.JobTypeReview && job.JobType != storage.JobTypeRange {
			continue
		}
		review, err := b.db.GetReviewByJobID(job.ID)
		if err != nil {
			continue
		}
		comments, err := b.db.GetCommentsForJob(job.ID)
		if err != nil {
			return nil, mcpserver.NewError(mcpserver.ErrorCodeInternal, "load review comments failed")
		}
		reviews = append(reviews, mcpserver.BranchReview{
			Job: job, Review: *review, Comments: storage.PromptTrustedResponses(comments),
		})
	}
	return reviews, nil
}
