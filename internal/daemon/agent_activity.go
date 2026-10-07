package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"go.kenn.io/roborev/internal/telemetry"
)

const agentActivityKey = "telemetry.agent_activity"

type agentActivityState struct {
	Day   string `json:"day"`
	Calls uint64 `json:"calls"`
}

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
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	var stored string
	err = conn.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key = ?`, agentActivityKey).Scan(&stored)
	if err != nil && err != sql.ErrNoRows {
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
	if state.Calls >= 101 {
		return
	}
	state.Calls++
	value, err := json.Marshal(state)
	if err != nil {
		return
	}
	var busyTimeout int
	if conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout) != nil {
		return
	}
	deadline, _ := ctx.Deadline()
	// A contended SQLite writer ignored a one-second context; cap its busy wait at that deadline.
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", max(0, min(int(time.Until(deadline).Milliseconds()), busyTimeout)))); err != nil {
		return
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeout))
	}()
	_, err = conn.ExecContext(ctx, `INSERT INTO sync_state (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, agentActivityKey, string(value))
	if err != nil {
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
