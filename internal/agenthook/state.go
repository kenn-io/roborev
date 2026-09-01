package agenthook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	roborevgit "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
)

var agentHookGit = gitcmd.New()

type hookScope struct {
	WorktreeRoot        string
	TrackedRepoRoot     string
	TrackedRepoIdentity string
	Head                string
	Branch              string
	WorktreeKey         string
	CandidateLineageKey string
	SnoozedUntil        time.Time
	Tracked             bool
}

type TrackedRepoResolution struct {
	Tracked      bool
	RootPath     string
	Identity     string
	Name         string
	SnoozedUntil time.Time
}

type gitScope struct {
	WorktreeRoot string
	GitDir       string
	CommonDir    string
	Head         string
	Branch       string
}

func LoadState(reviews ReviewSource) (*StateStore, error) {
	path := StatePath()
	s := &StateStore{
		path:        path,
		sessions:    map[string]SessionState{},
		fixSessions: map[string]FixSession{},
		reviews:     reviews,
		now:         time.Now,
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open agent hook state: %w", err)
	}
	defer file.Close()

	var snap Snapshot
	if err := json.NewDecoder(file).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode agent hook state: %w", err)
	}
	if snap.Sessions != nil {
		s.sessions = snap.Sessions
	}
	if snap.FixSessions != nil {
		s.fixSessions = snap.FixSessions
	}
	return s, nil
}

func StatePath() string {
	return filepath.Join(config.DataDir(), "agent-hook", "state.json")
}

func (s *StateStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create agent hook state dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "state.*.json.tmp")
	if err != nil {
		return fmt.Errorf("create agent hook state temp: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(Snapshot{Sessions: s.sessions, FixSessions: s.fixSessions}); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode agent hook state: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod agent hook state temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close agent hook state temp: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace agent hook state: %w", err)
	}
	ok = true
	return nil
}

// saveSessionLocked publishes a cloned session only when its atomic state-file
// replacement succeeds. Callers must hold s.mu.
func (s *StateStore) saveSessionLocked(sessionID string, state SessionState) error {
	previous, existed := s.sessions[sessionID]
	s.sessions[sessionID] = state
	if err := s.saveLocked(); err != nil {
		if existed {
			s.sessions[sessionID] = previous
		} else {
			delete(s.sessions, sessionID)
		}
		return err
	}
	return nil
}

// saveSessionAndFixSessionsLocked publishes session and worktree ownership
// together only when the state-file replacement succeeds. Callers must hold s.mu.
func (s *StateStore) saveSessionAndFixSessionsLocked(
	sessionID string,
	state SessionState,
	fixSessions map[string]FixSession,
) error {
	previous, existed := s.sessions[sessionID]
	previousFixSessions := s.fixSessions
	s.sessions[sessionID] = state
	s.fixSessions = fixSessions
	if err := s.saveLocked(); err != nil {
		if existed {
			s.sessions[sessionID] = previous
		} else {
			delete(s.sessions, sessionID)
		}
		s.fixSessions = previousFixSessions
		return err
	}
	return nil
}

func (s *StateStore) Record(req Request) (Response, error) {
	return s.RecordContext(context.Background(), req)
}

// Sessions returns an isolated snapshot of all tracked session state.
func (s *StateStore) Sessions() map[string]SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()

	sessions := make(map[string]SessionState, len(s.sessions))
	for id, state := range s.sessions {
		sessions[id] = cloneSessionState(state)
	}
	return sessions
}

// Reset removes one session or all sessions and persists the updated snapshot.
func (s *StateStore) Reset(sessionID string, all bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	previousSessions := s.sessions
	previousFixSessions := s.fixSessions
	if all {
		s.sessions = map[string]SessionState{}
		s.fixSessions = map[string]FixSession{}
	} else {
		s.sessions = maps.Clone(s.sessions)
		delete(s.sessions, sessionID)
		s.fixSessions = maps.Clone(s.fixSessions)
		for worktreeKey, fixSession := range s.fixSessions {
			if fixSession.SessionID == sessionID {
				delete(s.fixSessions, worktreeKey)
			}
		}
	}
	if err := s.saveLocked(); err != nil {
		s.sessions = previousSessions
		s.fixSessions = previousFixSessions
		return err
	}
	return nil
}

func cloneSessionState(state SessionState) SessionState {
	state.StopCountsSincePrompt = maps.Clone(state.StopCountsSincePrompt)
	state.CommitCountsSincePrompt = maps.Clone(state.CommitCountsSincePrompt)
	state.FailedReviewTriggeredCounts = maps.Clone(state.FailedReviewTriggeredCounts)
	state.AcknowledgedReviewIDs = maps.Clone(state.AcknowledgedReviewIDs)
	for key, ids := range state.AcknowledgedReviewIDs {
		state.AcknowledgedReviewIDs[key] = maps.Clone(ids)
	}
	state.RepoHeads = maps.Clone(state.RepoHeads)
	state.WorktreeLineageKeys = maps.Clone(state.WorktreeLineageKeys)
	state.PendingReminders = maps.Clone(state.PendingReminders)
	state.CommitSHAsSincePrompt = maps.Clone(state.CommitSHAsSincePrompt)
	for key, shas := range state.CommitSHAsSincePrompt {
		state.CommitSHAsSincePrompt[key] = slices.Clone(shas)
	}
	return state
}

func (s *StateStore) RecordContext(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	switch req.Event.HookEventName {
	case "PreToolUse":
		return s.recordPreToolUse(ctx, req)
	case "", "Stop":
		return s.recordStop(ctx, req)
	case "PostToolUse":
		return s.recordPostToolUse(ctx, req)
	default:
		return Response{SessionID: req.Event.SessionID, Skipped: true}, nil
	}
}

func (s *StateStore) recordStop(ctx context.Context, req Request) (Response, error) {
	if req.Event.StopHookActive {
		s.mu.Lock()
		st := s.sessions[req.Event.SessionID]
		s.mu.Unlock()
		return Response{
			SessionID:             req.Event.SessionID,
			Count:                 st.Count,
			Threshold:             req.Threshold,
			FailedReviewCount:     st.FailedReviewCount,
			FailedReviewThreshold: req.FailedReviewThreshold,
			ReminderPromptCount:   st.ReminderPromptCount,
			Skipped:               true,
		}, nil
	}
	scope, ok := s.resolveHookScope(ctx, req.Event.CWD)
	if ok {
		s.mu.Lock()
		fixSession, owned := s.activeOwnerFixSessionLocked(req, scope, s.currentTime())
		s.mu.Unlock()
		if owned {
			return Response{
				SessionID:    req.Event.SessionID,
				Triggered:    true,
				TriggeredBy:  "fix_session",
				FixSessionID: new(fixSession.ID),
				Reason:       "Finish the current Agent Hook fix.",
			}, nil
		}
	}
	snoozed := ok && scope.SnoozedUntil.After(time.Now())
	var prepare func(*SessionState) Response
	if snoozed {
		prepare = func(st *SessionState) Response {
			return applySnoozedState(st, req, scope)
		}
	}
	if resp, delivered, err := s.deliverPendingReminder(ctx, req, prepare); err != nil || delivered {
		return resp, err
	}
	if snoozed {
		return s.recordSnoozed(ctx, req, scope)
	}
	if !ok {
		return Response{
			SessionID:             req.Event.SessionID,
			Threshold:             req.Threshold,
			FailedReviewThreshold: req.FailedReviewThreshold,
			Skipped:               true,
		}, nil
	}
	if !scope.Tracked {
		return Response{
			SessionID:             req.Event.SessionID,
			Threshold:             req.Threshold,
			FailedReviewThreshold: req.FailedReviewThreshold,
			Skipped:               true,
		}, nil
	}
	openFailedReviewIDs, haveFailedReviewCount := findOpenFailedReviewIDs(
		ctx, s.reviews, scope.TrackedRepoRoot, scope.Branch, scope.Head,
	)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}

	st := cloneSessionState(s.sessions[req.Event.SessionID])
	lineageKey := ensureLineageKey(&st, scope)
	actionableReviewIDs := unacknowledgedReviewIDs(st, lineageKey, openFailedReviewIDs)
	failedReviewCount := len(actionableReviewIDs)

	now := s.currentTime()
	st.Count++
	if st.StopCountsSincePrompt == nil {
		st.StopCountsSincePrompt = map[string]int{}
	}
	st.StopCountsSincePrompt[lineageKey]++
	stopCountSincePrompt := st.StopCountsSincePrompt[lineageKey]
	st.LastTurnID = req.Event.TurnID
	st.LastCWD = req.Event.CWD
	st.LastSeenAt = now
	recordSequenceHeads(&st, scope, []string{scope.WorktreeKey})

	actionableReviews := hasActionableFailedReviews(failedReviewCount, haveFailedReviewCount)
	stopTriggered := thresholdReady(stopCountSincePrompt, req.Threshold) && actionableReviews
	failedReviewTriggered := applyFailedReviewTrigger(
		req, &st, scope.TrackedRepoRoot, scope.Branch, lineageKey,
		failedReviewCount, haveFailedReviewCount,
	)
	promptTriggered := stopTriggered || failedReviewTriggered
	fixSessions := s.fixSessions
	var fixSession *FixSession
	if promptTriggered {
		var deliveryAllowed bool
		fixSessions, fixSession, deliveryAllowed = s.prepareFixSessionGrantLocked(
			req, scope.WorktreeKey, now,
		)
		if !deliveryAllowed {
			promptTriggered = false
			stopTriggered = false
			failedReviewTriggered = false
			delete(st.FailedReviewTriggeredCounts, lineageKey)
		}
	}
	if promptTriggered {
		if stopTriggered {
			st.TriggeredAt = now
		}
		acknowledgeReviewIDs(&st, lineageKey, actionableReviewIDs)
		delete(st.FailedReviewTriggeredCounts, lineageKey)
		st.ReminderPromptCount++
		if failedReviewTriggered {
			st.FailedReviewTriggeredAt = now
		}
		resetPromptCountersForKeys(&st, promptResetKeys(scope, lineageKey))
		for key, pending := range st.PendingReminders {
			if pending.LineageKey == lineageKey {
				delete(st.PendingReminders, key)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if err := s.saveSessionAndFixSessionsLocked(req.Event.SessionID, st, fixSessions); err != nil {
		return Response{}, err
	}

	resp := Response{
		SessionID:             req.Event.SessionID,
		Count:                 st.Count,
		Threshold:             req.Threshold,
		FailedReviewCount:     st.FailedReviewCount,
		FailedReviewThreshold: req.FailedReviewThreshold,
		ReminderPromptCount:   st.ReminderPromptCount,
		Triggered:             promptTriggered,
	}
	if fixSession != nil {
		resp.FixSessionID = new(fixSession.ID)
	}
	switch {
	case failedReviewTriggered:
		resp.TriggeredBy = "failed_reviews"
		resp.Reason = buildFailedReviewReason(req, st, actionableReviewIDs)
	case stopTriggered:
		resp.TriggeredBy = "stop"
		resp.Reason = buildStopReason(req, stopCountSincePrompt, actionableReviewIDs)
	}
	return resp, nil
}

func (s *StateStore) recordPreToolUse(ctx context.Context, req Request) (Response, error) {
	if !isShellCommandTool(req.Event.ToolName) {
		return Response{
			SessionID:             req.Event.SessionID,
			CommitThreshold:       req.CommitThreshold,
			FailedReviewThreshold: req.FailedReviewThreshold,
			Skipped:               true,
		}, nil
	}
	if !IsCommitProducingCommand(req.Event.Command()) {
		return Response{
			SessionID:             req.Event.SessionID,
			CommitThreshold:       req.CommitThreshold,
			FailedReviewThreshold: req.FailedReviewThreshold,
			Skipped:               true,
		}, nil
	}

	scope, ok := s.resolveHookScope(ctx, commandGitDir(req.Event.CWD, req.Event.Command()))
	if !ok {
		return Response{
			SessionID:             req.Event.SessionID,
			CommitThreshold:       req.CommitThreshold,
			FailedReviewThreshold: req.FailedReviewThreshold,
			Skipped:               true,
		}, nil
	}
	if scope.SnoozedUntil.After(time.Now()) {
		return s.recordSnoozed(ctx, req, scope)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}

	st := cloneSessionState(s.sessions[req.Event.SessionID])
	if st.RepoHeads == nil {
		st.RepoHeads = map[string]string{}
	}
	lineageKey := ensureLineageKey(&st, scope)
	recordSequenceHeads(&st, scope, commitSequenceKeys(scope, lineageKey))
	st.LastCWD = req.Event.CWD
	st.LastSeenAt = time.Now().UTC()
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if err := s.saveSessionLocked(req.Event.SessionID, st); err != nil {
		return Response{}, err
	}

	return Response{
		SessionID:             req.Event.SessionID,
		CommitThreshold:       req.CommitThreshold,
		FailedReviewThreshold: req.FailedReviewThreshold,
	}, nil
}

func (s *StateStore) recordPostToolUse(ctx context.Context, req Request) (Response, error) {
	if !isShellCommandTool(req.Event.ToolName) {
		return Response{
			SessionID:             req.Event.SessionID,
			CommitThreshold:       req.CommitThreshold,
			FailedReviewThreshold: req.FailedReviewThreshold,
			Skipped:               true,
		}, nil
	}

	command := req.Event.Command()
	commitCommand := IsCommitProducingCommand(command)
	// Only commit commands move HEAD, so only they need the effective working
	// directory resolved from -C options; every other command tracks the cwd repo.
	gitDir := req.Event.CWD
	if commitCommand {
		gitDir = commandGitDir(req.Event.CWD, command)
	}

	scope, ok := s.resolveHookScope(ctx, gitDir)
	if !ok {
		return Response{
			SessionID:             req.Event.SessionID,
			CommitThreshold:       req.CommitThreshold,
			FailedReviewThreshold: req.FailedReviewThreshold,
			Skipped:               true,
		}, nil
	}
	if scope.SnoozedUntil.After(time.Now()) {
		return s.recordSnoozed(ctx, req, scope)
	}

	var openFailedReviewIDs reviewIDSet
	haveFailedReviewCount := false
	if scope.Tracked {
		openFailedReviewIDs, haveFailedReviewCount = findOpenFailedReviewIDs(
			ctx, s.reviews, scope.TrackedRepoRoot, scope.Branch, scope.Head,
		)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}

	st := cloneSessionState(s.sessions[req.Event.SessionID])
	if st.RepoHeads == nil {
		st.RepoHeads = map[string]string{}
	}
	priorLineageKey := ""
	if st.WorktreeLineageKeys != nil {
		priorLineageKey = st.WorktreeLineageKeys[scope.WorktreeKey]
	}
	lineageKey := ensureLineageKey(&st, scope)
	preserveDetachedRewriteLineage := false
	if commitCommand && scope.Branch != "" && detachedLineageKey(priorLineageKey) && lineageKey != priorLineageKey {
		previousWorktreeHead := st.RepoHeads[scope.WorktreeKey]
		if previousWorktreeHead != "" &&
			previousWorktreeHead != scope.Head &&
			!refReachableFromHead(scope.WorktreeRoot, previousWorktreeHead, scope.Head) &&
			commitsSincePromptForKey(st, scope.WorktreeKey) > 0 {
			preserveDetachedRewriteLineage = true
			lineageKey = priorLineageKey
			st.WorktreeLineageKeys[scope.WorktreeKey] = priorLineageKey
		}
	}
	sequenceKeys := commitSequenceKeys(scope, lineageKey)
	// Count commits only against a HEAD baseline recorded earlier in the
	// session; the first observation merely establishes that baseline below.
	// Counting on the first observation would misfire when a failed commit
	// command leaves an unrelated older commit as the latest reflog entry.
	var eventNewCommits []string
	if commitCommand {
		for _, key := range sequenceKeys {
			previousHead := st.RepoHeads[key]
			if previousHead == "" || previousHead == scope.Head {
				continue
			}
			newCommits, continuous := newCommitSHAs(scope.WorktreeRoot, previousHead, scope.Head)
			if err := ctx.Err(); err != nil {
				return Response{}, err
			}
			if !continuous {
				if st.CommitSHAsSincePrompt == nil {
					st.CommitSHAsSincePrompt = map[string][]string{}
				}
				st.CommitSHAsSincePrompt[key] = pendingCommitSHAsAfterRewrite(
					scope.WorktreeRoot, st.CommitSHAsSincePrompt[key], scope.Head,
				)
				delete(st.CommitCountsSincePrompt, key)
				eventNewCommits = appendUniqueCommitSHAs(eventNewCommits, []string{scope.Head})
				if key == scope.WorktreeKey {
					if preserveDetachedRewriteLineage {
						st.WorktreeLineageKeys[key] = lineageKey
					} else {
						st.WorktreeLineageKeys[key] = scope.CandidateLineageKey
						lineageKey = scope.CandidateLineageKey
					}
				}
				continue
			}
			if len(newCommits) == 0 {
				continue
			}
			if st.CommitSHAsSincePrompt == nil {
				st.CommitSHAsSincePrompt = map[string][]string{}
			}
			st.CommitSHAsSincePrompt[key] = appendUniqueCommitSHAs(st.CommitSHAsSincePrompt[key], newCommits)
			eventNewCommits = appendUniqueCommitSHAs(eventNewCommits, newCommits)
		}
	}

	recordSequenceHeads(&st, scope, sequenceKeys)
	st.LastCWD = req.Event.CWD
	now := s.currentTime()
	st.LastSeenAt = now
	if len(eventNewCommits) > 0 {
		st.CommitCount += len(eventNewCommits)
		st.LastCommitRepo = scope.WorktreeRoot
		st.LastCommitHead = scope.Head
	}

	actionableReviewIDs := unacknowledgedReviewIDs(st, lineageKey, openFailedReviewIDs)
	failedReviewCount := len(actionableReviewIDs)
	actionableReviews := hasActionableFailedReviews(failedReviewCount, haveFailedReviewCount)
	// The commit reminder fires once this checkout's threshold is met and
	// actionable failed reviews exist; it does not require a commit in this exact
	// event, because reviews are produced asynchronously and the failures for the
	// commit that crossed the threshold usually only land on a later tool call.
	// The count is keyed by both worktree and branch, so a deferred reminder for
	// one checkout is not consumed or reset by unrelated activity. thresholdReady
	// implies a real commit was counted for this checkout since its last prompt.
	commitCountSincePrompt := commitsSincePromptForKeys(st, sequenceKeys)
	commitTriggered := thresholdReady(commitCountSincePrompt, req.CommitThreshold) && actionableReviews
	// Capture this checkout's count before resetPromptCounters clears it, so the
	// reminder text reports the triggering repo's commits, not session-wide totals.
	triggeringCommitCount := commitCountSincePrompt
	failedReviewTriggered := applyFailedReviewTrigger(
		req, &st, scope.TrackedRepoRoot, scope.Branch, lineageKey,
		failedReviewCount, haveFailedReviewCount,
	)
	promptTriggered := commitTriggered || failedReviewTriggered
	fixSessions := s.fixSessions
	var fixSession *FixSession
	if promptTriggered && !req.DeferPostToolReminder {
		var deliveryAllowed bool
		fixSessions, fixSession, deliveryAllowed = s.prepareFixSessionGrantLocked(
			req, scope.WorktreeKey, now,
		)
		if !deliveryAllowed {
			promptTriggered = false
			commitTriggered = false
			failedReviewTriggered = false
			delete(st.FailedReviewTriggeredCounts, lineageKey)
		}
	}
	if promptTriggered && req.DeferPostToolReminder {
		if failedReviewTriggered {
			queuePendingReminder(&st, PendingReminder{
				TriggeredBy:         "failed_reviews",
				Reason:              deferredReminderReason(buildFailedReviewReason(req, st, actionableReviewIDs), scope.WorktreeRoot),
				Instruction:         req.Instruction,
				TrackedRepoRoot:     scope.TrackedRepoRoot,
				TrackedRepoIdentity: scope.TrackedRepoIdentity,
				WorktreeRoot:        scope.WorktreeRoot,
				Branch:              scope.Branch,
				Head:                scope.Head,
				LineageKey:          lineageKey,
				FailedReviewCount:   failedReviewCount,
				CreatedAt:           now,
			})
		}
		if commitTriggered {
			queuePendingReminder(&st, PendingReminder{
				TriggeredBy:         "commit",
				Reason:              deferredReminderReason(buildCommitReason(req, triggeringCommitCount, scope.WorktreeRoot, actionableReviewIDs), scope.WorktreeRoot),
				Instruction:         req.Instruction,
				TrackedRepoRoot:     scope.TrackedRepoRoot,
				TrackedRepoIdentity: scope.TrackedRepoIdentity,
				WorktreeRoot:        scope.WorktreeRoot,
				Branch:              scope.Branch,
				Head:                scope.Head,
				LineageKey:          lineageKey,
				CommitCount:         triggeringCommitCount,
				FailedReviewCount:   failedReviewCount,
				CreatedAt:           now,
			})
		}
		resetPromptCountersForKeys(&st, promptResetKeys(scope, lineageKey))
	} else if promptTriggered {
		if commitTriggered {
			st.CommitTriggeredAt = now
		}
		acknowledgeReviewIDs(&st, lineageKey, actionableReviewIDs)
		delete(st.FailedReviewTriggeredCounts, lineageKey)
		st.ReminderPromptCount++
		if failedReviewTriggered {
			st.FailedReviewTriggeredAt = now
		}
		resetPromptCountersForKeys(&st, promptResetKeys(scope, lineageKey))
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if err := s.saveSessionAndFixSessionsLocked(req.Event.SessionID, st, fixSessions); err != nil {
		return Response{}, err
	}

	resp := Response{
		SessionID:             req.Event.SessionID,
		Count:                 st.Count,
		Threshold:             req.Threshold,
		CommitCount:           st.CommitCount,
		CommitThreshold:       req.CommitThreshold,
		FailedReviewCount:     st.FailedReviewCount,
		FailedReviewThreshold: req.FailedReviewThreshold,
		ReminderPromptCount:   st.ReminderPromptCount,
		Triggered:             promptTriggered && !req.DeferPostToolReminder,
	}
	if fixSession != nil {
		resp.FixSessionID = new(fixSession.ID)
	}
	if req.DeferPostToolReminder {
		return resp, nil
	}
	switch {
	case failedReviewTriggered:
		resp.TriggeredBy = "failed_reviews"
		resp.Reason = buildFailedReviewReason(req, st, actionableReviewIDs)
	case commitTriggered:
		resp.TriggeredBy = "commit"
		resp.Reason = buildCommitReason(req, triggeringCommitCount, scope.WorktreeRoot, actionableReviewIDs)
	}
	return resp, nil
}

// recordSnoozed advances checkout baselines without accumulating reminder
// thresholds. Review work continues; only agent-facing reminders are muted.
func (s *StateStore) recordSnoozed(
	ctx context.Context,
	req Request,
	scope hookScope,
) (Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}

	st := cloneSessionState(s.sessions[req.Event.SessionID])
	resp := applySnoozedState(&st, req, scope)
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if err := s.saveSessionLocked(req.Event.SessionID, st); err != nil {
		return Response{}, err
	}
	return resp, nil
}

func applySnoozedState(st *SessionState, req Request, scope hookScope) Response {
	lineageKey := ensureLineageKey(st, scope)
	keys := uniqueStrings(append(
		[]string{scope.WorktreeKey, lineageKey},
		commitSequenceKeys(scope, lineageKey)...,
	))
	recordSequenceHeads(st, scope, keys)
	resetPromptCountersForKeys(st, keys)
	delete(st.FailedReviewTriggeredCounts, lineageKey)
	for key, pending := range st.PendingReminders {
		if pending.LineageKey == lineageKey {
			delete(st.PendingReminders, key)
		}
	}
	st.FailedReviewCount = 0
	st.LastCWD = req.Event.CWD
	st.LastSeenAt = time.Now().UTC()
	return Response{
		SessionID:             req.Event.SessionID,
		Count:                 st.Count,
		Threshold:             req.Threshold,
		CommitCount:           st.CommitCount,
		CommitThreshold:       req.CommitThreshold,
		FailedReviewThreshold: req.FailedReviewThreshold,
		ReminderPromptCount:   st.ReminderPromptCount,
		Skipped:               true,
	}
}

func pendingReminderKey(reminder PendingReminder) string {
	return reminder.LineageKey + "\x00" + reminder.TriggeredBy
}

func queuePendingReminder(st *SessionState, reminder PendingReminder) {
	if st.PendingReminders == nil {
		st.PendingReminders = map[string]PendingReminder{}
	}
	key := pendingReminderKey(reminder)
	if existing, ok := st.PendingReminders[key]; ok {
		reminder.CreatedAt = existing.CreatedAt
		reminder.CommitCount += existing.CommitCount
	}
	st.PendingReminders[key] = reminder
}

func deferredReminderReason(reason, worktree string) string {
	return fmt.Sprintf(
		"%s The triggering worktree is %s; change to it before running roborev commands.",
		strings.TrimSpace(reason), quoteReminderWorktree(worktree),
	)
}

func quoteReminderWorktree(worktree string) string {
	runes := []rune(worktree)
	var quoted strings.Builder
	quoted.WriteByte('"')
	for i := 0; i < len(runes); {
		switch runes[i] {
		case '\\':
			end := i
			for end < len(runes) && runes[end] == '\\' {
				end++
			}
			count := end - i
			switch {
			case end == len(runes):
				quoted.WriteString(strings.Repeat(`\`, count*2))
			case runes[end] == '"':
				quoted.WriteString(strings.Repeat(`\`, count*2+1))
				quoted.WriteByte('"')
				end++
			default:
				quoted.WriteString(strings.Repeat(`\`, count))
			}
			i = end
		case '"':
			quoted.WriteString(`\"`)
			i++
		case '\n':
			quoted.WriteString(`\n`)
			i++
		case '\r':
			quoted.WriteString(`\r`)
			i++
		case '\t':
			quoted.WriteString(`\t`)
			i++
		default:
			if unicode.IsControl(runes[i]) {
				fmt.Fprintf(&quoted, `\u%04x`, runes[i])
			} else {
				quoted.WriteRune(runes[i])
			}
			i++
		}
	}
	quoted.WriteByte('"')
	return quoted.String()
}

type pendingReminderCandidate struct {
	key      string
	reminder PendingReminder
}

func (s *StateStore) deliverPendingReminder(
	ctx context.Context,
	req Request,
	prepare func(*SessionState) Response,
) (Response, bool, error) {
	s.mu.Lock()
	st := s.sessions[req.Event.SessionID]
	candidates := make([]pendingReminderCandidate, 0, len(st.PendingReminders))
	for key, reminder := range st.PendingReminders {
		candidates = append(candidates, pendingReminderCandidate{key: key, reminder: reminder})
	}
	s.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		leftPriority := pendingReminderPriority(left.reminder.TriggeredBy)
		rightPriority := pendingReminderPriority(right.reminder.TriggeredBy)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		if !left.reminder.CreatedAt.Equal(right.reminder.CreatedAt) {
			return left.reminder.CreatedAt.Before(right.reminder.CreatedAt)
		}
		return left.key < right.key
	})

	discards := make([]pendingReminderCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		pending := candidate.reminder
		resolved, known := s.resolvePendingReminderRepo(ctx, pending)
		if err := ctx.Err(); err != nil {
			return Response{}, false, err
		}
		if known && (!resolved.Tracked || resolved.SnoozedUntil.After(time.Now())) {
			discards = append(discards, candidate)
			continue
		}
		if !pendingReminderLineageMatches(ctx, pending, resolved, known) {
			if err := ctx.Err(); err != nil {
				return Response{}, false, err
			}
			continue
		}
		openFailedReviewIDs, ok := findOpenFailedReviewIDs(
			ctx, s.reviews, pending.TrackedRepoRoot, pending.Branch, pending.Head,
		)
		if err := ctx.Err(); err != nil {
			return Response{}, false, err
		}
		if !ok {
			continue
		}
		if len(openFailedReviewIDs) == 0 {
			discards = append(discards, candidate)
			continue
		}

		s.mu.Lock()
		// Persistence is the at-most-once delivery boundary. The hook protocol
		// has no acknowledgment, so cancellation observed after this point
		// cannot safely distinguish a delivered response from a disconnect.
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return Response{}, false, err
		}
		st = cloneSessionState(s.sessions[req.Event.SessionID])
		applyPendingReminderDiscards(&st, discards)
		current, ok := st.PendingReminders[candidate.key]
		if !ok || current != candidate.reminder {
			s.mu.Unlock()
			continue
		}
		if prepare != nil {
			prepare(&st)
			current, ok = st.PendingReminders[candidate.key]
			if !ok || current != candidate.reminder {
				s.mu.Unlock()
				continue
			}
		}
		dedupeKey := pending.LineageKey
		if dedupeKey == "" {
			dedupeKey = repoHeadKey(pending.TrackedRepoRoot, pending.Branch)
		}
		actionableReviewIDs := unacknowledgedReviewIDs(st, dedupeKey, openFailedReviewIDs)
		if len(actionableReviewIDs) == 0 {
			delete(st.PendingReminders, candidate.key)
			delete(st.FailedReviewTriggeredCounts, dedupeKey)
			st.FailedReviewCount = 0
			if err := s.saveSessionLocked(req.Event.SessionID, st); err != nil {
				s.mu.Unlock()
				return Response{}, false, err
			}
			s.mu.Unlock()
			continue
		}
		fixSessions, fixSession, deliveryAllowed := s.prepareFixSessionGrantLocked(
			req,
			worktreeSequenceKey(pending.TrackedRepoRoot, pending.WorktreeRoot),
			s.currentTime(),
		)
		if !deliveryAllowed {
			s.mu.Unlock()
			continue
		}
		pending.FailedReviewCount = len(actionableReviewIDs)
		reasonReq := req
		reasonReq.Instruction = pending.Instruction
		switch pending.TriggeredBy {
		case "failed_reviews":
			pending.Reason = deferredReminderReason(buildFailedReviewReason(reasonReq, SessionState{
				FailedReviewCount:      len(actionableReviewIDs),
				LastFailedReviewRepo:   pending.TrackedRepoRoot,
				LastFailedReviewBranch: pending.Branch,
			}, actionableReviewIDs), pending.WorktreeRoot)
		case "commit":
			pending.Reason = deferredReminderReason(buildCommitReason(
				reasonReq, pending.CommitCount, pending.TrackedRepoRoot, actionableReviewIDs,
			), pending.WorktreeRoot)
		}
		delete(st.PendingReminders, candidate.key)
		acknowledgeReviewIDs(&st, dedupeKey, actionableReviewIDs)
		delete(st.FailedReviewTriggeredCounts, dedupeKey)
		st.ReminderPromptCount++
		st.FailedReviewCount = len(actionableReviewIDs)
		st.LastFailedReviewRepo = pending.TrackedRepoRoot
		st.LastFailedReviewBranch = pending.Branch
		now := s.currentTime()
		switch pending.TriggeredBy {
		case "failed_reviews":
			st.FailedReviewTriggeredAt = now
		case "commit":
			st.CommitTriggeredAt = now
		}
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return Response{}, false, err
		}
		err := s.saveSessionAndFixSessionsLocked(req.Event.SessionID, st, fixSessions)
		s.mu.Unlock()
		if err != nil {
			return Response{}, false, err
		}
		response := Response{
			SessionID:             req.Event.SessionID,
			Count:                 st.Count,
			Threshold:             req.Threshold,
			CommitCount:           pending.CommitCount,
			CommitThreshold:       req.CommitThreshold,
			FailedReviewCount:     pending.FailedReviewCount,
			FailedReviewThreshold: req.FailedReviewThreshold,
			ReminderPromptCount:   st.ReminderPromptCount,
			Triggered:             true,
			TriggeredBy:           pending.TriggeredBy,
			Reason:                pending.Reason,
		}
		if fixSession != nil {
			response.FixSessionID = new(fixSession.ID)
		}
		return response, true, nil
	}

	if len(discards) == 0 && prepare == nil {
		return Response{}, false, nil
	}

	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return Response{}, false, err
	}
	st = cloneSessionState(s.sessions[req.Event.SessionID])
	changed := applyPendingReminderDiscards(&st, discards)
	resp := Response{}
	if prepare != nil {
		resp = prepare(&st)
		changed = true
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return Response{}, false, err
	}
	if changed {
		if err := s.saveSessionLocked(req.Event.SessionID, st); err != nil {
			s.mu.Unlock()
			return Response{}, false, err
		}
	}
	s.mu.Unlock()
	if prepare != nil {
		return resp, true, nil
	}
	return Response{}, false, nil
}

func (s *StateStore) resolvePendingReminderRepo(
	ctx context.Context,
	pending PendingReminder,
) (TrackedRepoResolution, bool) {
	path := pending.WorktreeRoot
	if path == "" {
		path = pending.TrackedRepoRoot
	}
	if s.reviews == nil {
		return TrackedRepoResolution{}, false
	}
	return s.reviews.ResolveTrackedRepo(ctx, path, pending.Branch)
}

func pendingReminderLineageMatches(
	ctx context.Context,
	pending PendingReminder,
	resolved TrackedRepoResolution,
	known bool,
) bool {
	current, ok := currentGitScopeContext(ctx, pending.WorktreeRoot)
	if !ok {
		return false
	}
	if filepath.Clean(current.WorktreeRoot) != filepath.Clean(pending.WorktreeRoot) ||
		filepath.Clean(mainRepoRoot(ctx, current)) != filepath.Clean(pending.TrackedRepoRoot) {
		return false
	}
	if !known || !resolved.Tracked {
		return false
	}
	if resolved.RootPath != "" &&
		filepath.Clean(resolved.RootPath) != filepath.Clean(pending.TrackedRepoRoot) {
		return false
	}
	if pending.TrackedRepoIdentity != "" && resolved.Identity != pending.TrackedRepoIdentity {
		return false
	}
	// A missing head does not provide enough information for a finer lineage check.
	if pending.Head == "" {
		return true
	}
	if pending.Branch != "" {
		return current.Branch == pending.Branch
	}
	return current.Branch == "" && current.Head == pending.Head
}

func applyPendingReminderDiscards(
	st *SessionState,
	candidates []pendingReminderCandidate,
) bool {
	changed := false
	for _, candidate := range candidates {
		current, ok := st.PendingReminders[candidate.key]
		if !ok || current != candidate.reminder {
			continue
		}
		delete(st.PendingReminders, candidate.key)
		dedupeKey := current.LineageKey
		if dedupeKey == "" {
			dedupeKey = repoHeadKey(current.TrackedRepoRoot, current.Branch)
		}
		delete(st.FailedReviewTriggeredCounts, dedupeKey)
		if st.LastFailedReviewRepo == current.TrackedRepoRoot &&
			st.LastFailedReviewBranch == current.Branch {
			st.FailedReviewCount = 0
		}
		changed = true
	}
	return changed
}

func pendingReminderPriority(triggeredBy string) int {
	if triggeredBy == "failed_reviews" {
		return 0
	}
	return 1
}

func hasActionableFailedReviews(count int, ok bool) bool {
	return ok && count > 0
}

func thresholdReady(countSincePrompt, threshold int) bool {
	return threshold > 0 && countSincePrompt >= threshold
}

func isShellCommandTool(toolName string) bool {
	switch toolName {
	case "", "Bash", "Execute", "run_terminal_command", "run_terminal_cmd":
		return true
	default:
		return false
	}
}

// resetPromptCountersForKeys restarts the per-workspace counters after a
// reminder fires without discarding progress owed to another repo or branch.
func resetPromptCountersForKeys(st *SessionState, keys []string) {
	for _, key := range uniqueStrings(keys) {
		delete(st.StopCountsSincePrompt, key)
		delete(st.CommitCountsSincePrompt, key)
		delete(st.CommitSHAsSincePrompt, key)
	}
}

func repoHeadKey(repoRoot, branch string) string {
	if branch == "" {
		return repoRoot
	}
	return repoRoot + "\x00" + branch
}

func worktreeSequenceKey(repoRoot, worktreeRoot string) string {
	return repoRoot + "\x00worktree\x00" + filepath.Clean(worktreeRoot)
}

func commitSequenceKeys(scope hookScope, lineageKey string) []string {
	if scope.Branch == "" {
		return []string{scope.WorktreeKey}
	}
	branchKey := repoHeadKey(scope.TrackedRepoRoot, scope.Branch)
	if detachedLineageKey(lineageKey) {
		return uniqueStrings([]string{scope.WorktreeKey, branchKey})
	}
	return []string{branchKey}
}

func promptResetKeys(scope hookScope, lineageKey string) []string {
	return uniqueStrings(append(
		[]string{lineageKey}, commitSequenceKeys(scope, lineageKey)...,
	))
}

func recordSequenceHeads(st *SessionState, scope hookScope, keys []string) {
	if st.RepoHeads == nil {
		st.RepoHeads = map[string]string{}
	}
	for _, key := range keys {
		st.RepoHeads[key] = scope.Head
	}
}

func lineageSequenceKey(repoRoot, branch, worktreeRoot, head string) string {
	if branch != "" {
		return repoHeadKey(repoRoot, branch)
	}
	worktreeRoot = filepath.Clean(worktreeRoot)
	return repoRoot + "\x00detached\x00" + worktreeRoot + "\x00" + head
}

func ensureLineageKey(st *SessionState, scope hookScope) string {
	if st.WorktreeLineageKeys == nil {
		st.WorktreeLineageKeys = map[string]string{}
	}
	prior := st.WorktreeLineageKeys[scope.WorktreeKey]
	if prior != "" {
		if prior == scope.CandidateLineageKey {
			return prior
		}
		previousHead := ""
		if st.RepoHeads != nil {
			previousHead = st.RepoHeads[scope.WorktreeKey]
		}
		reachable := previousHead != "" && refReachableFromHead(scope.WorktreeRoot, previousHead, scope.Head)
		if scope.Branch == "" && detachedLineageKey(prior) && reachable {
			return prior
		}
		if scope.Branch != "" && detachedLineageKey(prior) && reachable {
			return prior
		}
	}
	st.WorktreeLineageKeys[scope.WorktreeKey] = scope.CandidateLineageKey
	return scope.CandidateLineageKey
}

func detachedLineageKey(key string) bool {
	return strings.Contains(key, "\x00lineage\x00") || strings.Contains(key, "\x00detached\x00")
}

func commitsSincePromptForKey(st SessionState, key string) int {
	return len(st.CommitSHAsSincePrompt[key]) + st.CommitCountsSincePrompt[key]
}

func commitsSincePromptForKeys(st SessionState, keys []string) int {
	seen := map[string]bool{}
	legacyCount := 0
	for _, key := range keys {
		for _, sha := range st.CommitSHAsSincePrompt[key] {
			if sha != "" {
				seen[sha] = true
			}
		}
		legacyCount += st.CommitCountsSincePrompt[key]
	}
	return len(seen) + legacyCount
}

func pendingCommitSHAsAfterRewrite(repoRoot string, existing []string, newHead string) []string {
	kept := make([]string, 0, len(existing)+1)
	for _, sha := range existing {
		if refReachableFromHead(repoRoot, sha, newHead) {
			kept = appendUniqueCommitSHAs(kept, []string{sha})
		}
	}
	return appendUniqueCommitSHAs(kept, []string{newHead})
}

func appendUniqueCommitSHAs(existing, incoming []string) []string {
	if len(incoming) == 0 {
		return existing
	}
	seen := make(map[string]bool, len(existing)+len(incoming))
	for _, sha := range existing {
		if sha == "" {
			continue
		}
		seen[sha] = true
	}
	for _, sha := range incoming {
		sha = strings.TrimSpace(sha)
		if sha == "" || seen[sha] {
			continue
		}
		existing = append(existing, sha)
		seen[sha] = true
	}
	return existing
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		out = append(out, value)
		seen[value] = true
	}
	return out
}

func applyFailedReviewTrigger(
	req Request, st *SessionState, repoRoot, branch, lineageKey string, count int, ok bool,
) bool {
	if !ok || req.FailedReviewThreshold <= 0 {
		return false
	}
	st.FailedReviewCount = count
	st.LastFailedReviewRepo = repoRoot
	st.LastFailedReviewBranch = branch
	// failedReviewCount is scoped to the current repo/branch, so dedup the prompt
	// per repo/branch. A single session-wide counter would let a prompt in one
	// repo/branch suppress prompts in another with an equal or lower count.
	key := lineageKey
	if key == "" {
		key = repoHeadKey(repoRoot, branch)
	}
	if count < req.FailedReviewThreshold {
		delete(st.FailedReviewTriggeredCounts, key)
		return false
	}
	if !thresholdReady(count-st.FailedReviewTriggeredCounts[key], req.FailedReviewThreshold) {
		return false
	}
	if st.FailedReviewTriggeredCounts == nil {
		st.FailedReviewTriggeredCounts = map[string]int{}
	}
	st.FailedReviewTriggeredCounts[key] = count
	return true
}

func buildStopReason(req Request, count int, reviewIDs reviewIDSet) string {
	detail := fmt.Sprintf("%s reached.", countPhrase(count, "Stop hook", "Stop hooks"))
	return buildPromptReason(req, detail+formatReviewJobIDs(reviewIDs))
}

// buildCommitReason describes the commit reminder for the checkout that triggered
// it. count and repo come from the triggering repo/branch (CommitCountsSincePrompt
// before it is reset), not the session-wide totals, so a deferred reminder for one
// repo reports that repo and its count rather than whichever repo committed most
// recently.
func buildCommitReason(req Request, count int, repo string, reviewIDs reviewIDSet) string {
	detail := fmt.Sprintf("%s reached", countPhrase(count, "commit", "commits"))
	if repoName := quotedLabel(repoDisplayName(repo)); repoName != "" {
		detail += " in " + repoName
	}
	return buildPromptReason(req, detail+"."+formatReviewJobIDs(reviewIDs))
}

func buildFailedReviewReason(req Request, st SessionState, reviewIDs reviewIDSet) string {
	detail := countPhrase(st.FailedReviewCount, "open failed roborev review", "open failed roborev reviews")
	if branch := quotedLabel(st.LastFailedReviewBranch); branch != "" {
		detail += " on " + branch
	} else if repoName := quotedLabel(repoDisplayName(st.LastFailedReviewRepo)); repoName != "" {
		detail += " in " + repoName
	}
	return buildPromptReason(req, detail+"."+formatReviewJobIDs(reviewIDs))
}

// formatReviewJobIDs names the exact daemon-selected reviews in reminder context.
func formatReviewJobIDs(reviewIDs reviewIDSet) string {
	if len(reviewIDs) == 0 {
		return ""
	}
	formatted := make([]string, 0, len(reviewIDs))
	for _, id := range slices.Sorted(maps.Keys(reviewIDs)) {
		formatted = append(formatted, fmt.Sprintf("%d", id))
	}
	return " Review job IDs: " + strings.Join(formatted, ", ") + "."
}

func unacknowledgedReviewIDs(st SessionState, lineageKey string, openReviewIDs reviewIDSet) reviewIDSet {
	actionable := maps.Clone(openReviewIDs)
	for id := range st.AcknowledgedReviewIDs[lineageKey] {
		delete(actionable, id)
	}
	return actionable
}

func acknowledgeReviewIDs(st *SessionState, lineageKey string, reviewIDs reviewIDSet) {
	if len(reviewIDs) == 0 {
		return
	}
	if st.AcknowledgedReviewIDs == nil {
		st.AcknowledgedReviewIDs = map[string]reviewIDSet{}
	}
	acknowledged := maps.Clone(st.AcknowledgedReviewIDs[lineageKey])
	if acknowledged == nil {
		acknowledged = reviewIDSet{}
	}
	maps.Copy(acknowledged, reviewIDs)
	st.AcknowledgedReviewIDs[lineageKey] = acknowledged
}

// sanitizeLabel makes an untrusted git branch or repo (directory) name safe to
// embed in agent-facing hook text. Both are attacker-influenced, so it drops
// control characters and double quotes that could inject new instruction lines
// or break out of delimiting, collapses whitespace, and caps the length so a
// hostile name cannot flood or steer the active agent.
func sanitizeLabel(raw string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == '"' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, raw)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	const maxRunes = 64
	if runes := []rune(cleaned); len(runes) > maxRunes {
		cleaned = strings.TrimSpace(string(runes[:maxRunes]))
	}
	return cleaned
}

// quotedLabel returns raw sanitized and wrapped in quotes so it renders as a
// clearly delimited data token, or "" when nothing usable remains.
func quotedLabel(raw string) string {
	clean := sanitizeLabel(raw)
	if clean == "" {
		return ""
	}
	return fmt.Sprintf("%q", clean)
}

func buildPromptReason(req Request, detail string) string {
	instruction := strings.TrimSpace(req.Instruction)
	if instruction == "" {
		instruction = DefaultInstruction
	}
	if strings.TrimSpace(detail) == "" {
		return instruction
	}
	return instruction + " " + detail
}

func countPhrase(count int, singular, plural string) string {
	if count == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	return fmt.Sprintf("%d %s", count, plural)
}

func repoDisplayName(repoPath string) string {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return ""
	}
	base := filepath.Base(filepath.Clean(repoPath))
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

func currentGitScope(cwd string) (gitScope, bool) {
	return currentGitScopeContext(context.Background(), cwd)
}

func currentGitScopeContext(parent context.Context, cwd string) (gitScope, bool) {
	if cwd == "" {
		return gitScope{}, false
	}
	if metadata, err := roborevgit.ReadCheckoutMetadata(cwd); err == nil {
		return gitScope{
			WorktreeRoot: metadata.WorktreeRoot,
			GitDir:       metadata.GitDir,
			CommonDir:    metadata.CommonDir,
			Head:         metadata.Head,
			Branch:       metadata.Branch,
		}, true
	}
	return currentGitScopeSubprocess(parent, cwd)
}

func currentGitScopeSubprocess(parent context.Context, cwd string) (gitScope, bool) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	out, err := agentHookGit.Output(ctx, cwd,
		"rev-parse", "--show-toplevel", "--git-dir", "--git-common-dir", "HEAD", "--abbrev-ref", "HEAD")
	if err != nil {
		return gitScope{}, false
	}
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	if len(lines) < 5 {
		return gitScope{}, false
	}
	root := cleanGitPath(lines[0])
	gitDir := absGitPath(cwd, cleanGitPath(lines[1]))
	commonDir := absGitPath(cwd, cleanGitPath(lines[2]))
	head := strings.TrimSpace(lines[3])
	branch := strings.TrimSpace(lines[4])
	if branch == "HEAD" {
		branch = ""
	}
	if root == "" || gitDir == "" || commonDir == "" || head == "" {
		return gitScope{}, false
	}
	return gitScope{
		WorktreeRoot: root,
		GitDir:       gitDir,
		CommonDir:    commonDir,
		Head:         head,
		Branch:       branch,
	}, true
}

func cleanGitPath(path string) string {
	path = gitrepo.NormalizePath(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func absGitPath(base, path string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Clean(path)
}

func (s *StateStore) resolveHookScope(ctx context.Context, cwd string) (hookScope, bool) {
	gitInfo, ok := currentGitScopeContext(ctx, cwd)
	if !ok {
		return hookScope{}, false
	}
	trackedRoot := mainRepoRoot(ctx, gitInfo)
	trackedIdentity := ""
	tracked := true
	var snoozedUntil time.Time
	if s.reviews != nil {
		resolved, known := s.reviews.ResolveTrackedRepo(
			ctx, gitInfo.WorktreeRoot, gitInfo.Branch,
		)
		if known {
			if !resolved.Tracked {
				tracked = false
			} else if strings.TrimSpace(resolved.RootPath) != "" {
				trackedRoot = strings.TrimSpace(resolved.RootPath)
			}
			trackedIdentity = strings.TrimSpace(resolved.Identity)
			snoozedUntil = resolved.SnoozedUntil
		}
	}
	return hookScope{
		WorktreeRoot:        gitInfo.WorktreeRoot,
		TrackedRepoRoot:     trackedRoot,
		TrackedRepoIdentity: trackedIdentity,
		Head:                gitInfo.Head,
		Branch:              gitInfo.Branch,
		WorktreeKey:         worktreeSequenceKey(trackedRoot, gitInfo.WorktreeRoot),
		CandidateLineageKey: lineageSequenceKey(
			trackedRoot, gitInfo.Branch, gitInfo.WorktreeRoot, gitInfo.Head,
		),
		SnoozedUntil: snoozedUntil,
		Tracked:      tracked,
	}, true
}

// mainRepoRoot resolves the main repository root for daemon API queries,
// following linked worktrees to the path the daemon stores jobs under. The
// daemon canonicalizes jobs to the main root on enqueue but the /api/jobs
// filter matches the path as sent, so a worktree session that queried its own
// checkout root would miss failed reviews recorded for the main repo. The
// checkout root still drives branch and HEAD detection; only the repo filter
// needs the main root. Falls back to worktreeRoot when resolution fails (for
// example a plain checkout, where the two roots are identical).
func mainRepoRoot(ctx context.Context, scope gitScope) string {
	if scope.GitDir == "" || scope.CommonDir == "" || scope.GitDir == scope.CommonDir {
		return scope.WorktreeRoot
	}
	if bareCommonDir(ctx, scope.CommonDir) {
		return scope.WorktreeRoot
	}
	if filepath.Base(scope.CommonDir) == ".git" {
		return filepath.Dir(scope.CommonDir)
	}
	worktree := configuredWorktree(ctx, scope.CommonDir)
	if worktree == "" {
		return scope.WorktreeRoot
	}
	return worktree
}

func bareCommonDir(parent context.Context, commonDir string) bool {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	out, err := agentHookGit.Output(ctx, "", "config", "--file", filepath.Join(commonDir, "config"), "--bool", "core.bare")
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func configuredWorktree(parent context.Context, commonDir string) string {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	out, err := agentHookGit.Output(ctx, "", "config", "--file", filepath.Join(commonDir, "config"), "core.worktree")
	if err != nil {
		return ""
	}
	worktree := cleanGitPath(string(out))
	if worktree == "" {
		return ""
	}
	if !filepath.IsAbs(worktree) {
		worktree = filepath.Join(commonDir, worktree)
	}
	return filepath.Clean(worktree)
}

func newCommitSHAs(repoRoot, oldHead, newHead string) ([]string, bool) {
	if oldHead == "" || newHead == "" || oldHead == newHead {
		return nil, true
	}
	if !refReachableFromHead(repoRoot, oldHead, newHead) {
		return nil, false
	}
	out, err := gitOutput(repoRoot, "rev-list", "--reverse", oldHead+".."+newHead)
	if err != nil {
		return []string{newHead}, true
	}
	var shas []string
	for line := range strings.SplitSeq(out, "\n") {
		sha := strings.TrimSpace(line)
		if sha != "" {
			shas = append(shas, sha)
		}
	}
	if len(shas) == 0 {
		return []string{newHead}, true
	}
	return shas, true
}

func gitOutput(cwd string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := agentHookGit.Output(ctx, cwd, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func IsCommitProducingCommand(command string) bool {
	_, ok := commitInvocationChdirs(shellFields(command))
	return ok
}

// commitInvocationChdirs scans fields for the first git invocation whose
// subcommand is commit, cherry-pick or revert, returning that invocation's -C
// path arguments (in order) and whether such an invocation exists. It performs no
// filesystem access, keeping IsCommitProducingCommand a pure predicate; commandGitDir
// resolves the paths only for the invocation that produces a commit. Keying both
// off the same invocation aligns them in a chained Bash command:
// `git status && git -C sub commit` yields sub's paths, while
// `git -C sub status && git commit` yields none.
func commitInvocationChdirs(fields []string) ([]string, bool) {
	for i := range fields {
		if !isGitToken(fields[i]) {
			continue
		}
		chdirs, sub := gitInvocation(fields, i)
		if sub < len(fields) && isCommitSubcommand(cleanShellToken(fields[sub])) {
			return chdirs, true
		}
	}
	return nil, false
}

// gitInvocation walks the global options of the git invocation whose git token is
// fields[start], collecting its -C path arguments in order, and returns those
// paths together with the index of the subcommand token (the first non-option
// token), or len(fields) when the invocation has none.
func gitInvocation(fields []string, start int) ([]string, int) {
	var chdirs []string
	j := start + 1
	for j < len(fields) {
		token := cleanShellToken(fields[j])
		switch {
		case token == "-C":
			if j+1 >= len(fields) {
				return chdirs, len(fields)
			}
			chdirs = append(chdirs, cleanShellToken(fields[j+1]))
			j += 2
		case token == "-c" || token == "--git-dir" || token == "--work-tree":
			j += 2 // option takes a separate argument we do not use
		case strings.HasPrefix(token, "--git-dir=") || strings.HasPrefix(token, "--work-tree="):
			j++
		case strings.HasPrefix(token, "-"):
			j++
		default:
			return chdirs, j // first non-option token is the subcommand
		}
	}
	return chdirs, j
}

// commandGitDir returns the working directory the commit-producing git invocation
// in command operates on, honoring that invocation's -C options applied
// cumulatively and relative to cwd, the way git does. In a chained Bash command it
// resolves the same invocation whose subcommand is commit/cherry-pick/revert, not
// merely the first git token. A -C path is used only when it resolves to an
// existing directory: shell expansions such as $(...) or ${VAR}, which the hook
// cannot evaluate, and paths that do not exist fall back to cwd. This keeps repo
// and HEAD tracking pointed at the repository a commit actually lands in - for
// example `git -C ./submodule commit` from a superproject - rather than cwd.
//
// Security: cwd and command arrive in the local agent hook payload, so this path
// is influenced only by the same user the daemon already runs as, and it feeds a
// read-only os.Stat plus read-only `git` reads in directories that user controls -
// never a write or a privileged read. There is no trust boundary to cross, and
// pinning the result under a base directory would defeat the cross-repo/submodule
// resolution above, so the static-analysis path-injection flag on the cwd -> path
// flow is a false positive.
func commandGitDir(cwd, command string) string {
	chdirs, ok := commitInvocationChdirs(shellFields(command))
	if !ok {
		return cwd
	}
	return resolveChdirs(cwd, chdirs)
}

// resolveChdirs folds -C path arguments into a working directory, each resolved
// against the directory established by the previous one, as git applies them.
func resolveChdirs(cwd string, chdirs []string) string {
	dir := cwd
	for _, path := range chdirs {
		dir = existingDir(dir, path)
	}
	return dir
}

// existingDir resolves path against base (absolute paths are used as-is) and
// returns it only when it names an existing directory; otherwise it returns base.
func existingDir(base, path string) string {
	if path == "" {
		return base
	}
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(base, resolved)
	}
	if info, err := os.Stat(resolved); err == nil && info.IsDir() {
		return resolved
	}
	return base
}

func shellFields(command string) []string {
	var fields []string
	var b strings.Builder
	var quote rune
	escaped := false
	backslashEscapes := runtime.GOOS != "windows"
	expansionDepth := 0
	inToken := false
	pendingExpansion := false
	for _, r := range command {
		if escaped {
			b.WriteRune(r)
			inToken = true
			escaped = false
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				inToken = true
				continue
			}
			if quote != '\'' && backslashEscapes && r == '\\' {
				escaped = true
				inToken = true
				continue
			}
			b.WriteRune(r)
			inToken = true
			continue
		}
		if pendingExpansion && (r == '(' || r == '{') {
			b.WriteRune(r)
			expansionDepth++
			inToken = true
			pendingExpansion = false
			continue
		}
		pendingExpansion = false
		if expansionDepth > 0 {
			if r == '$' {
				pendingExpansion = true
			}
			if r == ')' || r == '}' {
				expansionDepth--
			}
			b.WriteRune(r)
			inToken = true
			continue
		}
		switch r {
		case '\\':
			if backslashEscapes {
				escaped = true
			} else {
				b.WriteRune(r)
			}
			inToken = true
		case '$':
			b.WriteRune(r)
			pendingExpansion = true
			inToken = true
		case '\'', '"', '`':
			quote = r
			inToken = true
		case ' ', '\t', '\r', '\n', ';', '&', '|', '[', ']', '<', '>':
			if inToken {
				fields = append(fields, b.String())
				b.Reset()
				inToken = false
			}
		default:
			b.WriteRune(r)
			inToken = true
		}
	}
	if escaped {
		b.WriteRune('\\')
	}
	if inToken {
		fields = append(fields, b.String())
	}
	return fields
}

func isGitToken(token string) bool {
	token = cleanShellToken(token)
	return token == "git" || strings.HasSuffix(token, "/git")
}

func isCommitSubcommand(token string) bool {
	switch token {
	case "commit", "cherry-pick", "revert":
		return true
	default:
		return false
	}
}

func cleanShellToken(token string) string {
	return strings.Trim(token, " \t\r\n'\"`;$&|(){}[]<>")
}

// countsAsFailedReview reports whether job is a review whose F verdict should
// drive the failed-review reminder. Review (single/range/dirty), synthesis, and
// compact jobs produce meaningful P/F verdicts; task, insights, fix, and classify
// jobs do not. A fix job in particular stores a verdict parsed from its own output
// (see storage.DB.CompleteFixJob), so counting it would make the hook keep
// prompting $roborev-fix for a job that is not a failing review. The empty
// job_type is counted for legacy jobs recorded before job_type existed.
func countsAsFailedReview(job storage.ReviewJob) bool {
	switch job.JobType {
	case storage.JobTypeReview, storage.JobTypeRange, storage.JobTypeDirty,
		storage.JobTypeCompact, storage.JobTypeSynthesis, "":
		return true
	default:
		return false
	}
}

func countOpenFailedReviews(
	ctx context.Context,
	reviews ReviewSource,
	repoRoot, branch, head string,
) (int, bool) {
	ids, ok := findOpenFailedReviewIDs(ctx, reviews, repoRoot, branch, head)
	return len(ids), ok
}

func findOpenFailedReviewIDs(
	ctx context.Context,
	reviews ReviewSource,
	repoRoot, branch, head string,
) (reviewIDSet, bool) {
	if repoRoot == "" || reviews == nil {
		return nil, false
	}
	jobs, ok := reviews.ListOpenReviewJobs(ctx, repoRoot, branch)
	if !ok {
		return nil, false
	}
	var lineageMatcher *roborevgit.BranchLineageMatcher
	lineageMatcherLoaded := false
	lineageMatches := func(ref string) bool {
		if !lineageMatcherLoaded {
			lineageMatcherLoaded = true
			lineageMatcher, _ = roborevgit.NewBranchLineageMatcherCtx(ctx, repoRoot, branch, head)
		}
		return lineageMatcher != nil && lineageMatcher.Matches(ref)
	}
	ids := make(reviewIDSet, len(jobs))
	for _, job := range jobs {
		if job.Status != "" && job.Status != storage.JobStatusDone {
			continue
		}
		if job.Closed != nil && *job.Closed {
			continue
		}
		if !countsAsFailedReview(job) {
			continue
		}
		if !failedReviewCountsForHead(repoRoot, branch, head, job, lineageMatches) {
			continue
		}
		if job.Verdict != nil && strings.EqualFold(*job.Verdict, "F") {
			ids[job.ID] = struct{}{}
		}
	}
	return ids, true
}

// failedReviewCountsForHead reports whether an open failed review returned by
// the jobs query counts toward the current checkout. branch_include_empty makes
// branchful queries also return branchless jobs, so the reachability gate used
// for detached HEAD must apply to those too - otherwise a stale or unrelated
// detached review would prompt $roborev-fix on a branch it does not belong to.
//
//   - On detached HEAD, reviews reachable from HEAD are ours, even when they
//     carry a branch label created after the worktree started detached.
//   - A job carrying a branch belongs to the queried branch (the daemon already
//     scoped the attached-branch query to it).
//   - On a branch, branchless repo-level or dirty reviews still count, matching
//     the long-standing reminder behavior. Branchless concrete refs count only
//     when they belong to the current branch lineage and are not trunk history.
func failedReviewCountsForHead(repoRoot, branch, head string, job storage.ReviewJob, lineageMatches func(string) bool) bool {
	if branch == "" {
		return head != "" && detachedReviewMatches(repoRoot, head, job)
	}
	if strings.TrimSpace(job.Branch) != "" {
		return true
	}
	ref := strings.TrimSpace(job.GitRef)
	if ref == "" || ref == "dirty" || head == "" {
		return true
	}
	return lineageMatches != nil && lineageMatches(ref)
}

func detachedReviewMatches(repoRoot, head string, job storage.ReviewJob) bool {
	ref := strings.TrimSpace(job.GitRef)
	if ref == "" || ref == "dirty" {
		return false
	}
	if ref == head {
		return true
	}
	if _, end, ok := roborevgit.ParseRange(ref); ok {
		return refReachableFromHead(repoRoot, strings.TrimSpace(end), head)
	}
	return refReachableFromHead(repoRoot, ref, head)
}

func refReachableFromHead(repoRoot, ref, head string) bool {
	if ref == "" || head == "" {
		return false
	}
	if ref == head {
		return true
	}
	ok, err := roborevgit.IsAncestor(repoRoot, ref, head)
	return err == nil && ok
}
