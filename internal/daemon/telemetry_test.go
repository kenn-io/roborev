package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

type fakeTelemetryClient struct {
	enabled    bool
	events     []string
	properties []map[string]any
}

func (f *fakeTelemetryClient) Capture(event string, properties map[string]any) error {
	f.events = append(f.events, event)
	f.properties = append(f.properties, properties)
	return nil
}

func (f *fakeTelemetryClient) Close() error { return nil }

func (f *fakeTelemetryClient) Enabled() bool { return f.enabled }

func TestCaptureDaemonStartedTelemetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server, db, tmpDir := newTestServer(t)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)

	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:  repo.ID,
		GitRef:  "abc123",
		Agent:   "codex",
		JobType: storage.JobTypeReview,
	})
	require.NoError(err)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'running' WHERE id = ?`, job.ID)
	require.NoError(err)
	require.NoError(db.CompleteJob(job.ID, "codex", "prompt", "No issues found."))

	cfg := config.DefaultConfig()
	cfg.MaxWorkers = 4
	cfg.Sync.Enabled = true
	cfg.CI.Enabled = true
	cfg.AutoDesignReview.Enabled = true

	server.captureDaemonStartedTelemetry(cfg)

	require.Len(client.events, 1)
	require.Len(client.properties, 1)
	assert.Equal("daemon_started", client.events[0])
	assert.Equal(1, client.properties[0]["repo_count"])
	assert.Equal(1, client.properties[0]["review_count"])
	assert.Equal(true, client.properties[0]["sync_enabled"])
	assert.Equal(true, client.properties[0]["ci_enabled"])
	assert.Equal(true, client.properties[0]["auto_design_enabled"])
}

func TestStartDailyTelemetryLoopCapturesImmediately(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server, db, tmpDir := newTestServer(t)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)

	_, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server.startDailyTelemetryLoop(ctx, config.DefaultConfig())

	require.Len(client.events, 1)
	require.Len(client.properties, 1)
	assert.Equal("daemon_active", client.events[0])
	assert.Equal(1, client.properties[0]["repo_count"])
}

func TestCaptureDaemonStartedTelemetryDisabledNoops(t *testing.T) {
	assert := assert.New(t)

	server, _, _ := newTestServer(t)
	client := &fakeTelemetryClient{enabled: false}
	server.SetTelemetry(client)

	server.captureDaemonStartedTelemetry(config.DefaultConfig())

	assert.Empty(client.events)
	assert.Nil(client.properties)
}
