package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func TestWorkerStartsFreshWhenGeminiCLIChanges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("synthetic Gemini CLI uses a POSIX shell")
	}
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	argsFile := filepath.Join(t.TempDir(), "args")
	commandPath := filepath.Join(t.TempDir(), "gemini")
	require.NoError(t, os.WriteFile(commandPath, []byte(`#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
if [ "$1" = "--version" ]; then echo "0.34.0"; exit 0; fi
printf '%s\n' "$@" > "$ARGS_FILE"
cat > /dev/null
echo '{"type":"init","session_id":"new-gemini-session"}'
echo '{"type":"message","role":"assistant","content":"{\"schema_version\":3,\"summary\":\"No findings.\",\"verdict\":\"pass\",\"findings\":[]}","delta":true}'
echo '{"type":"result","status":"success"}'
`), 0o755))
	t.Setenv("ARGS_FILE", argsFile)
	cfg := config.DefaultConfig()
	cfg.GeminiCmd = commandPath
	tc.reconfigurePool(cfg)

	job := tc.createJobWithAgent(t, tc.GitRepo.HeadSHA(), "gemini")
	_, err := tc.DB.ExecContext(t.Context(), `
		UPDATE review_jobs
		SET session_id = 'old-agy-session', session_cli = 'agy', session_resumed = 1,
		    resume_source_job_uuid = '123e4567-e89b-12d3-a456-426614174000'
		WHERE id = ?`, job.ID)
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	tc.Pool.processJob(testWorkerID, claimed)

	got := tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	assert.Equal(t, "new-gemini-session", got.SessionID)
	assert.Equal(t, "gemini", got.SessionCLI)
	assert.Nil(t, got.ResumeSourceJobUUID)
	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.NotContains(t, string(args), "--resume")
}
