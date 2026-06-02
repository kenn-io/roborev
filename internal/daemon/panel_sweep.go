package daemon

import (
	"context"
	"log"
	"time"
)

// panelSweepInterval is how often the safety sweep looks for panel synthesis
// jobs left blocked after all their members went terminal.
const panelSweepInterval = 60 * time.Second

// runPanelSweep periodically releases panel synthesis jobs whose members are all
// terminal but whose claim_blocked gate was never cleared (e.g. a missed worker
// release after a crash). It returns when ctx is canceled.
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

// sweepStuckPanels releases every panel run whose synthesis is still blocked
// despite all members being terminal. One sweep iteration; safe to call anytime.
func (s *Server) sweepStuckPanels() {
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
