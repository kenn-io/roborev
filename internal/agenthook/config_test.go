package agenthook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveOptionsUsesDefaultsWithoutConfig(t *testing.T) {
	clearAgentHookEnv(t)
	opts, err := ResolveOptions(Options{ConfigPath: filepath.Join(t.TempDir(), "missing.toml")}, map[string]bool{"config": true})

	require.NoError(t, err)
	assert.Equal(t, DefaultTurnThreshold, opts.TurnThreshold)
	assert.Equal(t, DefaultCommitThreshold, opts.CommitThreshold)
	assert.Equal(t, DefaultFailedReviewThreshold, opts.FailedReviewThreshold)
	assert.Equal(t, DefaultInstruction, opts.Instruction)
}

func TestResolveOptionsUsesGlobalAgentHookConfig(t *testing.T) {
	clearAgentHookEnv(t)
	path := writeAgentHookConfig(t, `
[agent_hook]
turn_threshold = 6
commit_threshold = 2
failed_review_threshold = 3
instruction = "Run roborev fix."
`)

	opts, err := ResolveOptions(Options{ConfigPath: path}, map[string]bool{"config": true})

	require.NoError(t, err)
	assert.Equal(t, 6, opts.TurnThreshold)
	assert.Equal(t, 2, opts.CommitThreshold)
	assert.Equal(t, 3, opts.FailedReviewThreshold)
	assert.Equal(t, "Run roborev fix.", opts.Instruction)
}

func TestResolveOptionsAllowsZeroTurnThresholdFromConfig(t *testing.T) {
	clearAgentHookEnv(t)
	path := writeAgentHookConfig(t, `
[agent_hook]
turn_threshold = 0
`)

	opts, err := ResolveOptions(Options{ConfigPath: path}, map[string]bool{"config": true})

	require.NoError(t, err)
	assert.Equal(t, 0, opts.TurnThreshold)
}

func TestResolveOptionsEnvOverridesGlobalConfig(t *testing.T) {
	path := writeAgentHookConfig(t, `
[agent_hook]
turn_threshold = 6
commit_threshold = 2
failed_review_threshold = 3
instruction = "config instruction"
`)
	t.Setenv(TurnThresholdEnv, "7")
	t.Setenv(CommitThresholdEnv, "4")
	t.Setenv(FailedReviewThresholdEnv, "5")
	t.Setenv(InstructionEnv, "env instruction")
	t.Setenv(RoborevServerEnv, "127.0.0.1:9999")

	opts, err := ResolveOptions(Options{ConfigPath: path}, map[string]bool{"config": true})

	require.NoError(t, err)
	assert.Equal(t, 7, opts.TurnThreshold)
	assert.Equal(t, 4, opts.CommitThreshold)
	assert.Equal(t, 5, opts.FailedReviewThreshold)
	assert.Equal(t, "env instruction", opts.Instruction)
	assert.Equal(t, "127.0.0.1:9999", opts.RoborevServerAddr)
}

func TestResolveOptionsFlagsOverrideEnv(t *testing.T) {
	path := writeAgentHookConfig(t, `
[agent_hook]
turn_threshold = 6
commit_threshold = 2
failed_review_threshold = 3
instruction = "config instruction"
`)
	t.Setenv(TurnThresholdEnv, "7")
	t.Setenv(CommitThresholdEnv, "4")
	t.Setenv(FailedReviewThresholdEnv, "5")
	t.Setenv(InstructionEnv, "env instruction")

	opts, err := ResolveOptions(Options{
		ConfigPath:            path,
		TurnThreshold:         8,
		CommitThreshold:       9,
		FailedReviewThreshold: 10,
		Instruction:           "flag instruction",
		RoborevServerAddr:     "127.0.0.1:7777",
	}, map[string]bool{
		"config":                  true,
		"turn-threshold":          true,
		"commit-threshold":        true,
		"failed-review-threshold": true,
		"instruction":             true,
		"roborev-server":          true,
	})

	require.NoError(t, err)
	assert.Equal(t, 8, opts.TurnThreshold)
	assert.Equal(t, 9, opts.CommitThreshold)
	assert.Equal(t, 10, opts.FailedReviewThreshold)
	assert.Equal(t, "flag instruction", opts.Instruction)
	assert.Equal(t, "127.0.0.1:7777", opts.RoborevServerAddr)
}

func TestResolveOptionsRejectsNegativeThresholds(t *testing.T) {
	clearAgentHookEnv(t)
	for _, tc := range []struct {
		name    string
		opts    Options
		changed string
		want    string
	}{
		{name: "turn", opts: Options{TurnThreshold: -1}, changed: "turn-threshold", want: "turn threshold must be >= 0"},
		{name: "commit", opts: Options{CommitThreshold: -1}, changed: "commit-threshold", want: "commit threshold must be >= 0"},
		{name: "review", opts: Options{FailedReviewThreshold: -1}, changed: "failed-review-threshold", want: "failed review threshold must be >= 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.ConfigPath = filepath.Join(t.TempDir(), "missing.toml")
			_, err := ResolveOptions(tc.opts, map[string]bool{"config": true, tc.changed: true})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestResolveOptionsIgnoresSpikeJSONAndLegacyEnvAliases(t *testing.T) {
	clearAgentHookEnv(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-hook.json"), []byte(`{"turn_threshold":99}`), 0o600))
	t.Setenv("ROBOREV_HOOK_TURN_THRESHOLD", "99")

	opts, err := ResolveOptions(Options{ConfigPath: filepath.Join(dir, "config.toml")}, map[string]bool{"config": true})

	require.NoError(t, err)
	assert.Equal(t, DefaultTurnThreshold, opts.TurnThreshold)
}

func writeAgentHookConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func clearAgentHookEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		TurnThresholdEnv,
		CommitThresholdEnv,
		FailedReviewThresholdEnv,
		InstructionEnv,
		RoborevServerEnv,
		DaemonAddrEnv,
	} {
		t.Setenv(name, "")
	}
}
