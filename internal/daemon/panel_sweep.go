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

// orphanJobGrace covers only the gap between claiming a job and marking ownership.
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
	ids, err := s.db.ListStalledJobIDs(orphanJobGrace)
	if err != nil {
		log.Printf("panel sweep: list orphan candidates: %v", err)
		return
	}
	for _, id := range ids {
		job, err := s.db.GetJobByID(id)
		if err != nil {
			log.Printf("panel sweep: load orphan job %d: %v", id, err)
			continue
		}
		s.failOrphanedJob(job)
	}
}

func (s *Server) failOrphanedJob(job *storage.ReviewJob) {
	wp := s.workerPool
	wp.attemptTransitionsMu.RLock()
	defer wp.attemptTransitionsMu.RUnlock()
	if wp.OwnsJob(job.ID) || job.WorkerID == "" || job.StartedAt == nil || time.Since(*job.StartedAt) <= orphanJobGrace {
		return
	}
	const errorMsg = "worker exited before the job finished"
	updated, err := s.db.FailJobAttempt(job.ID, job.WorkerID, job.StartedAtRaw, errorMsg)
	if err != nil {
		log.Printf("panel sweep: fail orphan job %d: %v", job.ID, err)
		return
	}
	if updated {
		wp.invalidateBudgetSpend()
		wp.broadcastFailed(job, job.Agent, errorMsg)
		if wp.errorLog != nil {
			wp.errorLog.LogError("worker", fmt.Sprintf("job %d failed: %s", job.ID, errorMsg), job.ID)
		}
		wp.logJobFailed(job.ID, job.WorkerID, job.Agent, errorMsg)
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
