package daemon

import (
	"context"
	"time"

	"go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/roborev/internal/telemetry"
)

// recordAgentCall persists each count before attempting delivery, so restarts don't repeat events.
// A client retry repeats its callID, and the stored IDs count that call once per day.
func (s *Server) recordAgentCall(ctx context.Context, callID string) (posthog.Status, error) {
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
	day := now().UTC().Format(time.DateOnly)
	if s.agentActivityFullDay == day {
		return posthog.StatusSkipped, nil
	}
	calls, repeated, err := s.db.RecordAgentCall(ctx, day, callID)
	if err != nil {
		return "", err
	}
	if repeated {
		return posthog.StatusSkipped, nil
	}
	var event, bucket string
	switch calls {
	case 1:
		event, bucket = telemetry.EventAgentActive, "1-10"
	case 11:
		event, bucket = telemetry.EventAgentCallCount, "11-100"
	case 101:
		s.agentActivityFullDay = day
		event, bucket = telemetry.EventAgentCallCount, telemetry.CountOver100
	default:
		if calls == 0 {
			s.agentActivityFullDay = day
		}
		return posthog.StatusSkipped, nil
	}
	// The count is saved, so a failed capture must not invite a retry that counts the call again.
	if s.telemetry.Capture(event, map[string]any{telemetry.PropertyCallCountBucket: bucket}) != nil {
		return posthog.StatusSkipped, nil
	}
	return posthog.StatusQueued, nil
}
