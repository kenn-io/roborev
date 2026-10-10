package daemon

import (
	"context"
	"log"
	"time"
)

// retentionInterval is how often the daemon applies the prompt and job log
// retention settings.
const retentionInterval = time.Hour

// runRetention applies retention once at startup and then every interval
// until ctx is canceled. Settings are read on every pass, so a config reload
// takes effect without a restart.
func (s *Server) runRetention(ctx context.Context, interval time.Duration) {
	s.applyRetention(ctx, time.Now())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.applyRetention(ctx, time.Now())
		}
	}
}

// applyRetention removes stored prompts and job log files older than the
// configured retention. A setting of zero or less keeps everything.
func (s *Server) applyRetention(ctx context.Context, now time.Time) {
	cfg := s.configWatcher.Config()
	if days := cfg.PromptRetentionDays; days > 0 {
		cutoff := now.AddDate(0, 0, -days)
		removed, err := s.db.PruneJobPrompts(ctx, cutoff, cfg.Sync.Enabled)
		if err != nil {
			log.Printf("retention: %v", err)
		} else if removed > 0 {
			log.Printf("retention: removed prompts of %d jobs finished more than %d days ago", removed, days)
		}
	}
	if days := cfg.JobLogRetentionDays; days > 0 {
		if removed := CleanJobLogs(time.Duration(days) * 24 * time.Hour); removed > 0 {
			log.Printf("retention: removed %d job logs older than %d days", removed, days)
		}
	}
}
