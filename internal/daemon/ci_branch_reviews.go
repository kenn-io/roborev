package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"

	googlegithub "github.com/google/go-github/v93/github"

	"go.kenn.io/roborev/internal/config"
	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/procutil"
	"go.kenn.io/roborev/internal/storage"
)

// ciBranchStatusContext keeps default-branch results apart from the pull
// request status, which can share a commit after a fast-forward merge.
const ciBranchStatusContext = "roborev/default-branch"

// pollDefaultBranch reviews commits that landed on the repository's default
// branch since the previous poll. The first poll records the current head
// without reviewing history, so enabling the feature does not review a
// repository's past.
func (p *CIPoller) pollDefaultBranch(ctx context.Context, ghRepo string, cfg *config.Config) error {
	// Results can finish after the feature is turned off; still publish them.
	p.reconcileBranchReviews(ctx, ghRepo)
	if !cfg.CI.ReviewDefaultBranch {
		// Forget the review position, so turning reviews back on starts from
		// the branch head instead of reviewing what landed in between.
		return p.db.DeleteCIBranchCursor(ctx, ghRepo)
	}
	p.expireTimedOutBranchReviews(ctx, ghRepo, cfg)
	repo, err := p.findOrCloneRepo(ctx, ghRepo)
	if err != nil {
		return fmt.Errorf("find local repo for %s: %w", ghRepo, err)
	}
	if err := p.callGitFetch(ctx, ghRepo, repo.RootPath); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}

	branch, err := p.remoteDefaultBranch(ctx, ghRepo, repo.RootPath)
	if err != nil {
		return fmt.Errorf("detect default branch: %w", err)
	}
	head, err := gitText(ctx, repo.RootPath, "rev-parse", "--verify", "origin/"+branch+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve origin/%s: %w", branch, err)
	}

	scopedCfg := cfg.ForRepo(repo.RootPath)
	repoConfig, err := p.loadCIRepoConfigFor(repo.RootPath, ghRepo)
	configErr := err
	if err != nil && !config.IsExperimentConfigError(err) && !config.IsConfigValidationError(err) {
		return err
	}

	cursor, err := p.db.GetCIBranchCursor(ctx, ghRepo)
	if err != nil {
		return err
	}
	if cursor == nil || cursor.Branch != branch {
		log.Printf("CI poller: starting %s default-branch reviews after %s@%s", ghRepo, branch, gitpkg.ShortSHA(head))
		return p.db.SetCIBranchCursor(ctx, ghRepo, branch, head)
	}
	if cursor.HeadSHA == head {
		return nil
	}
	if q := p.quietHours; q != nil && q.Active(p.nowFn()) {
		return nil
	}
	landed, onBranch, err := landedCommits(ctx, repo.RootPath, cursor.HeadSHA, head)
	if err != nil {
		return fmt.Errorf("list landed commits: %w", err)
	}
	if !onBranch {
		// The branch was rewritten, the last reviewed commit is reachable only
		// through a merged branch, or the clone no longer has it. Later
		// commits cannot be told apart from reviewed ones, so continue from
		// the new head.
		log.Printf("CI poller: %s %s no longer follows %s; continuing default-branch reviews after %s",
			ghRepo, branch, gitpkg.ShortSHA(cursor.HeadSHA), gitpkg.ShortSHA(head))
		return p.db.SetCIBranchCursor(ctx, ghRepo, branch, head)
	}
	for _, commit := range landed {
		if commitOptedOut(repo.RootPath, commit.sha, scopedCfg, repoConfig) {
			if err := p.db.SetCIBranchCursor(ctx, ghRepo, branch, commit.sha); err != nil {
				return err
			}
			continue
		}
		if configErr != nil {
			if err := p.db.RecordCIBranchCommitOutcome(ctx, ghRepo, branch, commit.sha, "error",
				"Review could not be queued; run roborev config validate"); err != nil {
				return err
			}
			p.deliverPendingBranchStatuses(ctx, ghRepo)
			continue
		}
		if err := p.enqueueBranchCommit(ctx, scopedCfg, repoConfig, repo, ghRepo, branch, commit.parent, commit.sha); err != nil {
			return fmt.Errorf("review %s@%s: %w", branch, gitpkg.ShortSHA(commit.sha), err)
		}
	}
	return nil
}

type landedCommit struct {
	sha, parent string
}

// landedCommits lists the first-parent commits from cursor to head, oldest
// first, each with its first parent. onBranch is false when cursor is not on
// head's first-parent chain: it was rewritten away, is reachable only through
// a merged branch, or is missing from the clone.
func landedCommits(ctx context.Context, repoPath, cursor, head string) ([]landedCommit, bool, error) {
	// With --quiet, rev-parse exits 1 for a commit the clone does not have.
	// Other failures, such as an unreadable repository, exit 128.
	if _, err := gitText(ctx, repoPath, "rev-parse", "--verify", "--quiet", cursor+"^{commit}"); err != nil {
		if ctx.Err() == nil && isGitExitCode(err, 1) {
			return nil, false, nil
		}
		return nil, false, err
	}
	out, err := gitText(ctx, repoPath, "log", "--first-parent", "--reverse", "--format=%H %P", cursor+".."+head)
	if err != nil {
		return nil, false, err
	}
	var commits []landedCommit
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		commit := landedCommit{sha: fields[0]}
		if len(fields) > 1 {
			commit.parent = fields[1]
		}
		commits = append(commits, commit)
	}
	if len(commits) == 0 || commits[0].parent != cursor {
		return nil, false, nil
	}
	return commits, true, nil
}

// commitOptedOut reports whether the .roborev.toml in a landed commit turns
// default-branch reviews off, so a commit that lands while a repo has opted
// out is skipped even when the opt-out was reverted before the next poll. A
// commit without the file follows loaded, the configuration the poll uses,
// when it came from the poller's checkout because the branch commits none. A
// file that cannot be read leaves the decision to the global config.
func commitOptedOut(repoPath, sha string, cfg *config.Config, loaded ciRepoConfigSource) bool {
	repoCfg, err := config.LoadRepoConfigFromRef(repoPath, sha)
	if err != nil {
		return false
	}
	if repoCfg == nil && loaded.Ref == "" {
		repoCfg = loaded.Config
	}
	return !config.ResolveCIReviewDefaultBranch(repoCfg, cfg)
}

// enqueueBranchCommit reviews one landed commit against its first parent, so
// a merge commit is reviewed as the change it brought to the branch.
func (p *CIPoller) enqueueBranchCommit(
	ctx context.Context, cfg *config.Config, repoConfig ciRepoConfigSource,
	repo *storage.Repo, ghRepo, branch, parent, sha string,
) error {
	plan, err := p.planCIPanel(ctx, cfg, repoConfig, ciPanelTarget{
		repo: repo, ghRepo: ghRepo, base: parent, head: sha,
		branch: branch, baseBranch: branch,
		// Landed commits share one branch name. Assigning each commit on its
		// own splits them between arms by the experiment's ratio.
		experimentSubject: sha,
	})
	if err != nil {
		if _, ok := errors.AsType[*ciConfigurationError](err); ok {
			if recordErr := p.db.RecordCIBranchCommitOutcome(ctx, ghRepo, branch, sha, "error",
				"Review could not be queued; run roborev config validate"); recordErr != nil {
				return recordErr
			}
			p.deliverPendingBranchStatuses(ctx, ghRepo)
			return nil
		}
		return err
	}
	if len(plan.memberOpts) == 0 {
		return p.db.SetCIBranchCursor(ctx, ghRepo, branch, sha)
	}
	existing, err := p.db.GetCIBranchReview(ctx, ghRepo, sha)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existing == nil {
		// Post pending before the jobs exist. A fast review could otherwise
		// publish its result first and be overwritten by pending.
		err := p.callSetBranchStatus(ctx, ghRepo, sha, "pending", "Review in progress")
		if err != nil && !errors.Is(err, errBranchStatusNoToken) {
			log.Printf("CI poller: failed to set pending default-branch status for %s@%s: %v",
				ghRepo, gitpkg.ShortSHA(sha), err)
		}
	}
	created, synth, err := p.db.CreateCIBranchReviewRun(ctx, ghRepo, branch, sha, plan.memberOpts, plan.synthOpts)
	if err != nil {
		return fmt.Errorf("create default-branch panel run: %w", err)
	}
	if !created {
		return nil
	}
	p.broadcastJobEvent("job.enqueued", synth)
	log.Printf("CI poller: created default-branch panel run for %s@%s (%s, %d members)",
		ghRepo, gitpkg.ShortSHA(sha), branch, len(plan.memberOpts))
	return nil
}

// expireTimedOutBranchReviews applies ci.batch_timeout to default-branch
// panel runs, as expireTimedOutPanels does for pull requests.
func (p *CIPoller) expireTimedOutBranchReviews(ctx context.Context, ghRepo string, cfg *config.Config) {
	timeout := cfg.CI.ResolvedBatchTimeout()
	if timeout <= 0 {
		return
	}
	runs, err := p.db.GetTimedOutCIBranchReviewRuns(ctx, ghRepo, timeout)
	if err != nil {
		log.Printf("CI poller: error listing timed-out default-branch reviews for %s: %v", ghRepo, err)
		return
	}
	expired := 0
	for _, run := range runs {
		if p.expireTimedOutRun(run, timeout) {
			expired++
		}
	}
	if expired > 0 {
		log.Printf("CI poller: expired hung members in %d timed-out default-branch review(s) for %s", expired, ghRepo)
	}
}

// routeBranchReviewEvent records and publishes the result when jobID is the
// synthesis job of a default-branch review. Other jobs are ignored.
func (p *CIPoller) routeBranchReviewEvent(jobID int64) {
	// The event listener carries no context yet, so this starts its own, as
	// routePanelEvent does.
	ctx := context.Background()
	review, err := p.db.GetCIBranchReviewBySynthesisJobID(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		log.Printf("CI poller: error checking default-branch review for job %d: %v", jobID, err)
		return
	}
	p.finishBranchReview(ctx, *review)
}

// reconcileBranchReviews recovers results whose completion events were lost
// and retries status delivery that previously failed.
func (p *CIPoller) reconcileBranchReviews(ctx context.Context, ghRepo string) {
	reviews, err := p.db.ListCIBranchReviewsAwaitingResult(ctx, ghRepo)
	if err != nil {
		log.Printf("CI poller: error listing default-branch results for %s: %v", ghRepo, err)
	}
	for _, review := range reviews {
		p.finishBranchReview(ctx, review)
	}
	p.deliverPendingBranchStatuses(ctx, ghRepo)
}

func (p *CIPoller) finishBranchReview(ctx context.Context, review storage.CIBranchReview) {
	if review.SynthesisJobID == nil || review.PanelRunUUID == nil {
		return
	}
	synth, err := p.db.GetJobByIDContext(ctx, *review.SynthesisJobID)
	if err != nil {
		log.Printf("CI poller: error loading default-branch synthesis job %d: %v", *review.SynthesisJobID, err)
		return
	}
	var state, desc string
	switch synth.Status {
	case storage.JobStatusDone:
		members, err := p.db.GetPanelMemberReviews(*review.PanelRunUUID)
		if err != nil {
			log.Printf("CI poller: error loading default-branch member reviews for %s@%s: %v",
				review.GithubRepo, gitpkg.ShortSHA(review.SHA), err)
			return
		}
		result, err := p.db.GetReviewByJobID(synth.ID)
		if err != nil {
			// Leave the result unrecorded so the next poll retries instead of
			// publishing a verdict that was never read.
			log.Printf("CI poller: error loading default-branch review for %s@%s: %v",
				review.GithubRepo, gitpkg.ShortSHA(review.SHA), err)
			return
		}
		state, desc = branchCommitStatus(members, result.VerdictBool != nil && *result.VerdictBool == 0)
	case storage.JobStatusFailed:
		state, desc = "error", "Review failed"
	case storage.JobStatusCanceled:
		state, desc = "error", "Review canceled"
	default:
		return
	}
	if err := p.db.SetCIBranchReviewFinalStatus(ctx, review, state, desc); err != nil {
		log.Printf("CI poller: error recording default-branch result for %s@%s: %v",
			review.GithubRepo, gitpkg.ShortSHA(review.SHA), err)
		return
	}
	p.deliverPendingBranchStatuses(ctx, review.GithubRepo)
}

// branchCommitStatus reports review execution in the state, as pull request
// statuses do, and the verdict in the description. Findings do not mark the
// landed commit failed; they are followed up from the stored review.
func branchCommitStatus(members []storage.BatchReviewResult, foundIssues bool) (state, desc string) {
	state, execution := panelCommitStatus(members)
	desc = "No issues found"
	if foundIssues {
		desc = "Issues found"
	}
	return state, desc + strings.TrimPrefix(execution, "Review complete")
}

// deliverPendingBranchStatuses publishes recorded results. A failed send is
// retried on the next poll, unless GitHub will never accept the status. That
// result stays in the database, and its commit gets no status.
func (p *CIPoller) deliverPendingBranchStatuses(ctx context.Context, ghRepo string) {
	reviews, err := p.db.ListCIBranchReviewStatuses(ctx, ghRepo)
	if err != nil {
		log.Printf("CI poller: error listing default-branch statuses for %s: %v", ghRepo, err)
		return
	}
	for _, review := range reviews {
		won, err := p.db.ClaimCIBranchReviewStatus(ctx, review, panelPostingStaleWindow)
		if err != nil {
			log.Printf("CI poller: error claiming default-branch status for %s@%s: %v",
				ghRepo, gitpkg.ShortSHA(review.SHA), err)
			continue
		}
		if !won {
			continue
		}
		sendErr := p.callSetBranchStatus(ctx, ghRepo, review.SHA, review.FinalState, review.FinalDescription)
		refused := branchStatusRefused(sendErr)
		switch {
		case refused:
			log.Printf("CI poller: default-branch status for %s@%s will not be published: %v",
				ghRepo, gitpkg.ShortSHA(review.SHA), sendErr)
		case sendErr != nil:
			log.Printf("CI poller: failed to set default-branch status for %s@%s: %v",
				ghRepo, gitpkg.ShortSHA(review.SHA), sendErr)
		}
		// The send already happened. Record its outcome even when the poller
		// is stopping, so a restart does not publish the status again.
		if err := p.db.FinishCIBranchReviewStatus(context.WithoutCancel(ctx), review, sendErr == nil || refused); err != nil {
			log.Printf("CI poller: error acknowledging default-branch status for %s@%s: %v",
				ghRepo, gitpkg.ShortSHA(review.SHA), err)
		}
	}
}

// errBranchStatusNoToken means the daemon has no GitHub credentials. Reviews
// still run and are stored; only their commit statuses are skipped.
var errBranchStatusNoToken = errors.New("GitHub token required to publish commit status")

// branchStatusRefused reports whether GitHub will never accept a commit status
// from this daemon, so sending it again cannot succeed.
func branchStatusRefused(err error) bool {
	if errors.Is(err, errBranchStatusNoToken) || isPermanentGitHubAccessError(err) {
		return true
	}
	// GitHub answers 422 for a commit it no longer has, such as one removed by
	// a force push, and for a commit that already holds its maximum number of
	// statuses for one context.
	githubErr, ok := errors.AsType[*googlegithub.ErrorResponse](err)
	return ok && githubErr.Response != nil && githubErr.Response.StatusCode == http.StatusUnprocessableEntity
}

func (p *CIPoller) callSetBranchStatus(ctx context.Context, ghRepo, sha, state, description string) error {
	if p.setBranchStatusFn != nil {
		return p.setBranchStatusFn(ghRepo, sha, state, description)
	}
	if strings.TrimSpace(p.githubTokenForRepo(ghRepo)) == "" {
		return errBranchStatusNoToken
	}
	client, err := p.githubClientForRepo(ghRepo)
	if err != nil {
		return err
	}
	return client.SetNamedCommitStatus(ctx, ghRepo, sha, ciBranchStatusContext, state, description)
}

// remoteDefaultBranch asks the remote which branch is its default and returns
// that branch's name. A local branch in the poller's clone does not track what
// landed on GitHub, so only a fetched remote branch is reviewed.
//
// Git writes origin/HEAD when it clones, and fetch does not move it.
// Without asking on each poll, a default branch that is renamed or replaced on
// GitHub would never be followed. Pull request reviews read their repository
// configuration through origin/HEAD too, so they follow the change as well.
func (p *CIPoller) remoteDefaultBranch(ctx context.Context, ghRepo, repoPath string) (string, error) {
	unlock := lockGitMetadata(repoPath)
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "remote", "set-head", "origin", "--auto")
	procutil.HideConsole(cmd)
	if env := p.gitEnvForRepo(ghRepo); env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	unlock()
	if err != nil {
		return "", fmt.Errorf("update origin/HEAD: %w: %s", err, strings.TrimSpace(string(out)))
	}
	ref, err := gitpkg.GetDefaultBranch(repoPath)
	if err != nil {
		return "", err
	}
	branch, ok := strings.CutPrefix(ref, "origin/")
	if !ok {
		return "", errors.New("origin/HEAD does not name a fetched branch")
	}
	return branch, nil
}

func gitText(ctx context.Context, repoPath string, args ...string) (string, error) {
	out, err := gitOutput(ctx, repoPath, args...)
	return strings.TrimSpace(string(out)), err
}
