package daemon

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.kenn.io/roborev/internal/storage"
)

// panelSweepInterval is how often the safety sweep recovers jobs and panels.
const panelSweepInterval = 60 * time.Second

// orphanJobGrace delays recovery of recently started, ownerless attempts.
const orphanJobGrace = 2 * time.Minute

// runPanelSweep recovers orphaned jobs and blocked panels until ctx is canceled.
func (s *Server) runPanelSweep(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepStuckPanels()
		}
	}
}

func (s *Server) sweepOrphanedJobs() {
	jobs, err := s.db.ListStalledJobs(orphanJobGrace)
	if err != nil {
		log.Printf("panel sweep: list orphan candidates: %v", err)
		return
	}
	for _, job := range jobs {
		s.failOrphanedJob(&job)
	}
}

func (s *Server) failOrphanedJob(job *storage.StalledJob) {
	wp := s.workerPool
	if wp.ownsJob(job.WorkerID, job.ID) {
		return
	}
	current, err := s.db.GetJobByID(job.ID)
	if err != nil {
		log.Printf("panel sweep: load orphan job %d: %v", job.ID, err)
		return
	}
	wp.attemptTransitionsMu.RLock()
	if wp.ownsJob(job.WorkerID, job.ID) {
		wp.attemptTransitionsMu.RUnlock()
		return
	}
	const errorMsg = "worker stopped without saving the job's outcome"
	updated, err := s.db.FailJobAttempt(job.ID, job.WorkerID, job.StartedAt, errorMsg)
	wp.attemptTransitionsMu.RUnlock()
	if err != nil {
		log.Printf("panel sweep: fail orphan job %d: %v", job.ID, err)
		return
	}
	if updated {
		wp.invalidateBudgetSpend()
		// Daemon recovery suppresses hooks, unlike broadcastFailed's worker failures.
		event := eventForJob(jobEventType(current, "failed"), current, current.ID)
		event.Error = errorMsg
		event.WorktreePath = existingJobWorktreePath(current)
		event.SuppressHooks = true
		s.broadcaster.Broadcast(event)
		if wp.errorLog != nil {
			wp.errorLog.LogError("worker", fmt.Sprintf("job %d failed: %s", job.ID, errorMsg), job.ID)
		}
		wp.logJobFailed(job.ID, job.WorkerID, current.Agent, errorMsg)
		log.Printf("panel sweep: job %d failed: %s", job.ID, errorMsg)
	}
}

// sweepStuckPanels fails orphaned jobs, then releases panels with terminal members.
func (s *Server) sweepStuckPanels() {
	s.sweepOrphanedJobs()
	runs, err := s.db.ListStuckPanelRuns()
	if err != nil {
		log.Printf("panel sweep: list stuck runs: %v", err)
		return
	}
	for _, u := range runs {
		if err := s.db.MaybeReleasePanelSynthesis(u); err != nil {
			log.Printf("panel sweep: release %s: %v", u, err)
		}
	}
}
