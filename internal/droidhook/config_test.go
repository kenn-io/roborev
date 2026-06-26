package droidhook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agenthook"
)

// absentConfigPath returns a temp path that does not exist so resolution tests
// are isolated from the developer's real ~/.roborev/config.toml.
func absentConfigPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "absent.toml")
}

func TestDefaultOptions(t *testing.T) {
	opts := DefaultOptions()
	assert.Equal(t, 5, opts.TurnThreshold)
	assert.Equal(t, 0, opts.CommitThreshold)
	assert.Equal(t, 4, opts.FailedReviewThreshold)
	assert.Equal(t, DefaultInstruction, opts.Instruction)
	assert.NotEmpty(t, opts.ConfigPath)
}

func TestResolveOptionsDefaults(t *testing.T) {
	cli := agenthook.Options{ConfigPath: absentConfigPath(t)}
	opts, err := ResolveOptions(cli, map[string]bool{"config": true})
	require.NoError(t, err)
	assert.Equal(t, 5, opts.TurnThreshold)
	assert.Equal(t, 0, opts.CommitThreshold)
	assert.Equal(t, 4, opts.FailedReviewThreshold)
	assert.Equal(t, DefaultInstruction, opts.Instruction)
}

func TestResolveOptionsEnvOverrides(t *testing.T) {
	t.Setenv(TurnThresholdEnv, "7")
	t.Setenv(CommitThresholdEnv, "2")
	t.Setenv(FailedReviewThresholdEnv, "9")
	t.Setenv(InstructionEnv, "env-instruction")

	cli := agenthook.Options{ConfigPath: absentConfigPath(t)}
	opts, err := ResolveOptions(cli, map[string]bool{"config": true})
	require.NoError(t, err)
	assert.Equal(t, 7, opts.TurnThreshold)
	assert.Equal(t, 2, opts.CommitThreshold)
	assert.Equal(t, 9, opts.FailedReviewThreshold)
	assert.Equal(t, "env-instruction", opts.Instruction)
}

func TestResolveOptionsConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[droid_hook]
turn_threshold = 6
commit_threshold = 2
failed_review_threshold = 3
instruction = "config-instruction"
`), 0o600))

	cli := agenthook.Options{ConfigPath: path}
	opts, err := ResolveOptions(cli, map[string]bool{"config": true})
	require.NoError(t, err)
	assert.Equal(t, 6, opts.TurnThreshold)
	assert.Equal(t, 2, opts.CommitThreshold)
	assert.Equal(t, 3, opts.FailedReviewThreshold)
	assert.Equal(t, "config-instruction", opts.Instruction)
}

func TestResolveOptionsCLIWinsOverEnvAndConfig(t *testing.T) {
	t.Setenv(TurnThresholdEnv, "7")
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[droid_hook]\nturn_threshold = 6\n"), 0o600))

	cli := agenthook.Options{ConfigPath: path, TurnThreshold: 11}
	opts, err := ResolveOptions(cli, map[string]bool{"config": true, "turn-threshold": true})
	require.NoError(t, err)
	assert.Equal(t, 11, opts.TurnThreshold)
}

func TestResolveOptionsRejectsNegativeThresholds(t *testing.T) {
	cases := []struct {
		name    string
		cli     agenthook.Options
		changed map[string]bool
	}{
		{"turn", agenthook.Options{TurnThreshold: -1}, map[string]bool{"turn-threshold": true}},
		{"commit", agenthook.Options{CommitThreshold: -1}, map[string]bool{"commit-threshold": true}},
		{"failed", agenthook.Options{FailedReviewThreshold: -1}, map[string]bool{"failed-review-threshold": true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.cli.ConfigPath = absentConfigPath(t)
			c.changed["config"] = true
			_, err := ResolveOptions(c.cli, c.changed)
			require.Error(t, err)
		})
	}
}
