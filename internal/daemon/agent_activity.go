package daemon

import (
	"context"
	"time"

	"go.kenn.io/roborev/internal/telemetry"
)

// recordAgentCall persists each count before attempting delivery, so restarts don't repeat events.
func (s *Server) recordAgentCall(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, telemetry.NotificationTimeout)
	defer cancel()
	select {
	case s.agentActivityGate <- struct{}{}:
		defer func() { <-s.agentActivityGate }()
	case <-ctx.Done():
		return
	}
	if s.telemetry == nil || !s.telemetry.Enabled() {
		return
	}
	now := s.agentActivityNow
	if now == nil {
		now = time.Now
	}
	calls, err := s.db.RecordAgentCall(ctx, now().UTC().Format(time.DateOnly))
	if err != nil {
		return
	}
	var event, bucket string
	switch calls {
	case 1:
		event, bucket = telemetry.EventAgentActive, "1-10"
	case 11:
		event, bucket = telemetry.EventAgentCallCount, "11-100"
	case 101:
		event, bucket = telemetry.EventAgentCallCount, "over-100"
	default:
		return
	}
	_ = s.telemetry.Capture(event, map[string]any{telemetry.PropertyCallCountBucket: bucket})
}
