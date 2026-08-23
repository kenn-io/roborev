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
	headRef := "HEAD"
	if branch != "" {
		headRef = "refs/heads/" + branch
	}
	head, headErr := git.ResolveSHACtx(ctx, root, headRef)
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
	checkpoint, hasCheckpoint, stateChanged := resolvePostCommitCheckpoint(
		ctx, root, branch, head, &state,
	)
	if !hasCheckpoint || checkpoint == "" {
		checkpoint, err = git.ResolveSHACtx(ctx, root, head+"^1")
		if err != nil {
			return failOpenPostCommitBatchWithState(
				branch, head, statePath,
				"resolve initial checkpoint: "+err.Error(),
				state,
			)
		}
		state.Branches[branch] = checkpoint
		stateChanged = true
	}
	if stateChanged {
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
		if _, resolveErr := git.ResolveSHACtx(
			ctx, root, checkpoint+"^{commit}",
		); resolveErr == nil {
			return recoverOffChainPostCommitBatch(
				root, branch, head, checkpoint, statePath,
				"count first-parent commits: "+err.Error(), state,
			)
		}
		return failOpenPostCommitBatchWithState(
			branch, head, statePath, "count first-parent commits: "+err.Error(),
			state,
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

// resolvePostCommitCheckpoint returns the branch's checkpoint, adopting
// orphaned state entries when that widens the pending range. An orphaned
// entry — a stored branch name that no longer resolves, typically left by a
// branch rename or deletion — whose checkpoint sits on this branch's
// first-parent chain marks commits that were counted as pending but never
// reviewed. Adoption is deliberately evidence-free: a wrong adoption costs at
// most a redundant review, while discarding a checkpoint can silently skip
// commits. On-chain orphans that don't widen the range are dropped because
// the branch's own pending range already covers them.
func resolvePostCommitCheckpoint(
	ctx context.Context,
	root, branch, head string,
	state *postCommitBatchState,
) (checkpoint string, hasCheckpoint, stateChanged bool) {
	own, hasOwn := state.Branches[branch]
	if len(state.Branches) == 0 ||
		(len(state.Branches) == 1 && hasOwn) {
		return own, hasOwn, false
	}
	baseline, ok := postCommitCheckpointBaseline(ctx, root, own, head)
	if !ok {
		return own, hasOwn, false
	}
	live, err := git.LocalBranchSet(ctx, root)
	if err != nil {
		return own, hasOwn, false
	}
	adopted := ""
	adoptedPending := baseline
	changed := false
	for name, candidate := range state.Branches {
		if name == branch || candidate == "" {
			continue
		}
		if _, exists := live[name]; exists {
			continue
		}
		pending, onChain, err := git.FirstParentDistance(
			ctx, root, candidate, head,
		)
		if err != nil || !onChain {
			continue
		}
		delete(state.Branches, name)
		changed = true
		if pending >= adoptedPending {
			adopted = candidate
			adoptedPending = pending
		}
	}
	if adopted == "" {
		return own, hasOwn, changed
	}
	state.Branches[branch] = adopted
	return adopted, true, true
}

// postCommitCheckpointBaseline returns the pending distance the branch
// already covers without adoption: its own on-chain checkpoint, the
// merge-base recovery boundary when that checkpoint is off-chain, or the
// HEAD^1 seed when it has no checkpoint. ok is false when no baseline can be
// established (unresolvable checkpoint, unrelated history); adoption is
// skipped then, because replacing the checkpoint could shrink what the
// existing recovery paths would review.
func postCommitCheckpointBaseline(
	ctx context.Context, root, own, head string,
) (int, bool) {
	if own == "" {
		// Seeding at HEAD^1 leaves exactly the new commit pending.
		return 1, true
	}
	pending, onChain, err := git.FirstParentDistance(ctx, root, own, head)
	if err != nil {
		return 0, false
	}
	if onChain {
		return pending, true
	}
	mergeBase, err := git.GetMergeBase(root, own, head)
	if err != nil {
		return 0, false
	}
	pending, onChain, err = git.FirstParentDistance(ctx, root, mergeBase, head)
	if err != nil || !onChain {
		return 0, false
	}
	return pending, true
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
	head, err := git.ResolveSHACtx(ctx, root, "refs/heads/"+branch)
	if err != nil {
		return decision
	}
	// A rename before batching was disabled leaves the pending range under
	// the old branch name; adoption moves it here so the drain covers it.
	checkpoint, hasCheckpoint, stateChanged := resolvePostCommitCheckpoint(
		ctx, root, branch, head, &state,
	)
	if stateChanged {
		if err := savePostCommitBatchState(statePath, state); err != nil {
			return decision
		}
	}
	if !hasCheckpoint {
		return decision
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
	checkpoint, hasCheckpoint, stateChanged := resolvePostCommitCheckpoint(
		ctx, root, branch, head, &state,
	)
	if stateChanged {
		if err := savePostCommitBatchState(statePath, state); err != nil {
			return failOpenPostCommitBatchWithState(
				branch, head, statePath,
				"update batch state: "+err.Error(), state,
			)
		}
		decision.state = state
	}
	if !hasCheckpoint || checkpoint == "" {
		return decision
	}
	pending, onChain, err := git.FirstParentDistance(ctx, root, checkpoint, head)
	if err != nil {
		if _, resolveErr := git.ResolveSHACtx(
			ctx, root, checkpoint+"^{commit}",
		); resolveErr == nil {
			return recoverOffChainPostCommitBatch(
				root, branch, head, checkpoint, statePath,
				"count first-parent commits: "+err.Error(), state,
			)
		}
		return failOpenPostCommitBatchWithState(
			branch, head, statePath,
			"count first-parent commits: "+err.Error(), state,
		)
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
	ctx context.Context,
	root string,
	pushedBranches map[string]string,
	pushedHeads map[string]struct{},
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
		if _, isPushed := pushedBranches[savedBranch]; isPushed {
			continue
		}
		tip, err := git.ResolveSHACtx(ctx, root, "refs/heads/"+savedBranch)
		if err != nil {
			continue
		}
		carried := ""
		carriedPending := 0
		for head := range pushedHeads {
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
	var commits []string
	if err != nil {
		commits, err = git.GetRangeCommits(root, head)
		if err == nil && len(commits) > 0 {
			mergeBase = commits[0] + "^"
			reason += "; merge-base unavailable, recovering from root"
		}
	} else {
		commits, err = git.GetRangeCommits(root, mergeBase+".."+head)
	}
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
