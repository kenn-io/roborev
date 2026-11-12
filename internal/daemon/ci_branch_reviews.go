package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"go.kenn.io/roborev/internal/config"
	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
)

// ciBranchStatusContext keeps default-branch results apart from the pull
// request status, which can share a commit after a fast-forward merge.
const ciBranchStatusContext = "roborev/default-branch"

// ciBranchCommitsPerPoll bounds the reviews one poll enqueues for a
// repository. Later commits wait for the next poll.
const ciBranchCommitsPerPoll = 20

// pollDefaultBranch reviews commits that landed on the repository's default
// branch since the previous poll. The first poll records the current head
// without reviewing history, so enabling the feature does not review a
// repository's past.
func (p *CIPoller) pollDefaultBranch(ctx context.Context, ghRepo string, cfg *config.Config) error {
	// Results can finish after the feature is turned off; still publish them.
	p.reconcileBranchReviews(ghRepo)
	if !cfg.CI.ReviewDefaultBranch {
		// Forget the review position, so turning reviews back on starts from
		// the branch head instead of reviewing what landed in between.
		return p.db.DeleteCIBranchCursor(ghRepo)
	}
	p.expireTimedOutBranchReviews(ghRepo, cfg)
	repo, err := p.findOrCloneRepo(ctx, ghRepo)
	if err != nil {
		return fmt.Errorf("find local repo for %s: %w", ghRepo, err)
	}
	if err := p.callGitFetch(ctx, ghRepo, repo.RootPath); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}

	defaultRef, err := gitpkg.GetDefaultBranch(repo.RootPath)
	if err != nil {
		return fmt.Errorf("detect default branch: %w", err)
	}
	// A local branch in the poller's clone does not track what landed on
	// GitHub, so only the fetched remote default branch is reviewed.
	branch, ok := strings.CutPrefix(defaultRef, "origin/")
	if !ok {
		return fmt.Errorf("detect default branch: origin/HEAD does not name a fetched branch")
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

	cursor, err := p.db.GetCIBranchCursor(ghRepo)
	if err != nil {
		return err
	}
	if cursor == nil || cursor.Branch != branch {
		log.Printf("CI poller: starting %s default-branch reviews after %s@%s", ghRepo, branch, gitpkg.ShortSHA(head))
		return p.db.SetCIBranchCursor(ghRepo, branch, head)
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
		// The branch was rewritten, or the last reviewed commit is reachable
		// only through a merged branch. Later commits cannot be told apart
		// from reviewed ones, so continue from the new head.
		log.Printf("CI poller: %s %s no longer follows %s; continuing default-branch reviews after %s",
			ghRepo, branch, gitpkg.ShortSHA(cursor.HeadSHA), gitpkg.ShortSHA(head))
		return p.db.SetCIBranchCursor(ghRepo, branch, head)
	}
	queued := 0
	for _, commit := range landed {
		if queued == ciBranchCommitsPerPoll {
			break
		}
		if commitOptedOut(repo.RootPath, commit.sha, scopedCfg) {
			if err := p.db.SetCIBranchCursor(ghRepo, branch, commit.sha); err != nil {
				return err
			}
			continue
		}
		queued++
		if configErr != nil {
			if err := p.db.RecordCIBranchCommitOutcome(ghRepo, branch, commit.sha, "error",
				"Review could not be queued; run roborev config validate"); err != nil {
				return err
			}
			p.deliverPendingBranchStatuses(ghRepo)
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
// head's first-parent chain: it was rewritten away or is reachable only
// through a merged branch.
func landedCommits(ctx context.Context, repoPath, cursor, head string) ([]landedCommit, bool, error) {
	if _, err := gitText(ctx, repoPath, "cat-file", "-e", cursor+"^{commit}"); err != nil {
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
// file that cannot be read leaves the decision to the default-branch config.
func commitOptedOut(repoPath, sha string, cfg *config.Config) bool {
	repoCfg, err := config.LoadRepoConfigFromRef(repoPath, sha)
	return err == nil && !config.ResolveCIReviewDefaultBranch(repoCfg, cfg)
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
	})
	if err != nil {
		if _, ok := errors.AsType[*ciConfigurationError](err); ok {
			if recordErr := p.db.RecordCIBranchCommitOutcome(ghRepo, branch, sha, "error",
				"Review could not be queued; run roborev config validate"); recordErr != nil {
				return recordErr
			}
			p.deliverPendingBranchStatuses(ghRepo)
			return nil
		}
		return err
	}
	if len(plan.memberOpts) == 0 {
		return p.db.SetCIBranchCursor(ghRepo, branch, sha)
	}
	existing, err := p.db.GetCIBranchReview(ghRepo, sha)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existing == nil {
		// Post pending before the jobs exist. A fast review could otherwise
		// publish its result first and be overwritten by pending.
		if err := p.callSetBranchStatus(ghRepo, sha, "pending", "Review in progress"); err != nil {
			log.Printf("CI poller: failed to set pending default-branch status for %s@%s: %v",
				ghRepo, gitpkg.ShortSHA(sha), err)
		}
	}
	created, synth, err := p.db.CreateCIBranchReviewRun(ghRepo, branch, sha, plan.memberOpts, plan.synthOpts)
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
func (p *CIPoller) expireTimedOutBranchReviews(ghRepo string, cfg *config.Config) {
	timeout := cfg.CI.ResolvedBatchTimeout()
	if timeout <= 0 {
		return
	}
	runs, err := p.db.GetTimedOutCIBranchReviewRuns(ghRepo, timeout)
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
	review, err := p.db.GetCIBranchReviewBySynthesisJobID(jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		log.Printf("CI poller: error checking default-branch review for job %d: %v", jobID, err)
		return
	}
	p.finishBranchReview(*review)
}

// reconcileBranchReviews recovers results whose completion events were lost
// and retries status delivery that previously failed.
func (p *CIPoller) reconcileBranchReviews(ghRepo string) {
	reviews, err := p.db.ListCIBranchReviewsAwaitingResult(ghRepo)
	if err != nil {
		log.Printf("CI poller: error listing default-branch results for %s: %v", ghRepo, err)
	}
	for _, review := range reviews {
		p.finishBranchReview(review)
	}
	p.deliverPendingBranchStatuses(ghRepo)
}

func (p *CIPoller) finishBranchReview(review storage.CIBranchReview) {
	if review.SynthesisJobID == nil || review.PanelRunUUID == nil {
		return
	}
	synth, err := p.db.GetJobByID(*review.SynthesisJobID)
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
	if err := p.db.SetCIBranchReviewFinalStatus(review.ID, state, desc); err != nil {
		log.Printf("CI poller: error recording default-branch result for %s@%s: %v",
			review.GithubRepo, gitpkg.ShortSHA(review.SHA), err)
		return
	}
	p.deliverPendingBranchStatuses(review.GithubRepo)
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

func (p *CIPoller) deliverPendingBranchStatuses(ghRepo string) {
	reviews, err := p.db.ListCIBranchReviewStatuses(ghRepo)
	if err != nil {
		log.Printf("CI poller: error listing default-branch statuses for %s: %v", ghRepo, err)
		return
	}
	for _, review := range reviews {
		won, err := p.db.ClaimCIBranchReviewStatus(review.ID, panelPostingStaleWindow)
		if err != nil {
			log.Printf("CI poller: error claiming default-branch status for %s@%s: %v",
				ghRepo, gitpkg.ShortSHA(review.SHA), err)
			continue
		}
		if !won {
			continue
		}
		sendErr := p.callSetBranchStatus(ghRepo, review.SHA, review.FinalState, review.FinalDescription)
		if sendErr != nil {
			log.Printf("CI poller: failed to set default-branch status for %s@%s: %v",
				ghRepo, gitpkg.ShortSHA(review.SHA), sendErr)
		}
		if err := p.db.FinishCIBranchReviewStatus(review.ID, sendErr == nil); err != nil {
			log.Printf("CI poller: error acknowledging default-branch status for %s@%s: %v",
				ghRepo, gitpkg.ShortSHA(review.SHA), err)
		}
	}
}

func (p *CIPoller) callSetBranchStatus(ghRepo, sha, state, description string) error {
	if p.setBranchStatusFn != nil {
		return p.setBranchStatusFn(ghRepo, sha, state, description)
	}
	client, err := p.githubClientForRepo(ghRepo)
	if err != nil {
		return err
	}
	return client.SetCommitStatusContext(context.Background(), ghRepo, sha, ciBranchStatusContext, state, description)
}

func gitText(ctx context.Context, repoPath string, args ...string) (string, error) {
	out, err := gitOutput(ctx, repoPath, args...)
	return strings.TrimSpace(string(out)), err
}
