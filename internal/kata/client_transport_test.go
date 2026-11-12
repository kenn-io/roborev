package kata

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestKataTransportChild is invoked as a synthetic Kata executable by the
// transport test. It exits before accessing any configuration or network.
func TestKataTransportChild(t *testing.T) {
	if os.Getenv("ROBOREV_KATA_TRANSPORT_CHILD") != "1" {
		return
	}
	fmt.Fprint(os.Stdout, os.Getenv("ROBOREV_KATA_TRANSPORT_STDOUT"))
	fmt.Fprint(os.Stderr, os.Getenv("ROBOREV_KATA_TRANSPORT_STDERR"))
	os.Exit(1)
}

func TestRealRunErrorTransport(t *testing.T) {
	binary, err := os.Executable()
	require.NoError(t, err)
	for _, tc := range []struct {
		name, stderr, stdout string
		want                 error
	}{
		{name: "docbank", stderr: `{"error":{"code":"docbank_not_configured"}}`, want: ErrDocbankNotConfigured},
		{name: "daemon", stderr: `{"error":{"code":"daemon_unavailable"}}`, want: ErrDaemonUnavailable},
		{name: "warning prefix", stderr: "kata: ignoring invalid KATA_HTTP_TIMEOUT=\"30\" (expected a Go duration like 30s or 2m); using default 30s\n" + `{"error":{"code":"docbank_not_configured"}}` + "\n", want: ErrDocbankNotConfigured},
		{name: "malformed", stderr: `not json`},
		{name: "unknown", stderr: `{"error":{"code":"unknown"}}`},
		{name: "stdout error", stdout: `{"error":{"code":"docbank_not_configured"}}`, stderr: "failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := NewCLIClient(dir)
			c.bin = binary
			c.env = []string{"ROBOREV_KATA_TRANSPORT_CHILD=1", "ROBOREV_KATA_TRANSPORT_STDERR=" + tc.stderr, "ROBOREV_KATA_TRANSPORT_STDOUT=" + tc.stdout, "HOME=" + dir, "USERPROFILE=" + dir, "KATA_HOME=" + dir, "XDG_CONFIG_HOME=" + dir, "CODEX_HOME=", "CLAUDE_CONFIG_DIR=", "PI_CODING_AGENT_DIR="}
			out, err := c.run(t.Context(), c, []string{"-test.run=^TestKataTransportChild$"}, nil)
			require.Error(t, err)
			assert.Nil(t, out)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			} else {
				require.ErrorContains(t, err, "exit 1")
				require.ErrorContains(t, err, tc.stderr)
				require.NotErrorIs(t, err, ErrDocbankNotConfigured)
				require.NotErrorIs(t, err, ErrDaemonUnavailable)
			}
		})
	}
}
