package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDaemonRunValidatesHookEnabledAutoDesignHeuristics(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", tmp)

	configPath := filepath.Join(tmp, "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(`[auto_design_review]
hook_enabled = true
trigger_paths = ["["]
`), 0o644))

	cmd := daemonRunCmd()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{
		"--db", filepath.Join(tmp, "reviews.db"),
		"--config", configPath,
		"--addr", "127.0.0.1:0",
	})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid [auto_design_review] config")
	assert.Contains(t, err.Error(), "trigger_paths")
}
