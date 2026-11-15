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

func TestWorkerClearsUnsupportedGeminiSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("synthetic Gemini CLIs use a POSIX shell")
	}
	tests := []struct {
		name, command, version, output, resumeFlag, wantSessionID, wantSessionCLI string
	}{
		{
			name:    "Gemini CLI changed",
			command: "gemini", version: "0.34.0", resumeFlag: "--resume",
			output: `{"type":"init","session_id":"new-gemini-session"}
{"type":"message","role":"assistant","content":"{\"schema_version\":3,\"summary\":\"No findings.\",\"verdict\":\"pass\",\"findings\":[]}","delta":true}
{"type":"result","status":"success"}
`,
			wantSessionID: "new-gemini-session", wantSessionCLI: "gemini",
		},
		{
			name:    "Antigravity resume unsupported",
			command: "agy", version: "1.1.7", resumeFlag: "--conversation",
			output: `{"schema_version":3,"summary":"No findings.","verdict":"pass","findings":[]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			worker := newWorkerTestContext(t, 1)
			argsFile := filepath.Join(t.TempDir(), "args")
			outputFile := filepath.Join(t.TempDir(), "output")
			require.NoError(t, os.WriteFile(outputFile, []byte(tc.output), 0o600))
			commandPath := filepath.Join(t.TempDir(), tc.command)
			script := `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
if [ "$1" = "--version" ]; then echo "$CLI_VERSION"; exit 0; fi
printf '%s\n' "$@" > "$ARGS_FILE"
cat "$OUTPUT_FILE"
`
			require.NoError(t, os.WriteFile(commandPath, []byte(script), 0o755))
			t.Setenv("CLI_VERSION", tc.version)
			t.Setenv("ARGS_FILE", argsFile)
			t.Setenv("OUTPUT_FILE", outputFile)
			cfg := config.DefaultConfig()
			cfg.GeminiCmd = commandPath
			worker.reconfigurePool(cfg)

			job := worker.createJobWithAgent(t, worker.GitRepo.HeadSHA(), "gemini")
			_, err := worker.DB.ExecContext(t.Context(), `
				UPDATE review_jobs
				SET session_id = 'old-agy-session', session_cli = 'agy', session_resumed = 1,
				    resume_source_job_uuid = '123e4567-e89b-12d3-a456-426614174000'
				WHERE id = ?`, job.ID)
			require.NoError(t, err)
			claimed, err := worker.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.Equal(t, job.ID, claimed.ID)

			worker.Pool.processJob(testWorkerID, claimed)

			got := worker.assertJobStatus(t, job.ID, storage.JobStatusDone)
			assert.Equal(t, tc.wantSessionID, got.SessionID)
			assert.Equal(t, tc.wantSessionCLI, got.SessionCLI)
			assert.Nil(t, got.ResumeSourceJobUUID)
			var resumed int
			require.NoError(t, worker.DB.QueryRowContext(t.Context(),
				`SELECT session_resumed FROM review_jobs WHERE id = ?`, job.ID,
			).Scan(&resumed))
			assert.Zero(t, resumed)

			args, err := os.ReadFile(argsFile)
			require.NoError(t, err)
			assert.NotContains(t, string(args), tc.resumeFlag)
		})
	}
}
