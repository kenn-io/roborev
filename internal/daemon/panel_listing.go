package daemon

import (
	"log"

	"go.kenn.io/roborev/internal/storage"
)

// attachPanelSummaries populates PanelSummary on the synthesis (parent) rows of
// a listing page so collapsed panels render reviewer counts without an N+1
// per-row fetch. It runs one GROUP BY aggregate over the page's panel runs and
// mutates jobs in place. Failures are logged and non-fatal — the listing still
// returns, just without summaries.
func attachPanelSummaries(db *storage.DB, jobs []storage.ReviewJob) {
	var runUUIDs []string
	for i := range jobs {
		if jobs[i].PanelRole == storage.PanelRoleSynthesis && jobs[i].PanelRunUUID != "" {
			runUUIDs = append(runUUIDs, jobs[i].PanelRunUUID)
		}
	}
	if len(runUUIDs) == 0 {
		return
	}

	summaries, err := db.GetPanelSummaries(runUUIDs)
	if err != nil {
		log.Printf("Warning: failed to load panel summaries: %v", err)
		return
	}

	for i := range jobs {
		if jobs[i].PanelRole != storage.PanelRoleSynthesis {
			continue
		}
		if summary, ok := summaries[jobs[i].PanelRunUUID]; ok {
			s := summary
			jobs[i].PanelSummary = &s
		}
	}
}
