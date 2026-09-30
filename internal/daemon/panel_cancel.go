package daemon

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"go.kenn.io/roborev/internal/storage"
)

// cascadeCancelPanelMembers cancels every member of a synthesis parent's run.
// It delegates to the shared cascadePanelMembers helper so the member-cancel
// loop is single-sourced between the HTTP cancel path and the CI poller.
func (s *Server) cascadeCancelPanelMembers(
	job *storage.ReviewJob, callerBroadcastsEvent bool,
) []storage.ReviewJob {
	canceled, err := cascadePanelMembers(s.db, func(id int64) {
		s.workerPool.cancelJob(id, callerBroadcastsEvent)
	}, job)
	if err != nil {
		log.Printf("cancel cascade: %v", err)
	}
	return canceled
}

// retireCIPanelForCanceledSynthesis makes a directly canceled CI synthesis
// parent non-postable without marking it posted. Non-CI panel runs have no
// ci_pr_panels mapping and are ignored. This covers queued/API cancellations
// that do not produce a worker review.canceled event.
func (s *Server) retireCIPanelForCanceledSynthesis(job *storage.ReviewJob) {
	if job == nil || job.PanelRole != storage.PanelRoleSynthesis || job.PanelRunUUID == nil {
		return
	}
	panel, err := s.db.GetCIPanelBySynthesisJobID(job.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		log.Printf("cancel cascade: lookup CI panel for synthesis %d: %v", job.ID, err)
		return
	}
	if err := s.db.MarkPanelRetired(panel.ID); err != nil {
		log.Printf("cancel cascade: retire CI panel %d: %v", panel.ID, err)
	}
	if err := s.db.DeleteReviewAttempt(panel.GithubRepo, panel.PRNumber, panel.HeadSHA); err != nil {
		log.Printf("cancel cascade: delete CI review attempt for %s#%d@%s: %v",
			panel.GithubRepo, panel.PRNumber, panel.HeadSHA, err)
	}
}

// cascadePanelMembers cancels every member of a synthesis parent's run. It is a
// no-op unless job is a synthesis parent of a panel run. Best-effort: members
// that are already terminal (sql.ErrNoRows from CancelJob) are skipped, and any
// other per-member error is returned without aborting the cascade. This lets a
// cancel of the synthesis row tear down its still-queued members, which
// otherwise have no path to a terminal state. killWorker kills the running
// worker process for a member (may be nil — e.g. the CI poller in tests, where
// it is nil-guarded by the caller).
func cascadePanelMembers(
	db *storage.DB, killWorker func(int64), job *storage.ReviewJob,
) ([]storage.ReviewJob, error) {
	if job == nil || job.PanelRole != storage.PanelRoleSynthesis || job.PanelRunUUID == nil {
		return nil, nil
	}
	members, err := db.GetPanelMembers(*job.PanelRunUUID)
	if err != nil {
		return nil, fmt.Errorf("list members for %s: %w", job.PanelRunUUID, err)
	}
	canceled := make([]storage.ReviewJob, 0, len(members))
	var cancelErrors []error
	for i := range members {
		m := &members[i]
		if err := db.CancelJob(m.ID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				cancelErrors = append(cancelErrors, fmt.Errorf("cancel member %d: %w", m.ID, err))
			}
			continue
		}
		if killWorker != nil {
			killWorker(m.ID)
		}
		canceled = append(canceled, *m)
	}
	return canceled, errors.Join(cancelErrors...)
}

// cancelPanelRunParentFirst tears down a whole panel run by canceling the
// synthesis PARENT before cascading to its members. The parent-first order is
// correctness-critical and mirrors humaCancelJob (server.go): a running member
// that observes cancellation releases the synthesis gate via
// MaybeReleasePanelSynthesis, so if the cascade ran first a worker could still
// claim and complete the now-released synthesis despite the cancel. Canceling
// the parent first makes that release a no-op on an already-canceled row.
//
// synth must be the run's synthesis (parent) job; a nil synth is a no-op.
// killWorker kills the running worker process and may be nil (nil-guarded).
// Best-effort: an already-terminal synthesis (sql.ErrNoRows) is skipped, and the
// member cascade still runs so partially-canceled runs converge to fully
// terminal. The returned jobs are exactly the rows this call transitioned, in
// parent-first order, so callers can announce those state changes.
func cancelPanelRunParentFirst(
	db *storage.DB, killWorker func(int64), synth *storage.ReviewJob,
) ([]storage.ReviewJob, error) {
	if synth == nil {
		return nil, nil
	}
	canceled := make([]storage.ReviewJob, 0, 1)
	if err := db.CancelJob(synth.ID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("cancel synthesis %d: %w", synth.ID, err)
		}
	} else {
		canceled = append(canceled, *synth)
		if killWorker != nil {
			killWorker(synth.ID)
		}
	}
	members, err := cascadePanelMembers(db, killWorker, synth)
	return append(canceled, members...), err
}

// releaseSynthesisIfCanceledMember releases the run's synthesis when a member
// was canceled directly over HTTP. It is a no-op unless job is a panel member.
// The worker releases the synthesis on a member's terminal transition, but an
// HTTP cancel bypasses the worker, so a directly canceled member would leave the
// synthesis blocked until the safety sweep. MaybeReleasePanelSynthesis is
// idempotent and only releases once every member is terminal.
func (s *Server) releaseSynthesisIfCanceledMember(job *storage.ReviewJob) {
	if job == nil || job.PanelRole != storage.PanelRoleMember || job.PanelRunUUID == nil {
		return
	}
	if err := s.db.MaybeReleasePanelSynthesis(*job.PanelRunUUID); err != nil {
		log.Printf("cancel cascade: release synthesis for %s: %v", job.PanelRunUUID, err)
	}
}
