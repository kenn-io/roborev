package tokens

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

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

// installFakeAgentsview writes an executable "agentsview" shell script
// into a fresh temp dir and prepends it to PATH. Lets FetchForSession
// run without a real agentsview install. Skips on Windows (scripts are
// POSIX shell).
func installFakeAgentsview(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "agentsview")
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := exec.LookPath("agentsview")
	require.NoError(t, err)
}

func TestFetchForSessionSurfacesTokenUseFailureAfterSessionUsageFallback(t *testing.T) {
	installFakeAgentsview(t, `#!/bin/sh
if [ "$1" = "session" ] && [ "$2" = "usage" ]; then
  echo "unknown command: session usage" >&2
  exit 1
fi
echo "unexpected args: $@" >&2
exit 99
`)

	usage, err := FetchForSession(context.Background(), "test-session-id")
	require.Error(t, err)
	assert.Nil(t, usage)
	assert.Contains(t, err.Error(), "agentsview token-use: exit 99")
	assert.Contains(t, err.Error(), "unexpected args: token-use test-session-id")
}

func TestFetchForSessionUsesSessionUsage(t *testing.T) {
	// The script errors on any other subcommand, so reaching the JSON
	// proves command selection.
	installFakeAgentsview(t, `#!/bin/sh
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

func TestFetchForSessionWithConfigUsesHTTPEndpoint(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"session_id":"codex:s/1","agent":"codex",`+
			`"project":"roborev","total_output_tokens":28800,`+
			`"peak_context_tokens":118000,"has_token_data":true,`+
			`"cost_usd":0.42,"has_cost":true}`)
	}))
	t.Cleanup(server.Close)

	usage, err := FetchForSessionWithConfig(
		context.Background(), "codex:s/1",
		FetchConfig{
			Endpoint: server.URL + "/api/v1/sessions/{session_id}/usage",
			Timeout:  time.Second,
		},
	)

	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, "/api/v1/sessions/codex:s%2F1/usage", gotPath)
	assert.Equal(t, int64(28800), usage.OutputTokens)
	assert.Equal(t, int64(118000), usage.PeakContextTokens)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.42, usage.CostUSD, 1e-9)
}

func TestFetchForSessionWithConfigHonorsUsageFlags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"session_id":"s","agent":"codex",`+
			`"total_output_tokens":999,"peak_context_tokens":888,`+
			`"has_token_data":false,"cost_usd":0.0,"has_cost":true}`)
	}))
	t.Cleanup(server.Close)

	usage, err := FetchForSessionWithConfig(
		context.Background(), "s",
		FetchConfig{
			Endpoint: server.URL + "/usage/{session_id}",
			Timeout:  time.Second,
		},
	)

	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Zero(t, usage.OutputTokens)
	assert.Zero(t, usage.PeakContextTokens)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.0, usage.CostUSD, 1e-9)
}

func TestFetchForSessionWithConfigHTTPNotFoundMeansNoUsage(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)

	usage, err := FetchForSessionWithConfig(
		context.Background(), "missing",
		FetchConfig{
			Endpoint: server.URL + "/usage/{session_id}",
			Timeout:  time.Second,
		},
	)

	require.NoError(t, err)
	assert.Nil(t, usage)
}

func TestFetchForSessionWithConfigHTTPStatusErrorReportsExactStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "unauthorized",
			status: http.StatusUnauthorized,
			body:   `{"error":{"code":"unauthorized"}}`,
		},
		{
			name:   "unprocessable entity",
			status: http.StatusUnprocessableEntity,
			body:   `{"error":{"code":"invalid_session_id"}}`,
		},
		{
			name:   "internal server error",
			status: http.StatusInternalServerError,
			body:   `{"error":{"code":"usage_query_failed"}}`,
		},
		{
			name:   "service unavailable without body",
			status: http.StatusServiceUnavailable,
			body:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.body == "" {
					w.WriteHeader(tt.status)
					return
				}
				http.Error(w, tt.body, tt.status)
			}))
			t.Cleanup(server.Close)

			usage, err := FetchForSessionWithConfig(
				context.Background(), "s",
				FetchConfig{
					Endpoint: server.URL + "/usage/{session_id}",
					Timeout:  time.Second,
				},
			)

			require.Error(t, err)
			assert.Nil(t, usage)
			assert.Contains(t, err.Error(), fmt.Sprintf("HTTP %d %s", tt.status, http.StatusText(tt.status)))
			if tt.body != "" {
				assert.Contains(t, err.Error(), tt.body)
			}
		})
	}
}

func TestFetchForSessionWithConfigRejectsBadSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"session_id":"s","has_token_data":true,"has_cost":false}`)
	}))
	t.Cleanup(server.Close)

	usage, err := FetchForSessionWithConfig(
		context.Background(), "s",
		FetchConfig{
			Endpoint: server.URL + "/usage/{session_id}",
			Timeout:  time.Second,
		},
	)

	require.Error(t, err)
	assert.Nil(t, usage)
	assert.Contains(t, err.Error(), "missing token counts")
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

func TestFetchForSessionUsesSessionUsageForHeadBuild(t *testing.T) {
	installFakeAgentsview(t, `#!/bin/sh
if [ "$1" = "version" ]; then
  echo "agentsview HEAD (commit a31468b4, built 2026-06-07)"
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

func TestFetchForSessionSurfacesSessionUsageFailureForHeadBuild(t *testing.T) {
	installFakeAgentsview(t, `#!/bin/sh
if [ "$1" = "version" ]; then
  echo "agentsview HEAD (commit a31468b4, built 2026-06-07)"
  exit 0
fi
if [ "$1" = "session" ] && [ "$2" = "usage" ]; then
  echo "usage exploded" >&2
  exit 42
fi
echo "unexpected args: $@" >&2
exit 99
`)

	usage, err := FetchForSession(context.Background(), "s")
	require.Error(t, err)
	assert.Nil(t, usage)
	assert.Contains(t, err.Error(), "agentsview usage: exit 42")
	assert.Contains(t, err.Error(), "usage exploded")
}

func TestFetchForSessionFallsBackToTokenUseWhenSessionUsageIsMissing(t *testing.T) {
	installFakeAgentsview(t, `#!/bin/sh
if [ "$1" = "session" ] && [ "$2" = "usage" ]; then
  echo "unknown command: session usage" >&2
  exit 1
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
	assert.False(t, usage.HasCost)
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

func TestFetchForSessionSkipsMissingAgentsviewByDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH handling differs on Windows")
	}

	t.Setenv("PATH", t.TempDir())

	usage, err := FetchForSession(context.Background(), "s")

	require.NoError(t, err)
	assert.Nil(t, usage)
}

func TestFetchForSessionWithConfigRequiresAgentsviewWhenRequested(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH handling differs on Windows")
	}

	t.Setenv("PATH", t.TempDir())

	usage, err := FetchForSessionWithConfig(
		context.Background(), "s", FetchConfig{RequireCLI: true},
	)

	require.Error(t, err)
	assert.Nil(t, usage)
	assert.Contains(t, err.Error(), "agentsview lookup")
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
