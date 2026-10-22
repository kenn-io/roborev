package daemon

import (
	"context"
	"time"

	"go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/roborev/internal/telemetry"
)

// recordAgentCall persists each count before attempting delivery, so restarts don't repeat events.
func (s *Server) recordAgentCall(ctx context.Context) (posthog.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, telemetry.NotificationTimeout)
	defer cancel()
	select {
	case s.agentActivityGate <- struct{}{}:
		defer func() { <-s.agentActivityGate }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if s.telemetry == nil || !s.telemetry.Enabled() {
		return posthog.StatusDisabled, nil
	}
	now := s.agentActivityNow
	if now == nil {
		now = time.Now
	}
	calls, err := s.db.RecordAgentCall(ctx, now().UTC().Format(time.DateOnly))
	if err != nil {
		return "", err
	}
	var event, bucket string
	switch calls {
	case 1:
		event, bucket = telemetry.EventAgentActive, "1-10"
	case 11:
		event, bucket = telemetry.EventAgentCallCount, "11-100"
	case 101:
		event, bucket = telemetry.EventAgentCallCount, telemetry.CountOver100
	default:
		return posthog.StatusSkipped, nil
	}
	if err := s.telemetry.Capture(event, map[string]any{telemetry.PropertyCallCountBucket: bucket}); err != nil {
		return "", err
	}
	return posthog.StatusQueued, nil
}
