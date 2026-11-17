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
	"go.kenn.io/roborev/internal/testutil"
)

// TestProcessJob_DroidAllowanceExhaustionFailsOverOnFirstAttempt runs a droid
// shim through droid_cmd. The shim records its argv and fails with Factory's
// subscription-allowance wording, so the job must use the configured model,
// cool droid down, and move to the backup agent without retrying droid.
func TestProcessJob_DroidAllowanceExhaustionFailsOverOnFirstAttempt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("droid shim is a POSIX shell script")
	}
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	shim := filepath.Join(dir, "droid-wrapper")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + argvLog + "'\n" +
		"cat >/dev/null\n" +
		"echo 'Error during droid execution: 403 Your subscription allowance for this period is used up, " +
		"and your plan does not allow spending wallet balance on top of it.' >&2\n" +
		"exit 1\n"
	require.NoError(t, os.WriteFile(shim, []byte(script), 0o755))

	cfg := config.DefaultConfig()
	cfg.DroidCmd = shim
	cfg.DefaultBackupAgent = "test"
	tc.reconfigurePool(cfg)

	job := tc.createJobWithAgent(t, sha, "droid")
	_, err := tc.DB.ExecContext(t.Context(),
		`UPDATE review_jobs SET model = ? WHERE id = ?`, "custom:Primary-Model-0", job.ID)
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)

	tc.Pool.processJob(testWorkerID, claimed)

	updated := tc.assertJobStatus(t, claimed.ID, storage.JobStatusQueued)
	assert := assert.New(t)
	assert.Equal("test", updated.Agent)
	assert.True(tc.Pool.isAgentCoolingDown("droid"))

	argv, err := os.ReadFile(argvLog)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
	require.Len(t, lines, 1, "droid should run once, not retry")
	assert.Contains(lines[0], "-m custom:Primary-Model-0")
}
