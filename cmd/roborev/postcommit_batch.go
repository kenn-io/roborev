package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"go.kenn.io/roborev/internal/git"
)

const (
	postCommitBatchStateVersion      = 1
	postCommitBatchLockRetryInterval = 10 * time.Millisecond
)

type postCommitBatchState struct {
	Version  int               `json:"version"`
	Branches map[string]string `json:"branches"`
}

type postCommitBatchDecision struct {
	Enabled            bool
	Track              bool
	Ready              bool
	Branch             string
	Head               string
	Checkpoint         string
	Pending            int
	Reason             string
	ClearCheckpoint    bool
	PreserveCheckpoint bool
	statePath          string
	state              postCommitBatchState
}

func (d postCommitBatchDecision) AccumulatedRef() string {
	if d.Head == "" {
		return "HEAD"
	}
	if d.Checkpoint == "" {
		return d.Head
	}
	return d.Checkpoint + ".." + d.Head
}

func newPostCommitBatchState() postCommitBatchState {
	return postCommitBatchState{
		Version:  postCommitBatchStateVersion,
		Branches: make(map[string]string),
	}
}

func postCommitBatchStatePath(repoPath string) (string, error) {
	gitDir, err := git.ResolveGitCommonDir(repoPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(gitDir, "roborev", "post-commit-batches.json"), nil
}

func acquirePostCommitBatchLock(
	ctx context.Context, repoPath string,
) (func() error, error) {
	statePath, err := postCommitBatchStatePath(repoPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	lock := flock.New(filepath.Join(filepath.Dir(statePath), "post-commit.lock"))
	// TryLockContext retries at this interval until it acquires the lock or
	// ctx is canceled. The interval is not a lock timeout.
	locked, err := lock.TryLockContext(ctx, postCommitBatchLockRetryInterval)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, fmt.Errorf("lock not acquired")
	}
	return lock.Unlock, nil
}

func loadPostCommitBatchState(
	path string,
) (postCommitBatchState, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return newPostCommitBatchState(), false, nil
	}
	if err != nil {
		return postCommitBatchState{}, false, err
	}
	defer f.Close()

	var state postCommitBatchState
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return postCommitBatchState{}, true, fmt.Errorf("decode state: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return postCommitBatchState{}, true, fmt.Errorf("decode state: %w", err)
	}
	if state.Version != postCommitBatchStateVersion {
		return postCommitBatchState{}, true, fmt.Errorf(
			"unsupported state version %d", state.Version,
		)
	}
	if state.Branches == nil {
		state.Branches = make(map[string]string)
	}
	return state, true, nil
}

func savePostCommitBatchState(path string, state postCommitBatchState) error {
	if state.Branches == nil {
		state.Branches = make(map[string]string)
	}
	state.Version = postCommitBatchStateVersion
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	f, err := os.CreateTemp(filepath.Dir(path), ".post-commit-batches-*.json")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		f.Close()
		return fmt.Errorf("encode state: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("secure state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

func planPostCommitBatch(
	ctx context.Context,
	root string,
	batchSize int,
) postCommitBatchDecision {
	decision := postCommitBatchDecision{Ready: true}
	statePath, pathErr := postCommitBatchStatePath(root)
	if batchSize < 2 {
		return planDisabledPostCommitBatch(ctx, root, statePath, pathErr, decision)
	}

	decision.Enabled = true
	decision.Track = true
	decision.statePath = statePath
	branch := git.GetCurrentBranch(root)
	head, headErr := git.ResolveSHACtx(ctx, root, "HEAD")
	if pathErr != nil {
		return failOpenPostCommitBatch(branch, head, "resolve state path: "+pathErr.Error())
	}
	if headErr != nil {
		return failOpenPostCommitBatchAt(
			branch, "", statePath, "resolve HEAD: "+headErr.Error(),
		)
	}
	if branch == "" {
		return failOpenPostCommitBatchAt(
			"", head, statePath, "detached HEAD cannot keep branch batch state",
		)
	}

	state, _, err := loadPostCommitBatchState(statePath)
	if err != nil {
		return failOpenPostCommitBatchAt(
			branch, head, statePath, "load batch state: "+err.Error(),
		)
	}
	checkpoint, hasCheckpoint := state.Branches[branch]
	if !hasCheckpoint || checkpoint == "" {
		checkpoint, hasCheckpoint = migrateRenamedPostCommitCheckpoint(
			ctx, root, branch, &state,
		)
		if !hasCheckpoint {
			checkpoint, err = git.ResolveSHACtx(ctx, root, "HEAD^1")
			if err != nil {
				return failOpenPostCommitBatchWithState(
					branch, head, statePath,
					"resolve initial checkpoint: "+err.Error(),
					state,
				)
			}
			state.Branches[branch] = checkpoint
		}
		if err := savePostCommitBatchState(statePath, state); err != nil {
			return failOpenPostCommitBatchWithState(
				branch, head, statePath, "update batch state: "+err.Error(), state,
			)
		}
	}

	pending, onChain, err := git.FirstParentDistance(
		ctx, root, checkpoint, head,
	)
	if err != nil {
		return failOpenPostCommitBatchWithState(
			branch, head, statePath, "count first-parent commits: "+err.Error(),
			state,
		)
	}
	if !onChain {
		// An off-chain checkpoint can be a stale entry left by a deleted
		// branch whose name this branch was renamed onto, hiding the rename
		// migration. Reconcile from rename evidence before merge-base
		// recovery, or the renamed branch's earlier pending commits are
		// permanently skipped while its old entry strands.
		checkpoint, pending, onChain = reconcileOffChainPostCommitCheckpoint(
			ctx, root, branch, head, statePath, checkpoint, &state,
		)
	}
	if !onChain {
		return recoverOffChainPostCommitBatch(
			root, branch, head, checkpoint, statePath,
			"checkpoint is not on HEAD first-parent chain", state,
		)
	}

	decision.Ready = pending >= batchSize
	decision.Branch = branch
	decision.Head = head
	decision.Checkpoint = checkpoint
	decision.Pending = pending
	decision.state = state
	return decision
}

// migrateRenamedPostCommitCheckpoint recovers a checkpoint stranded by a
// branch rename: the old name keeps its state entry but no longer exists as a
// ref, so the renamed branch would re-seed at HEAD^1 and permanently skip the
// pending pre-rename commits. The rename entries git records in the new
// branch's reflog name the prior branches (consecutive renames leave a
// chain), so migration follows that evidence alone: the most recently named
// prior branch with a stored checkpoint wins, every prior name's entry is
// removed, and stale entries for deleted or unrelated branches are never
// adopted. No rename evidence (including a disabled or expired reflog)
// leaves the state untouched.
func migrateRenamedPostCommitCheckpoint(
	ctx context.Context,
	root, branch string,
	state *postCommitBatchState,
) (string, bool) {
	migrated := ""
	for _, source := range git.BranchRenameSources(ctx, root, branch) {
		if source == branch {
			continue
		}
		// A source name that resolves again was recreated as a live branch;
		// its entry now tracks that branch's own batch and must be neither
		// adopted nor deleted. The renamed branch then seeds fresh, the
		// documented fail-open behavior when no usable evidence remains.
		if _, err := git.ResolveSHACtx(
			ctx, root, "refs/heads/"+source,
		); err == nil {
			continue
		}
		if migrated == "" {
			migrated = state.Branches[source]
		}
		delete(state.Branches, source)
	}
	if migrated == "" {
		return "", false
	}
	state.Branches[branch] = migrated
	return migrated, true
}

func planDisabledPostCommitBatch(
	ctx context.Context,
	root, statePath string,
	pathErr error,
	decision postCommitBatchDecision,
) postCommitBatchDecision {
	if pathErr != nil {
		return decision
	}
	if _, err := os.Stat(statePath); err != nil {
		return decision
	}
	state, exists, err := loadPostCommitBatchState(statePath)
	if err != nil || !exists {
		return decision
	}
	branch := git.GetCurrentBranch(root)
	if branch == "" {
		return decision
	}
	head, err := git.ResolveSHACtx(ctx, root, "HEAD")
	if err != nil {
		return decision
	}
	checkpoint, hasCheckpoint := state.Branches[branch]
	if !hasCheckpoint {
		// A rename before batching was disabled leaves the pending range
		// under the old branch name; migrate it so the drain still covers it.
		checkpoint, hasCheckpoint = migrateRenamedPostCommitCheckpoint(
			ctx, root, branch, &state,
		)
		if !hasCheckpoint {
			return decision
		}
		if err := savePostCommitBatchState(statePath, state); err != nil {
			return decision
		}
	}
	decision.Track = true
	decision.ClearCheckpoint = true
	decision.Branch = branch
	decision.Head = head
	decision.statePath = statePath
	decision.state = state
	if checkpoint == "" {
		return decision
	}
	pending, onChain, distanceErr := git.FirstParentDistance(
		ctx, root, checkpoint, head,
	)
	switch {
	case distanceErr != nil:
		// The checkpoint cannot be interpreted right now (for example a
		// transient git failure). Keep it so the next hook run retries the
		// drain instead of silently dropping pending commits.
		decision.PreserveCheckpoint = true
	case !onChain:
		// A rebase or rewrite moved the checkpoint off the first-parent
		// chain. Drain from the last shared commit, same as enabled
		// batching, so rewritten pending commits still get one review.
		mergeBase, err := git.GetMergeBase(root, checkpoint, head)
		var commits []string
		if err == nil {
			commits, err = git.GetRangeCommits(root, mergeBase+".."+head)
		}
		if err != nil {
			decision.PreserveCheckpoint = true
			return decision
		}
		if len(commits) > 0 {
			decision.Enabled = true
			decision.Checkpoint = mergeBase
			decision.Pending = len(commits)
		}
	case pending > 0:
		decision.Enabled = true
		decision.Checkpoint = checkpoint
		decision.Pending = pending
	}
	return decision
}

func planStoredPostCommitBatch(
	ctx context.Context, root string, batchSize int, branch, pushedHead string,
) postCommitBatchDecision {
	decision := postCommitBatchDecision{Enabled: true, Track: true}
	headRef := pushedHead
	if headRef == "" {
		headRef = "refs/heads/" + branch
	}
	head, err := git.ResolveSHACtx(ctx, root, headRef)
	if err != nil {
		return decision
	}
	decision.Branch = branch
	decision.Head = head
	statePath, err := postCommitBatchStatePath(root)
	if err != nil {
		return failOpenPostCommitBatch(
			branch, head, "resolve state path: "+err.Error(),
		)
	}
	decision.statePath = statePath
	state, exists, err := loadPostCommitBatchState(statePath)
	if err != nil {
		return failOpenPostCommitBatchAt(
			branch, head, statePath, "load batch state: "+err.Error(),
		)
	}
	if !exists {
		return decision
	}
	decision.state = state
	checkpoint, hasCheckpoint := state.Branches[branch]
	if !hasCheckpoint || checkpoint == "" {
		checkpoint, hasCheckpoint = migrateRenamedPostCommitCheckpoint(
			ctx, root, branch, &state,
		)
		if !hasCheckpoint {
			return decision
		}
		if err := savePostCommitBatchState(statePath, state); err != nil {
			return failOpenPostCommitBatchWithState(
				branch, head, statePath,
				"update batch state: "+err.Error(), state,
			)
		}
		decision.state = state
	}
	pending, onChain, err := git.FirstParentDistance(ctx, root, checkpoint, head)
	if err != nil {
		return failOpenPostCommitBatchWithState(
			branch, head, statePath,
			"count first-parent commits: "+err.Error(), state,
		)
	}
	if !onChain {
		checkpoint, pending, onChain = reconcileOffChainPostCommitCheckpoint(
			ctx, root, branch, head, statePath, checkpoint, &state,
		)
		decision.state = state
	}
	if !onChain {
		return recoverOffChainPostCommitBatch(
			root, branch, head, checkpoint, statePath,
			"checkpoint is not on branch first-parent chain", state,
		)
	}
	if pending == 0 {
		return decision
	}
	decision.Ready = pending >= batchSize
	decision.Checkpoint = checkpoint
	decision.Pending = pending
	return decision
}

// pushedAncestorFlushBranches returns stored batch branches, mapped to the
// last commit each shares with a pushed head, when that boundary sits past
// the branch's checkpoint. A push carries every unpushed ancestor commit, so
// a branch created from a parent mid-batch (even partway into the pending
// range) pushes those inherited commits although only the child ref is
// updated; the parent's range must flush through the shared boundary, with
// later parent commits staying pending. The state read is lock-free: each
// returned branch is re-planned under the batch lock by its own flush
// invocation, which no-ops when nothing is pending.
func pushedAncestorFlushBranches(
	ctx context.Context, root string, pushed map[string]string,
) map[string]string {
	statePath, err := postCommitBatchStatePath(root)
	if err != nil {
		return nil
	}
	state, exists, err := loadPostCommitBatchState(statePath)
	if err != nil || !exists {
		return nil
	}
	ancestors := make(map[string]string)
	for savedBranch, checkpoint := range state.Branches {
		if checkpoint == "" {
			continue
		}
		if _, isPushed := pushed[savedBranch]; isPushed {
			continue
		}
		tip, err := git.ResolveSHACtx(ctx, root, "refs/heads/"+savedBranch)
		if err != nil {
			continue
		}
		carried := ""
		carriedPending := 0
		for _, head := range pushed {
			boundary, err := git.GetMergeBase(root, tip, head)
			if err != nil || boundary == "" {
				continue
			}
			pending, onChain, err := git.FirstParentDistance(
				ctx, root, checkpoint, boundary,
			)
			if err != nil || !onChain || pending <= carriedPending {
				continue
			}
			carried = boundary
			carriedPending = pending
		}
		if carried != "" {
			ancestors[savedBranch] = carried
		}
	}
	return ancestors
}

// reconcileOffChainPostCommitCheckpoint retries an off-chain checkpoint
// against rename evidence: when the branch was renamed onto a name whose
// stale entry shadowed the migration, the source branch's checkpoint replaces
// it and the pending count is recomputed. It returns the original checkpoint
// with onChain false when there is no rename evidence, nothing changes, or
// the migrated checkpoint is still off-chain — callers then fall back to
// merge-base recovery as before.
func reconcileOffChainPostCommitCheckpoint(
	ctx context.Context,
	root, branch, head, statePath, checkpoint string,
	state *postCommitBatchState,
) (string, int, bool) {
	migrated, ok := migrateRenamedPostCommitCheckpoint(ctx, root, branch, state)
	if !ok || migrated == checkpoint {
		return checkpoint, 0, false
	}
	if err := savePostCommitBatchState(statePath, *state); err != nil {
		return checkpoint, 0, false
	}
	pending, onChain, err := git.FirstParentDistance(ctx, root, migrated, head)
	if err != nil || !onChain {
		return migrated, 0, false
	}
	return migrated, pending, true
}

func failOpenPostCommitBatch(branch, head, reason string) postCommitBatchDecision {
	return failOpenPostCommitBatchAt(branch, head, "", reason)
}

func failOpenPostCommitBatchAt(
	branch, head, statePath, reason string,
) postCommitBatchDecision {
	return postCommitBatchDecision{
		Enabled:   true,
		Track:     true,
		Ready:     true,
		Branch:    branch,
		Head:      head,
		Reason:    reason,
		statePath: statePath,
	}
}

func failOpenPostCommitBatchWithState(
	branch, head, statePath, reason string,
	state postCommitBatchState,
) postCommitBatchDecision {
	decision := failOpenPostCommitBatchAt(branch, head, statePath, reason)
	decision.PreserveCheckpoint = state.Branches[branch] != ""
	decision.state = state
	return decision
}

// recoverOffChainPostCommitBatch handles a checkpoint that is no longer on the
// branch's first-parent chain (typically after a rebase or history rewrite).
// It reviews everything from the last shared commit so pending work is never
// silently skipped, at the cost of possibly re-reviewing rewritten commits.
func recoverOffChainPostCommitBatch(
	root, branch, head, checkpoint, statePath, reason string,
	state postCommitBatchState,
) postCommitBatchDecision {
	mergeBase, err := git.GetMergeBase(root, checkpoint, head)
	if err != nil {
		return failOpenPostCommitBatchWithState(
			branch, head, statePath,
			reason+"; find recovery boundary: "+err.Error(), state,
		)
	}
	commits, err := git.GetRangeCommits(root, mergeBase+".."+head)
	if err != nil || len(commits) == 0 {
		if err == nil {
			err = fmt.Errorf("recovery range contains no commits")
		}
		return failOpenPostCommitBatchWithState(
			branch, head, statePath,
			reason+"; validate recovery range: "+err.Error(), state,
		)
	}
	decision := failOpenPostCommitBatchAt(branch, head, statePath, reason)
	decision.Checkpoint = mergeBase
	decision.Pending = len(commits)
	decision.state = state
	return decision
}

func advancePostCommitBatch(decision postCommitBatchDecision) error {
	if !decision.Track || decision.Branch == "" ||
		decision.Head == "" || decision.statePath == "" {
		return nil
	}
	if decision.PreserveCheckpoint {
		return nil
	}
	state := decision.state
	if state.Branches == nil {
		state.Branches = make(map[string]string)
	}
	if decision.ClearCheckpoint {
		delete(state.Branches, decision.Branch)
		return savePostCommitBatchState(decision.statePath, state)
	}
	state.Branches[decision.Branch] = decision.Head
	return savePostCommitBatchState(decision.statePath, state)
}
