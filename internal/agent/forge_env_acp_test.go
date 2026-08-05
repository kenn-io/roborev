package agent

import (
	"context"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// echoTerminalEnvScript prints the forge credentials and one caller-supplied
// variable, so a real terminal launch can be inspected end to end.
const echoTerminalEnvScript = `#!/bin/sh
echo "gitlab=[$GITLAB_TOKEN] gh=[$GH_TOKEN] github=[$GITHUB_TOKEN] cijob=[$CI_JOB_TOKEN]"
echo "extra=[$ACP_EXTRA] gac=[$GOOGLE_APPLICATION_CREDENTIALS]"
`

// newTerminalTestClient builds an acpClient permitted to create terminals.
// Terminal creation is gated on auto-approve mode, so both mode fields have to
// agree for CreateTerminal to get past its authorization check.
func newTerminalTestClient(t *testing.T, repoRoot string) *acpClient {
	t.Helper()
	agent := &ACPAgent{
		Mode:            "yolo",
		AutoApproveMode: "yolo",
		ReadOnlyMode:    "plan",
	}
	return &acpClient{
		agent:     agent,
		sessionID: "session-1",
		repoRoot:  repoRoot,
		terminals: make(map[string]*acpTerminal),
	}
}

// TestACPCreateTerminalStripsForgeCredentials covers the ACP terminal launch
// path, which forge_env.go singles out as the most direct exfiltration channel
// and which has no withGitHubCredentials() opt-out. The subtests run with and
// without params.Env: the empty case is the one a regression that skips the
// cmd.Env assignment would slip through.
func TestACPCreateTerminalStripsForgeCredentials(t *testing.T) {
	skipIfWindows(t)

	tests := []struct {
		name  string
		env   []acp.EnvVariable
		wants []string
	}{
		{
			name:  "NoCallerEnv",
			wants: []string{"extra=[]"},
		},
		{
			name:  "WithCallerEnv",
			env:   []acp.EnvVariable{{Name: "ACP_EXTRA", Value: "kept"}},
			wants: []string{"extra=[kept]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			forgeCredentialTestEnv(t)
			cmdPath := writeTempCommand(t, echoTerminalEnvScript)
			client := newTerminalTestClient(t, t.TempDir())

			created, err := client.CreateTerminal(context.Background(),
				acp.CreateTerminalRequest{
					SessionId: acp.SessionId("session-1"),
					Command:   cmdPath,
					Env:       tt.env,
				})
			require.NoError(t, err)

			_, err = client.WaitForTerminalExit(context.Background(),
				acp.WaitForTerminalExitRequest{
					SessionId:  acp.SessionId("session-1"),
					TerminalId: created.TerminalId,
				})
			require.NoError(t, err)

			out, err := client.TerminalOutput(context.Background(),
				acp.TerminalOutputRequest{
					SessionId:  acp.SessionId("session-1"),
					TerminalId: created.TerminalId,
				})
			require.NoError(t, err)

			// Every forge credential must read back empty. Comparing the
			// rendered "key=[]" form catches a value leaking through.
			for _, want := range []string{
				"gitlab=[]", "gh=[]", "github=[]", "cijob=[]",
			} {
				assert.Contains(out.Output, want,
					"terminal must not see a forge credential")
			}
			// The provider credential survives: an agent that cannot reach its
			// model cannot review anything.
			assert.Contains(out.Output, "gac=[/run/secrets/sa.json]")
			for _, want := range tt.wants {
				assert.Contains(out.Output, want)
			}
		})
	}
}

func TestRemovedEnvKeysReportsNamesOnly(t *testing.T) {
	assert := assert.New(t)

	before := []string{
		"PATH=/bin",
		"GITLAB_TOKEN=glpat-secret",
		"GH_TOKEN=gh-secret",
	}
	removed := removedEnvKeys(before, StripForgeCredentials(before))

	assert.ElementsMatch([]string{"GITLAB_TOKEN", "GH_TOKEN"}, removed)
	// Values must never appear in something destined for a log line.
	for _, entry := range removed {
		assert.NotContains(entry, "secret")
		assert.NotContains(entry, "=")
	}
}

func TestRemovedEnvKeysEmptyWhenNothingStripped(t *testing.T) {
	before := []string{"PATH=/bin", "HOME=/root"}
	assert.Empty(t, removedEnvKeys(before, StripForgeCredentials(before)))
}

func TestStripUntrustedEnvLoggedStripsAndReturns(t *testing.T) {
	assert := assert.New(t)

	env := []string{"PATH=/bin", "GITLAB_TOKEN=glpat-secret"}
	got := StripUntrustedEnvLogged(env, "test site")

	assert.Equal([]string{"PATH=/bin"}, got)
	// The input slice must not be mutated: callers pass cmd.Environ() and
	// still hold it.
	assert.Contains(strings.Join(env, " "), "GITLAB_TOKEN=glpat-secret")
}
