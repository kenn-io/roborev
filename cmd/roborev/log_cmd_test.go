package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
)

func TestLogCleanCmd_NegativeDays(t *testing.T) {
	cmd := logCleanCmd()
	cmd.SetArgs([]string{"--days", "-1"})
	err := cmd.Execute()
	require.Error(t, err, "expected error for negative --days")
}

func TestLogCleanCmd_OverflowDays(t *testing.T) {
	cmd := logCleanCmd()
	cmd.SetArgs([]string{"--days", "999999"})
	err := cmd.Execute()
	require.Error(t, err, "expected error for oversized --days")
}

func TestIsBrokenPipe(t *testing.T) {
	assert := assert.New(t)
	assert.False(isBrokenPipe(nil), "nil should not be broken pipe")
	assert.False(isBrokenPipe(fmt.Errorf("other error")), "non-EPIPE error should not be broken pipe")
	assert.True(isBrokenPipe(syscall.EPIPE), "bare EPIPE should be broken pipe")
	assert.True(isBrokenPipe(fmt.Errorf("write: %w", syscall.EPIPE)), "wrapped EPIPE should be broken pipe")
}

func TestLogCmd_InvalidJobID(t *testing.T) {
	assert := assert.New(t)

	cmd := logCmd()
	cmd.SetArgs([]string{"abc"})
	cmd.SilenceUsage = true
	err := cmd.Execute()
	require.Error(t, err, "expected error for non-numeric job ID")
	assert.Contains(err.Error(), "invalid job ID")
}

func TestLogCmd_MissingLogFile(t *testing.T) {
	assert := assert.New(t)

	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	cmd := logCmd()
	cmd.SetArgs([]string{"99999"})
	cmd.SilenceUsage = true
	err := cmd.Execute()
	require.Error(t, err, "expected error for missing log file")
	assert.Contains(err.Error(), "no log for job")
}

func TestLogCmd_PathFlag(t *testing.T) {
	assert := assert.New(t)

	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)

	var buf bytes.Buffer
	cmd := logCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--path", "42"})
	cmd.SilenceUsage = true
	// --path succeeds even if log file doesn't exist.
	err := cmd.Execute()
	require.NoError(t, err, "--path should succeed")

	out := strings.TrimSpace(buf.String())
	want := filepath.Join(dir, "logs", "jobs", "42.log")
	assert.Equal(want, out)
}

func TestLogCmd_RawFlag(t *testing.T) {
	assert := assert.New(t)

	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)

	// Create a log file at the expected path.
	logDir := filepath.Join(dir, "logs", "jobs")
	os.MkdirAll(logDir, 0o755)
	logPath := filepath.Join(logDir, "42.log")
	rawContent := `{"type":"assistant"}` + "\n"
	os.WriteFile(logPath, []byte(rawContent), 0o644)

	var buf bytes.Buffer
	cmd := logCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--raw", "42"})
	cmd.SilenceUsage = true
	err := cmd.Execute()
	require.NoError(t, err, "--raw should succeed")
	assert.Equal(rawContent, buf.String())
}

func TestLogCmdUsesExplicitDatabase(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	dbPath := filepath.Join(dataDir, "custom.db")
	db, err := storage.Open(dbPath)
	require.NoError(t, err)
	repo, err := db.GetOrCreateRepo(filepath.Join(dataDir, "repo"))
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "abc123", Agent: "grok",
	})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	require.NoError(t, os.MkdirAll(daemon.JobLogDir(), 0o700))
	const logContent = "custom database output\n"
	require.NoError(t, os.WriteFile(
		daemon.JobLogPath(job.ID), []byte(logContent), 0o600,
	))

	var out bytes.Buffer
	cmd := logCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--db", dbPath, fmt.Sprint(job.ID)})
	cmd.SilenceUsage = true
	require.NoError(t, cmd.Execute())
	assert.Equal(t, logContent, out.String())
}

func TestRenderJobLogUsesStoredIdentity(t *testing.T) {
	tests := []struct {
		name          string
		agent         string
		source        string
		recordedAgent string
		log           string
		want          []string
		notWant       []string
	}{
		{
			name: "provider log", agent: "grok",
			log: `{"type":"item.completed","item":{"type":"agent_message","text":"other provider output"}}` + "\n" +
				`{"type":"text","data":"formatted provider output"}` + "\n" + `{"type":"end"}` + "\n",
			want:    []string{"formatted provider output"},
			notWant: []string{"other provider output"},
		},
		{
			name: "mixed auto design log", agent: "grok",
			source: storage.JobSourceAutoDesign, recordedAgent: storage.AutoDesignAgentSentinel,
			log: `{"type":"system","subtype":"init","session_id":"classifier"}` + "\n" +
				`{"type":"item.completed","item":{"type":"agent_message","text":"classifier output"}}` + "\n" +
				`{"type":"text","data":"design output"}` + "\n" + `{"type":"end"}` + "\n",
			want: []string{"classifier output", "design output"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("ROBOREV_DATA_DIR", dataDir)
			db, err := storage.Open(storage.DefaultDBPath())
			require.NoError(t, err)
			repo, err := db.GetOrCreateRepo(filepath.Join(dataDir, "repo"))
			require.NoError(t, err)
			job, err := db.EnqueueJob(storage.EnqueueOpts{
				RepoID: repo.ID, GitRef: "abc123", Agent: tt.agent, Source: tt.source,
			})
			require.NoError(t, err)
			require.NoError(t, db.Close())

			require.NoError(t, os.MkdirAll(daemon.JobLogDir(), 0o700))
			require.NoError(t, os.WriteFile(daemon.JobLogPath(job.ID), []byte(tt.log), 0o600))
			if tt.recordedAgent != "" {
				require.NoError(t, daemon.RecordJobLogAgent(job.ID, tt.recordedAgent))
			}
			var out bytes.Buffer
			require.NoError(t, renderJobLog(
				job.ID, &out, true, storage.DefaultDBPath(),
			))
			plain := streamfmt.StripANSI(out.String())
			for _, want := range tt.want {
				assert.Contains(t, plain, want)
			}
			for _, notWant := range tt.notWant {
				assert.NotContains(t, plain, notWant)
			}
			assert.NotContains(t, plain, `"type"`)
		})
	}
}

// If a queued failover is canceled before the backup starts, the existing log
// still belongs to the prior provider even though the job row names the backup.
func TestRenderJobLogUsesPersistedLogIdentityAfterCanceledFailover(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	db, err := storage.Open(storage.DefaultDBPath())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo, err := db.GetOrCreateRepo(filepath.Join(dataDir, "repo"))
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "abc123", Agent: "codex", Source: storage.JobSourceAutoDesign,
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	require.NoError(t, os.MkdirAll(daemon.JobLogDir(), 0o700))
	require.NoError(t, os.WriteFile(
		daemon.JobLogPath(job.ID),
		[]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"prior provider output"}}`+"\n"),
		0o600,
	))
	require.NoError(t, daemon.RecordJobLogAgent(job.ID, "codex"))
	failedOver, err := db.FailoverJob(job.ID, "worker-1", "grok", "")
	require.NoError(t, err)
	require.True(t, failedOver)
	require.NoError(t, db.CancelJob(job.ID))

	var out bytes.Buffer
	require.NoError(t, renderJobLog(
		job.ID, &out, true, storage.DefaultDBPath(),
	))
	plain := streamfmt.StripANSI(out.String())
	assert.Contains(t, plain, "prior provider output")
	assert.NotContains(t, plain, `"type"`)
}

func TestRenderJobLogOrphanSuggestsRaw(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.MkdirAll(daemon.JobLogDir(), 0o700))
	require.NoError(t, os.WriteFile(
		daemon.JobLogPath(42), []byte(`{"type":"assistant"}`+"\n"), 0o600,
	))

	err := renderJobLog(42, io.Discard, true, storage.DefaultDBPath())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--raw")
}

func TestLooksLikeJSON(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{`{"type":"assistant"}`, true},
		{`  {"type":"tool"}`, true},
		{`not json`, false},
		{``, false},
		{`[1,2,3]`, false},
		{`{invalid}`, false},
		// Valid JSON but no "type" field — should NOT match
		{`{"foo":"bar"}`, false},
		// Empty type — should NOT match
		{`{"type":""}`, false},
	}
	for _, tt := range tests {
		assert := assert.New(t)
		got := streamfmt.LooksLikeJSON(tt.input)
		assert.Equal(tt.want, got, "streamfmt.LooksLikeJSON(%q)", tt.input)
	}
}
