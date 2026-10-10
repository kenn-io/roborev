package daemon

import (
	"fmt"
	"slices"
	"strings"
	"time"

	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

// HealthObservation reports current failures and their recovery evidence from
// one durable snapshot. Cached review errors preserve history, while delivery
// and new worker failures affect health immediately, without another poll.
func (p *CIPoller) HealthObservation() storage.ComponentHealth {
	// Hold the lock through the storage read so retry dispatch ownership stays
	// consistent with the durable snapshot used to report recovery.
	p.mu.Lock()
	defer p.mu.Unlock()
	ci := storage.ComponentHealth{Name: "ci"}
	if !p.running {
		ci.Message = "not running"
		return ci
	}
	if p.initialPollPending {
		ci.Message = "waiting for initial poll"
		return ci
	}
	failures := make(map[ciPollTarget]string)
	for target, failure := range p.pollErrors {
		if !failure.reviewFailure {
			failures[target] = failure.message
		}
	}
	recovering := len(failures) == 0
	states, err := p.db.GetFailedReviewAttemptStates(p.monitoredRepos)
	if err != nil {
		ci.Message = "review health unavailable"
		return ci
	}
	now := time.Now().UTC()
	var earliest time.Time
	for _, state := range states {
		a := state.Attempt
		target := ciPollTarget{repo: a.GithubRepo, prNumber: a.PRNumber, headSHA: a.HeadSHA}
		if _, exists := failures[target]; !exists {
			failures[target] = fmt.Sprintf("review failed for %s#%d", a.GithubRepo, a.PRNumber)
		}
		deadline := a.FirstAttemptAt.Add(reviewpkg.DefaultRetrySchedule.TransientWall)
		if a.LastErrorClass != "transient" || a.FirstAttemptAt.IsZero() ||
			a.FirstAttemptAt.After(now) || !now.Before(deadline) || !p.retryWorkPending(target, state, deadline) {
			recovering = false
		}
		if earliest.IsZero() || deadline.Before(earliest) {
			earliest = deadline
		}
	}
	if len(failures) == 0 {
		ci.Healthy, ci.Message = true, "running"
		return ci
	}
	messages := make([]string, 0, len(failures))
	for _, message := range failures {
		messages = append(messages, message)
	}
	slices.Sort(messages)
	ci.Message = strings.Join(messages, "; ")
	if recovering {
		ci.Recovery = &storage.ComponentRecovery{ObservedAt: now, Deadline: earliest.UTC()}
	}
	return ci
}

func (p *CIPoller) retryWorkPending(target ciPollTarget, state storage.FailedReviewAttemptState, deadline time.Time) bool {
	a := state.Attempt
	switch a.State {
	case "deferred":
		// Due time is not a dispatch deadline: the next normal poll owns this
		// scheduled retry. Waiting remains bounded by the original retry budget.
		return a.NextAttemptAt != nil && !a.NextAttemptAt.Before(a.FirstAttemptAt) && a.NextAttemptAt.Before(deadline)
	case "pending":
		if state.Panel == nil {
			return p.dispatchingRetries[target] > 0
		}
	default:
		return false
	}
	if state.Panel == nil || state.Panel.PostedAt != nil || state.Synthesis == nil {
		return false
	}
	synth := state.Synthesis
	switch synth.Status {
	case storage.JobStatusRunning:
		return true
	case storage.JobStatusQueued:
		return !synth.ClaimBlocked || state.HasLiveMembers
	case storage.JobStatusDone, storage.JobStatusFailed:
		// Automatic publication and transient finalization are still part of
		// this retry. Use the same outcome classifier as the finalizer.
		var synthesisFailure *reviewpkg.ReviewResult
		if synth.Status == storage.JobStatusFailed {
			synthesisFailure = &reviewpkg.ReviewResult{Agent: synth.Agent, Status: reviewpkg.ResultFailed, Error: synth.Error}
		}
		outcome := classifyPanelOutcome(toReviewResults(state.Members), synthesisFailure, a.ConsecutiveGenuineAttempts)
		return outcome.Kind == OutcomePost || outcome.Kind == OutcomeDeferTransient
	default:
		return false
	}
}
