package daemon

import (
	"context"
	"encoding/json"
	"time"

	"go.kenn.io/roborev/internal/telemetry"
)

const agentActivityKey = "telemetry.agent_activity"

type agentActivityState struct {
	Day   string `json:"day"`
	Calls uint64 `json:"calls"`
}

// recordAgentCall persists each count before attempting delivery, so restarts don't repeat events.
func (s *Server) recordAgentCall(_ context.Context) {
	s.agentActivityMu.Lock()
	defer s.agentActivityMu.Unlock()
	if s.telemetry == nil || !s.telemetry.Enabled() {
		return
	}
	stored, err := s.db.GetSyncState(agentActivityKey)
	if err != nil {
		return
	}
	var state agentActivityState
	if stored != "" && json.Unmarshal([]byte(stored), &state) != nil {
		return
	}
	now := s.agentActivityNow
	if now == nil {
		now = time.Now
	}
	day := now().UTC().Format(time.DateOnly)
	if state.Day != day {
		state = agentActivityState{Day: day}
	}
	state.Calls++
	value, err := json.Marshal(state)
	if err != nil || s.db.SetSyncState(agentActivityKey, string(value)) != nil {
		return
	}
	var event, bucket string
	switch state.Calls {
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
