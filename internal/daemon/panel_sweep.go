package daemon

import (
	"context"
	"log"
	"time"
)

// panelSweepInterval is how often the safety sweep recovers jobs and panels.
const panelSweepInterval = 60 * time.Second

// orphanJobGrace allows workers time to register a newly claimed job.
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
	jobs, err := s.db.ListRunningJobsBefore(time.Now().Add(-orphanJobGrace))
	if err != nil {
		log.Printf("panel sweep: list orphan candidates: %v", err)
		return
	}
	const errorMsg = "worker exited before the job finished"
	for _, candidate := range jobs {
		if s.workerPool.IsJobRunning(candidate.ID) {
			continue
		}
		updated, err := s.workerPool.failJobAndInvalidateBudget(candidate.ID, candidate.WorkerID, errorMsg)
		if err != nil {
			log.Printf("panel sweep: fail orphan job %d: %v", candidate.ID, err)
			continue
		}
		if updated {
			job, err := s.db.GetJobByID(candidate.ID)
			if err != nil {
				log.Printf("panel sweep: load failed job %d: %v", candidate.ID, err)
				continue
			}
			s.workerPool.broadcastFailed(job, job.Agent, errorMsg)
		}
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
