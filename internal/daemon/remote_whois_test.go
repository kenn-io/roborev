package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func whoisJSON(capEntries string) string {
	return `{"Node":{"Name":"laptop.example-tailnet.ts.net.","Tags":["tag:dev"]},` +
		`"UserProfile":{"LoginName":"user-a@example.com"},` +
		`"CapMap":{"kenn.io/cap/roborev":[` + capEntries + `]}}`
}

func TestParseWhoisAccessLevels(t *testing.T) {
	tests := []struct {
		name    string
		entries string
		want    RemoteAccess
	}{
		{"read", `{"access":"read"}`, RemoteAccessRead},
		{"queue", `{"access":"queue"}`, RemoteAccessQueue},
		{"highest wins", `{"access":"read"},{"access":"queue"}`, RemoteAccessQueue},
		{"unknown value", `{"access":"admin"}`, RemoteAccessNone},
		{"malformed entry", `"queue"`, RemoteAccessNone},
		{"no entries", ``, RemoteAccessNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caller, err := parseWhois([]byte(whoisJSON(tt.entries)))
			require.NoError(t, err)
			assert.Equal(t, tt.want, caller.Access)
		})
	}
}

func TestParseWhoisIdentity(t *testing.T) {
	assert := assert.New(t)
	caller, err := parseWhois([]byte(whoisJSON(`{"access":"read"}`)))
	require.NoError(t, err)
	assert.Equal("laptop.example-tailnet.ts.net.", caller.Node)
	assert.Equal("user-a@example.com", caller.Login)
	assert.Equal([]string{"tag:dev"}, caller.Tags)
}

func TestParseWhoisRejectsGarbage(t *testing.T) {
	_, err := parseWhois([]byte("not json"))
	require.ErrorContains(t, err, "decode tailscale whois output")
}

func TestTailscaleWhoisRunsCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tailscale CLI is a shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "tailscale")
	body := "#!/bin/sh\n[ \"$1 $2 $3\" = \"whois --json 100.64.0.2:5555\" ] || { echo bad args >&2; exit 2; }\n" +
		"cat <<'EOF'\n" + whoisJSON(`{"access":"queue"}`) + "\nEOF\n"
	require.NoError(t, os.WriteFile(script, []byte(body), 0o755))

	caller, err := tailscaleWhois(script)(context.Background(), "100.64.0.2:5555")
	require.NoError(t, err)
	assert.Equal(t, RemoteAccessQueue, caller.Access)

	_, err = tailscaleWhois(script)(context.Background(), "100.64.0.9:1")
	require.EqualError(t, err, "tailscale whois failed", "stderr stays in the daemon log")
}

func TestTailscaleWhoisTimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tailscale CLI is a shell script")
	}
	script := filepath.Join(t.TempDir(), "tailscale")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755))
	orig := remoteWhoisTimeout
	remoteWhoisTimeout = 50 * time.Millisecond
	t.Cleanup(func() { remoteWhoisTimeout = orig })

	// A subprocess is outside what testing/synctest can observe, so this
	// test runs on wall-clock time with a shortened bound.
	start := time.Now()
	_, err := tailscaleWhois(script)(context.Background(), "100.64.0.2:5555")
	require.EqualError(t, err, "tailscale whois timed out")
	assert.Less(t, time.Since(start), 20*time.Second, "the bound, not the script, ended the run")
}
