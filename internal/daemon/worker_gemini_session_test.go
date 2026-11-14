package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	assert.Nil(t, got.ResumeSourceJobUUID)
	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.NotContains(t, string(args), "--resume")
}

func TestWorkerModelFallbackStoresSuccessfulGeminiSession(t *testing.T) {
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
printf '%s\n' "$@" >> "$ARGS_FILE"
has_model=0
for arg in "$@"; do
  if [ "$arg" = "-m" ]; then has_model=1; fi
done
if [ "$has_model" = "1" ]; then
  echo '{"type":"init","session_id":"failed-session"}'
  echo "Error: model is not found for API version v1" >&2
  exit 1
fi
echo '{"type":"init","session_id":"successful-session"}'
echo '{"type":"message","role":"assistant","content":"{\"schema_version\":3,\"summary\":\"No findings.\",\"verdict\":\"pass\",\"findings\":[]}","delta":true}'
echo '{"type":"result","status":"success"}'
`), 0o755))
	t.Setenv("ARGS_FILE", argsFile)
	cfg := config.DefaultConfig()
	cfg.GeminiCmd = commandPath
	tc.reconfigurePool(cfg)

	job := tc.createJobWithAgent(t, tc.GitRepo.HeadSHA(), "gemini")
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	tc.Pool.processJob(testWorkerID, claimed)

	got := tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	assert.Equal(t, "successful-session", got.SessionID)
	assert.Equal(t, "gemini", got.SessionCLI)
	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	argsText := string(args)
	assert.Equal(t, 1, strings.Count(argsText, "-m\n"))
	assert.Equal(t, 2, strings.Count(argsText, "--output-format\nstream-json\n"))
	assert.NotContains(t, argsText, "--resume")
}
