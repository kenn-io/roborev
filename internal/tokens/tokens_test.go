package tokens

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatSummary(t *testing.T) {
	tests := []struct {
		name  string
		usage Usage
		want  string
	}{
		{"zero", Usage{}, ""},
		{
			"small counts",
			Usage{PeakContextTokens: 500, OutputTokens: 120},
			"500 ctx · 120 out",
		},
		{
			"thousands",
			Usage{PeakContextTokens: 45200, OutputTokens: 3900},
			"45.2k ctx · 3.9k out",
		},
		{
			"millions",
			Usage{PeakContextTokens: 2_500_000, OutputTokens: 15_000},
			"2.5M ctx · 15.0k out",
		},
		{
			"output only",
			Usage{OutputTokens: 800},
			"0 ctx · 800 out",
		},
		{
			"tokens and cost",
			Usage{
				PeakContextTokens: 118000, OutputTokens: 28800,
				CostUSD: 0.42, HasCost: true,
			},
			"118.0k ctx · 28.8k out · ~$0.42",
		},
		{
			"tokens unpriced",
			Usage{PeakContextTokens: 1000, OutputTokens: 200},
			"1.0k ctx · 200 out",
		},
		{
			"cost only",
			Usage{CostUSD: 0.05, HasCost: true},
			"~$0.05",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.usage.FormatSummary()
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFormatCost(t *testing.T) {
	assert.Equal(t, "~$0.42",
		Usage{CostUSD: 0.42, HasCost: true}.FormatCost())
	assert.Empty(t, Usage{}.FormatCost())
	assert.Empty(t, Usage{CostUSD: 0.42, HasCost: false}.FormatCost(),
		"cost suppressed when has_cost is false")
	assert.Equal(t, "~$0.00",
		Usage{CostUSD: 0, HasCost: true}.FormatCost(),
		"a genuine zero cost still renders")
}

func TestGeVersion(t *testing.T) {
	tests := []struct {
		a, b [3]int
		want bool
	}{
		{[3]int{0, 30, 0}, [3]int{0, 30, 0}, true},
		{[3]int{0, 30, 1}, [3]int{0, 30, 0}, true},
		{[3]int{0, 29, 9}, [3]int{0, 30, 0}, false},
		{[3]int{1, 0, 0}, [3]int{0, 30, 0}, true},
		{[3]int{0, 15, 0}, [3]int{0, 15, 0}, true},
		{[3]int{0, 14, 9}, [3]int{0, 15, 0}, false},
		{[3]int{2, 0, 0}, [3]int{1, 99, 99}, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%v_ge_%v", tt.a, tt.b), func(t *testing.T) {
			assert.Equal(t, tt.want, geVersion(tt.a, tt.b))
		})
	}
}

func TestParseJSON(t *testing.T) {
	t.Run("empty string", func(t *testing.T) {
		assert.Nil(t, ParseJSON(""))
	})

	t.Run("valid json", func(t *testing.T) {
		u := ParseJSON(
			`{"peak_context_tokens":1000,"total_output_tokens":200}`,
		)
		require.NotNil(t, u)
		assert.Equal(t, int64(1000), u.PeakContextTokens)
		assert.Equal(t, int64(200), u.OutputTokens)
	})

	t.Run("with cost", func(t *testing.T) {
		u := ParseJSON(
			`{"peak_context_tokens":1000,"total_output_tokens":200,` +
				`"cost_usd":0.42,"has_cost":true}`,
		)
		require.NotNil(t, u)
		assert.True(t, u.HasCost)
		assert.InDelta(t, 0.42, u.CostUSD, 1e-9)
	})

	t.Run("cost only no tokens", func(t *testing.T) {
		u := ParseJSON(`{"cost_usd":0.05,"has_cost":true}`)
		require.NotNil(t, u)
		assert.True(t, u.HasCost)
		assert.InDelta(t, 0.05, u.CostUSD, 1e-9)
	})

	t.Run("all zeros", func(t *testing.T) {
		assert.Nil(t, ParseJSON(
			`{"peak_context_tokens":0,"total_output_tokens":0}`,
		))
	})

	t.Run("invalid json", func(t *testing.T) {
		assert.Nil(t, ParseJSON(`{invalid`))
	})
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantLevel  capability
		wantParsed bool
	}{
		{
			"below token-use min",
			"agentsview v0.14.9 (commit abc, built 2026-01-01)",
			capNone, true,
		},
		{
			"token-use min",
			"agentsview v0.15.0 (commit abc, built 2026-01-01)",
			capTokenUse, true,
		},
		{
			"token-use range",
			"agentsview v0.29.0 (commit abc, built 2026-05-10)",
			capTokenUse, true,
		},
		{
			"session usage min",
			"agentsview v0.30.0 (commit abc, built 2026-05-23)",
			capSessionUsage, true,
		},
		{
			"session usage newer patch",
			"agentsview v0.31.2 (commit abc, built 2026-06-01)",
			capSessionUsage, true,
		},
		{
			"major bump",
			"agentsview v1.0.0 (commit abc, built 2026-01-01)",
			capSessionUsage, true,
		},
		{
			"dev suffix at session min",
			"agentsview v0.30.0-1-g891cb62 (commit 891cb62, built 2026-05-23)",
			capSessionUsage, true,
		},
		{
			"very old",
			"agentsview v0.10.0 (commit abc, built 2026-01-01)",
			capNone, true,
		},
		{"unparseable", "something unexpected", capNone, false},
		{"empty", "", capNone, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			level, parsed := parseVersion([]byte(tt.output))
			assert.Equal(t, tt.wantLevel, level, "level")
			assert.Equal(t, tt.wantParsed, parsed, "parsed")
		})
	}
}

// installFakeAgentsview writes an executable "agentsview" shell script
// into a fresh temp dir, prepends it to PATH, and resets the version
// cache. Lets FetchForSession/resolveAgentsview run without a real
// agentsview install. Skips on Windows (scripts are POSIX shell).
func installFakeAgentsview(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}
	ResetVersionCache()
	t.Cleanup(ResetVersionCache)

	dir := t.TempDir()
	bin := filepath.Join(dir, "agentsview")
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := exec.LookPath("agentsview")
	require.NoError(t, err)
}

func TestFetchForSessionSkipsOldVersion(t *testing.T) {
	// agentsview too old (< 0.15.0): FetchForSession returns nil
	// without invoking any usage command (which could spawn a server).
	installFakeAgentsview(t, `#!/bin/sh
if [ "$1" = "version" ]; then
  echo "agentsview v0.14.0 (commit abc, built 2026-01-01)"
  exit 0
fi
echo "ERROR: should not be called" >&2
exit 99
`)

	usage, err := FetchForSession(context.Background(), "test-session-id")
	require.NoError(t, err)
	assert.Nil(t, usage)
}

func TestFetchForSessionUsesSessionUsageOnNewVersion(t *testing.T) {
	// agentsview >= 0.30.0: FetchForSession calls `session usage` and
	// captures the cost estimate. The script errors on any other
	// subcommand, so reaching the JSON proves command selection.
	installFakeAgentsview(t, `#!/bin/sh
if [ "$1" = "version" ]; then
  echo "agentsview v0.30.0 (commit abc, built 2026-05-23)"
  exit 0
fi
if [ "$1" = "session" ] && [ "$2" = "usage" ]; then
  echo '{"session_id":"s","agent":"codex","total_output_tokens":28800,"peak_context_tokens":118000,"cost_usd":0.42,"has_cost":true}'
  exit 0
fi
echo "unexpected args: $@" >&2
exit 99
`)

	usage, err := FetchForSession(context.Background(), "s")
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, int64(28800), usage.OutputTokens)
	assert.Equal(t, int64(118000), usage.PeakContextTokens)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.42, usage.CostUSD, 1e-9)
}

func TestFetchForSessionUsesSessionUsageWhenPrereleaseSupportsIt(t *testing.T) {
	installFakeAgentsview(t, `#!/bin/sh
if [ "$1" = "version" ]; then
  echo "agentsview v0.29.0-22-ga31468b4 (commit a31468b4, built 2026-05-23)"
  exit 0
fi
if [ "$1" = "session" ] && [ "$2" = "usage" ]; then
  echo '{"session_id":"s","agent":"codex","total_output_tokens":28800,"peak_context_tokens":118000,"cost_usd":0.42,"has_cost":true}'
  exit 0
fi
echo "unexpected args: $@" >&2
exit 99
`)

	usage, err := FetchForSession(context.Background(), "s")
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, int64(28800), usage.OutputTokens)
	assert.Equal(t, int64(118000), usage.PeakContextTokens)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.42, usage.CostUSD, 1e-9)
}

func TestFetchForSessionFallsBackToTokenUse(t *testing.T) {
	// agentsview in [0.15.0, before session usage support):
	// FetchForSession falls back to the deprecated token-use command.
	// The legacy output here has no cost, matching older builds. The
	// script errors on `session usage`, so success proves the fallback.
	installFakeAgentsview(t, `#!/bin/sh
if [ "$1" = "version" ]; then
  echo "agentsview v0.15.0 (commit abc, built 2026-01-01)"
  exit 0
fi
if [ "$1" = "token-use" ]; then
  echo '{"session_id":"s","agent":"codex","total_output_tokens":1000,"peak_context_tokens":2000}'
  exit 0
fi
echo "unexpected args: $@" >&2
exit 99
`)

	usage, err := FetchForSession(context.Background(), "s")
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, int64(1000), usage.OutputTokens)
	assert.Equal(t, int64(2000), usage.PeakContextTokens)
	assert.False(t, usage.HasCost, "token-use output carries no cost")
}

func TestFetchForSessionExitCodesMeanNoUsage(t *testing.T) {
	// Exit 2 (not found) and 3 (no token/cost data) are not errors:
	// FetchForSession returns (nil, nil) for both.
	for _, code := range []int{2, 3} {
		t.Run(fmt.Sprintf("exit%d", code), func(t *testing.T) {
			installFakeAgentsview(t, fmt.Sprintf(`#!/bin/sh
if [ "$1" = "version" ]; then
  echo "agentsview v0.30.0 (commit abc, built 2026-05-23)"
  exit 0
fi
exit %d
`, code))

			usage, err := FetchForSession(context.Background(), "missing")
			require.NoError(t, err)
			assert.Nil(t, usage)
		})
	}
}

func TestResolveAgentsviewRetriesAfterTransientFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	ResetVersionCache()
	t.Cleanup(ResetVersionCache)

	// First call: agentsview not on PATH → transient failure.
	t.Setenv("PATH", t.TempDir())
	_, level := resolveAgentsview(context.Background())
	assert.Equal(t, capNone, level, "should fail when binary is absent")

	// Install a valid agentsview and retry — should succeed.
	dir := t.TempDir()
	bin := filepath.Join(dir, "agentsview")
	script := `#!/bin/sh
if [ "$1" = "version" ]; then
  echo "agentsview v0.15.0 (commit abc, built 2026-01-01)"
  exit 0
fi
if [ "$1" = "token-use" ]; then
  exit 0
fi
exit 99
`
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)

	path, level := resolveAgentsview(context.Background())
	assert.Equal(t, capTokenUse, level, "should succeed after binary appears")
	assert.Equal(t, bin, path)
}

func TestResolveAgentsviewCachesTooOldPermanently(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	ResetVersionCache()
	t.Cleanup(ResetVersionCache)

	dir := t.TempDir()
	bin := filepath.Join(dir, "agentsview")
	script := `#!/bin/sh
echo "agentsview v0.14.0 (commit abc, built 2026-01-01)"
`
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)

	_, level := resolveAgentsview(context.Background())
	assert.Equal(t, capNone, level)

	// Even if we "upgrade" the script, the too-old result is cached.
	script2 := `#!/bin/sh
echo "agentsview v0.30.0 (commit abc, built 2026-05-23)"
`
	require.NoError(t, os.WriteFile(bin, []byte(script2), 0o755))

	_, level = resolveAgentsview(context.Background())
	assert.Equal(t, capNone, level, "too-old should be cached permanently")
}

func TestResolveAgentsviewInvalidatesCacheOnPathChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	ResetVersionCache()
	t.Cleanup(ResetVersionCache)

	// First call: agentsview at dir1 is too old → cached.
	dir1 := t.TempDir()
	bin1 := filepath.Join(dir1, "agentsview")
	script1 := "#!/bin/sh\necho 'agentsview v0.14.0 (commit abc)'\n"
	require.NoError(t, os.WriteFile(bin1, []byte(script1), 0o755))

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", dir1+string(os.PathListSeparator)+origPath)

	_, level := resolveAgentsview(context.Background())
	assert.Equal(t, capNone, level)

	// "Upgrade" by placing a new binary earlier in PATH.
	dir2 := t.TempDir()
	bin2 := filepath.Join(dir2, "agentsview")
	script2 := "#!/bin/sh\necho 'agentsview v0.30.0 (commit def)'\n"
	require.NoError(t, os.WriteFile(bin2, []byte(script2), 0o755))

	t.Setenv("PATH", dir2+string(os.PathListSeparator)+origPath)

	path, level := resolveAgentsview(context.Background())
	assert.Equal(t, capSessionUsage, level, "new path should trigger re-probe")
	assert.Equal(t, bin2, path)
}

func TestResolveAgentsviewRetriesAfterUnparseableOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	ResetVersionCache()
	t.Cleanup(ResetVersionCache)

	// First call: agentsview returns unparseable version output.
	dir := t.TempDir()
	bin := filepath.Join(dir, "agentsview")
	script := "#!/bin/sh\necho 'something unexpected'\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)

	_, level := resolveAgentsview(context.Background())
	assert.Equal(t, capNone, level, "unparseable should fail")

	// Replace with valid version — should succeed because unparseable
	// output was NOT cached as too-old.
	dir2 := t.TempDir()
	bin2 := filepath.Join(dir2, "agentsview")
	script2 := "#!/bin/sh\necho 'agentsview v0.15.0 (commit abc)'\n"
	require.NoError(t, os.WriteFile(bin2, []byte(script2), 0o755))

	t.Setenv("PATH", dir2+string(os.PathListSeparator)+origPath)

	path, level := resolveAgentsview(context.Background())
	assert.NotEqual(t, capNone, level, "should succeed after valid version appears")
	assert.NotEmpty(t, path)
}

func TestToJSON(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		assert.Empty(t, ToJSON(nil))
	})

	t.Run("round trip", func(t *testing.T) {
		orig := &Usage{
			PeakContextTokens: 5000,
			OutputTokens:      300,
		}
		s := ToJSON(orig)
		got := ParseJSON(s)
		require.NotNil(t, got)
		assert.Equal(t, orig.PeakContextTokens, got.PeakContextTokens)
		assert.Equal(t, orig.OutputTokens, got.OutputTokens)
	})

	t.Run("round trip with cost", func(t *testing.T) {
		orig := &Usage{
			PeakContextTokens: 5000,
			OutputTokens:      300,
			CostUSD:           1.23,
			HasCost:           true,
		}
		got := ParseJSON(ToJSON(orig))
		require.NotNil(t, got)
		assert.True(t, got.HasCost)
		assert.InDelta(t, 1.23, got.CostUSD, 1e-9)
	})
}
