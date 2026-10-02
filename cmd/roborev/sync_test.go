package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func TestSyncStatusUsesDaemon(t *testing.T) {
	machineID := uuid.New()
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/sync/status", r.URL.Path)
		assert.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		response := daemon.SyncStatusOutput{}
		response.Body.Enabled = true
		response.Body.Message = "waiting for connection"
		response.Body.Interval = "7m"
		response.Body.MachineName = "test-machine"
		response.Body.MachineID = &machineID
		response.Body.Warnings = []string{"configuration warning"}
		response.Body.PendingPush = daemon.SyncPendingCounts{Jobs: 1000, Reviews: 2, Comments: 3}
		response.Body.PendingLimit = 1000
		response.Body.PendingIncomplete = true
		assert.NoError(t, json.MarshalWrite(w, response.Body))
	}))
	var out bytes.Buffer
	command := syncStatusCmd()
	command.SetOut(&out)
	command.SetArgs([]string{})
	require.NoError(t, command.Execute())
	assert.Equal(t, "Sync: enabled\nInterval: 7m\nMachine name: test-machine\nWarning: configuration warning\nMachine ID: "+machineID.String()+"\n\nWarning: could not count all pending items\nPending push: >=1000 jobs, 2 reviews, 3 comments\n\nPostgreSQL: disconnected\nwaiting for connection\n", out.String())
}

func TestSyncStatusDisabled(t *testing.T) {
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		response := daemon.SyncStatusOutput{}
		response.Body.Message = "sync not enabled"
		assert.NoError(t, json.MarshalWrite(w, response.Body))
	}))
	var out bytes.Buffer
	command := syncStatusCmd()
	command.SetOut(&out)
	command.SetArgs([]string{})
	require.NoError(t, command.Execute())
	assert.Equal(t, "Sync: disabled\n\nEnable in ~/.roborev/config.toml:\n  [sync]\n  enabled = true\n  postgres_url = \"postgres://...\"\n", out.String())
}

func TestSyncStatusReportsDaemonProblemDetail(t *testing.T) {
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, err := w.Write([]byte(`{"status":500,"title":"Internal Server Error","detail":"could not read pending sync work"}`))
		assert.NoError(t, err)
	}))
	command := syncStatusCmd()
	command.SetArgs([]string{})
	err := command.Execute()
	require.Error(t, err)
	assert.ErrorContains(t, err, "could not read pending sync work")
}
