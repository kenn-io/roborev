package storage

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDaemonStatusIncludesUpdateDrainState(t *testing.T) {
	status := DaemonStatus{
		UpdateDraining:       true,
		UpdateDrainPolicy:    "interrupt",
		UpdateDrainExpiresAt: "2026-08-17T12:00:00Z",
	}

	encoded, err := json.Marshal(status)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"active_snoozes": null,
		"version": "",
		"queued_jobs": 0,
		"running_jobs": 0,
		"completed_jobs": 0,
		"failed_jobs": 0,
		"canceled_jobs": 0,
		"applied_jobs": 0,
		"rebased_jobs": 0,
		"skipped_jobs": 0,
		"active_workers": 0,
		"max_workers": 0,
		"queue_paused": false,
		"web_capabilities": null,
		"update_draining": true,
		"update_drain_policy": "interrupt",
		"update_drain_expires_at": "2026-08-17T12:00:00Z"
	}`, string(encoded))
}
