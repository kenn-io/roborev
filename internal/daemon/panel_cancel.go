package daemon

import (
	"database/sql"
	"errors"
	"log"

	"go.kenn.io/roborev/internal/storage"
)

// cascadeCancelPanelMembers cancels every member of a synthesis parent's run.
// It delegates to the shared cascadePanelMembers helper so the member-cancel
// loop is single-sourced between the HTTP cancel path and the CI poller.
func (s *Server) cascadeCancelPanelMembers(job *storage.ReviewJob) {
	cascadePanelMembers(s.db, func(id int64) { s.workerPool.CancelJob(id) }, job)
}

// retireCIPanelForCanceledSynthesis makes a directly canceled CI synthesis
// parent non-postable without marking it posted. Non-CI panel runs have no
// ci_pr_panels mapping and are ignored. This covers queued/API cancellations
// that do not produce a worker review.canceled event.
func (s *Server) retireCIPanelForCanceledSynthesis(job *storage.ReviewJob) {
	if job == nil || job.PanelRole != storage.PanelRoleSynthesis || job.PanelRunUUID == "" {
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
// other per-member error is logged without aborting the cascade. This lets a
// cancel of the synthesis row tear down its still-queued members, which
// otherwise have no path to a terminal state. killWorker kills the running
// worker process for a member (may be nil — e.g. the CI poller in tests, where
// it is nil-guarded by the caller).
func cascadePanelMembers(db *storage.DB, killWorker func(int64), job *storage.ReviewJob) {
	if job == nil || job.PanelRole != storage.PanelRoleSynthesis || job.PanelRunUUID == "" {
		return
	}
	members, err := db.GetPanelMembers(job.PanelRunUUID)
	if err != nil {
		log.Printf("cancel cascade: list members for %s: %v", job.PanelRunUUID, err)
		return
	}
	for i := range members {
		m := &members[i]
		if err := db.CancelJob(m.ID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				log.Printf("cancel cascade: cancel member %d: %v", m.ID, err)
			}
			continue
		}
		if killWorker != nil {
			killWorker(m.ID)
		}
	}
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
// terminal.
func cancelPanelRunParentFirst(db *storage.DB, killWorker func(int64), synth *storage.ReviewJob) {
	if synth == nil {
		return
	}
	if err := db.CancelJob(synth.ID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("cancel cascade: cancel synthesis %d: %v", synth.ID, err)
		}
	} else if killWorker != nil {
		killWorker(synth.ID)
	}
	cascadePanelMembers(db, killWorker, synth)
}

// releaseSynthesisIfCanceledMember releases the run's synthesis when a member
// was canceled directly over HTTP. It is a no-op unless job is a panel member.
// The worker releases the synthesis on a member's terminal transition, but an
// HTTP cancel bypasses the worker, so a directly canceled member would leave the
// synthesis blocked until the safety sweep. MaybeReleasePanelSynthesis is
// idempotent and only releases once every member is terminal.
func (s *Server) releaseSynthesisIfCanceledMember(job *storage.ReviewJob) {
	if job == nil || job.PanelRole != storage.PanelRoleMember || job.PanelRunUUID == "" {
		return
	}
	if err := s.db.MaybeReleasePanelSynthesis(job.PanelRunUUID); err != nil {
		log.Printf("cancel cascade: release synthesis for %s: %v", job.PanelRunUUID, err)
	}
}
