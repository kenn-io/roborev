package daemon

import (
	"context"
	"log"
	"time"

	"go.kenn.io/roborev/internal/storage"
)

// panelSweepInterval is how often the safety sweep recovers jobs and panels.
const panelSweepInterval = 60 * time.Second

// runPanelSweep recovers orphaned jobs and blocked panels until ctx is canceled.
func (s *Server) runPanelSweep(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepStuckPanels(ctx)
		}
	}
}

func (s *Server) sweepOrphanedJobs(ctx context.Context) {
	ids, err := s.db.ListRunningJobIDsContext(ctx)
	if err != nil {
		log.Printf("panel sweep: list orphan candidates: %v", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		job, err := s.db.GetJobByIDContext(ctx, id)
		if err != nil {
			log.Printf("panel sweep: load orphan job %d: %v", id, err)
			continue
		}
		if job.WorkerID == "" {
			continue
		}
		s.failOrphanedJob(ctx, job)
	}
}

func (s *Server) failOrphanedJob(ctx context.Context, job *storage.ReviewJob) {
	wp := s.workerPool
	wp.attemptTransitionsMu.RLock()
	defer wp.attemptTransitionsMu.RUnlock()
	if wp.ownsJob(job.WorkerID, job.ID) {
		return
	}
	const errorMsg = "daemon did not save the job's outcome"
	updated, err := s.db.FailJobAttempt(ctx, job.ID, job.WorkerID, job.StartedAtRaw, errorMsg)
	if err != nil {
		log.Printf("panel sweep: fail orphan job %d: %v", job.ID, err)
		return
	}
	if updated {
		wp.invalidateBudgetSpend()
		event := eventForJob(jobEventType(job, "failed"), job, job.ID)
		event.Error = errorMsg
		event.SuppressHooks = true
		s.broadcaster.Broadcast(event)
		wp.logJobFailed(job.ID, job.WorkerID, job.Agent, errorMsg)
	}
}

// sweepStuckPanels fails orphaned jobs, then releases panels with terminal members.
func (s *Server) sweepStuckPanels(ctx context.Context) {
	s.sweepOrphanedJobs(ctx)
	if ctx.Err() != nil {
		return
	}
	runs, err := s.db.ListStuckPanelRunsContext(ctx)
	if err != nil {
		log.Printf("panel sweep: list stuck runs: %v", err)
		return
	}
	for _, u := range runs {
		if ctx.Err() != nil {
			return
		}
		if err := s.db.MaybeReleasePanelSynthesisContext(ctx, u); err != nil {
			log.Printf("panel sweep: release %s: %v", u, err)
		}
	}
}
