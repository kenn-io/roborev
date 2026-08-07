package tokens

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
			"codex input counts",
			Usage{
				InputTokens: 79150, CachedInputTokens: 2560,
				OutputTokens: 3389,
			},
			"79.2k in (2.6k cached) · 3.4k out",
		},
		{
			"cost only",
			Usage{CostUSD: 0.05, HasCost: true},
			"~$0.05",
		},
		{
			"cache writes are reported alongside reads",
			Usage{
				InputTokens: 24594, CachedInputTokens: 1408,
				CacheCreationTokens: 8192, OutputTokens: 290,
			},
			"24.6k in (1.4k cached, 8.2k written) · 290 out",
		},
		{
			// Cache writes alone are real consumption, so the summary must
			// not collapse to an empty string.
			"cache writes alone still summarize",
			Usage{CacheCreationTokens: 512},
			"0 ctx (512 written) · 0 out",
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

	t.Run("cost flag without amount is unpriced", func(t *testing.T) {
		u := ParseJSON(`{"total_output_tokens":200,"has_cost":true}`)
		require.NotNil(t, u)
		assert.False(t, u.HasCost)
	})

	t.Run("null cost amount is unpriced", func(t *testing.T) {
		u := ParseJSON(
			`{"total_output_tokens":200,"cost_usd":null,"has_cost":true}`,
		)
		require.NotNil(t, u)
		assert.False(t, u.HasCost)
	})

	t.Run("explicit zero cost remains priced", func(t *testing.T) {
		u := ParseJSON(`{"cost_usd":0,"has_cost":true}`)
		require.NotNil(t, u)
		assert.True(t, u.HasCost)
		assert.Zero(t, u.CostUSD)
	})

	t.Run("codex input buckets", func(t *testing.T) {
		u := ParseJSON(
			`{"input_tokens":79150,"cached_input_tokens":2560,` +
				`"total_output_tokens":3389}`,
		)
		require.NotNil(t, u)
		assert.Equal(t, int64(79150), u.InputTokens)
		assert.Equal(t, int64(2560), u.CachedInputTokens)
		assert.Equal(t, int64(3389), u.OutputTokens)
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

func TestParseCodexUsageJSONL(t *testing.T) {
	log := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-123"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":79150,"cached_input_tokens":2560,"output_tokens":3389}}`,
	}, "\n") + "\n"

	usage, err := ParseCodexUsageJSONL(strings.NewReader(log))
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, int64(79150), usage.InputTokens)
	assert.Equal(t, int64(2560), usage.CachedInputTokens)
	assert.Equal(t, int64(3389), usage.OutputTokens)
	assert.Equal(t, "job_log_turn_completed", usage.UsageSource)
	assert.Equal(t, "thread-123", usage.ThreadID)
	assert.Positive(t, usage.EventOffset)
}

// Codex reports cache-creation tokens separately from cache reads. The field
// exists as of codex-cli 0.146.0 and must not be folded into
// cached_input_tokens, which counts cache *reads*.
func TestParseCodexUsageJSONLCapturesCacheWriteTokens(t *testing.T) {
	log := `{"type":"turn.completed","usage":{"input_tokens":24594,` +
		`"cached_input_tokens":1408,"cache_write_input_tokens":8192,` +
		`"output_tokens":290,"reasoning_output_tokens":158}}` + "\n"

	usage, err := ParseCodexUsageJSONL(strings.NewReader(log))
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, int64(24594), usage.InputTokens)
	assert.Equal(t, int64(1408), usage.CachedInputTokens)
	assert.Equal(t, int64(8192), usage.CacheCreationTokens)
	assert.Equal(t, int64(290), usage.OutputTokens)
}

// A turn that only wrote cache still consumed tokens, so it is usage data.
func TestParseCodexUsageJSONLCacheWriteAloneIsUsage(t *testing.T) {
	usage, err := ParseCodexUsageJSONL(strings.NewReader(
		`{"type":"turn.completed","usage":{"cache_write_input_tokens":512}}` + "\n",
	))
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, int64(512), usage.CacheCreationTokens)
}

func TestParseCodexUsageJSONLIgnoresMissingUsage(t *testing.T) {
	usage, err := ParseCodexUsageJSONL(strings.NewReader(
		"plain text\n" +
			`{"type":"turn.completed","usage":{}}` + "\n",
	))

	require.NoError(t, err)
	assert.Nil(t, usage)
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
	require.ErrorIs(t, err, ErrUsageProviderUnavailable)
	assert.Contains(t, err.Error(), "agentsview lookup")
}

// agentsview v0.39.0 moved the cost from a "cost_usd" float to a
// "cost": {"microdollars": N} envelope. Both shapes must decode, and a null
// cost_usd must never be read as a valid $0.
func TestUsageFromSessionPayloadCostShapes(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantErr   string
		wantCost  bool
		wantUSD   float64
		wantNoUse bool
	}{
		{
			name:     "legacy cost_usd float",
			body:     `{"has_token_data":false,"cost_usd":0.42,"has_cost":true}`,
			wantCost: true,
			wantUSD:  0.42,
		},
		{
			name:     "microdollar envelope",
			body:     `{"has_token_data":false,"cost":{"microdollars":131767},"has_cost":true}`,
			wantCost: true,
			wantUSD:  0.131767,
		},
		{
			name: "null cost_usd falls back to microdollars",
			body: `{"has_token_data":false,"cost_usd":null,` +
				`"cost":{"microdollars":598641},"has_cost":true}`,
			wantCost: true,
			wantUSD:  0.598641,
		},
		{
			name: "both present prefers microdollar envelope",
			body: `{"has_token_data":false,"cost_usd":0.42,` +
				`"cost":{"microdollars":999999},"has_cost":true}`,
			wantCost: true,
			wantUSD:  0.999999,
		},
		{
			name:     "zero microdollars is a valid free run",
			body:     `{"has_token_data":false,"cost":{"microdollars":0},"has_cost":true}`,
			wantCost: true,
			wantUSD:  0,
		},
		{
			name:     "zero cost_usd is a valid free run",
			body:     `{"has_token_data":false,"cost_usd":0,"has_cost":true}`,
			wantCost: true,
			wantUSD:  0,
		},
		{
			name:    "has_cost with neither field is a schema error",
			body:    `{"has_token_data":false,"has_cost":true}`,
			wantErr: "missing cost",
		},
		{
			name: "null microdollars is not a valid zero",
			body: `{"has_token_data":false,"cost":{"microdollars":null},` +
				`"has_cost":true}`,
			wantErr: "missing cost",
		},
		{
			name:      "no cost and no tokens means no usage",
			body:      `{"has_token_data":false,"has_cost":false}`,
			wantNoUse: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var payload SessionUsagePayload
			require.NoError(t, json.Unmarshal([]byte(tt.body), &payload))

			usage, err := UsageFromSessionPayload(payload)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Nil(t, usage)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.wantNoUse {
				assert.Nil(t, usage)
				return
			}
			require.NotNil(t, usage)
			assert.Equal(t, tt.wantCost, usage.HasCost)
			assert.InDelta(t, tt.wantUSD, usage.CostUSD, 1e-9)
		})
	}
}

func TestFetchForSessionCLIParsesMicrodollarCost(t *testing.T) {
	installFakeAgentsview(t, `#!/bin/sh
if [ "$1" = "session" ] && [ "$2" = "usage" ]; then
  echo '{"session_id":"s","agent":"codex","total_output_tokens":4762,"peak_context_tokens":70092,"has_token_data":true,"cost_usd":null,"cost":{"microdollars":598641},"has_cost":true,"cost_source":"computed"}'
  exit 0
fi
echo "unexpected args: $@" >&2
exit 99
`)

	usage, err := FetchForSession(context.Background(), "s")
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, int64(4762), usage.OutputTokens)
	assert.Equal(t, int64(70092), usage.PeakContextTokens)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.598641, usage.CostUSD, 1e-9)
}

// An unpriced session must not be recorded as a $0 review: dropping the cost
// flag keeps it out of the priced numerator instead of deflating the total.
func TestFetchForSessionCLIHasCostWithoutAmountDropsCost(t *testing.T) {
	installFakeAgentsview(t, `#!/bin/sh
if [ "$1" = "session" ] && [ "$2" = "usage" ]; then
  echo '{"session_id":"s","agent":"codex","total_output_tokens":300,"peak_context_tokens":5000,"has_token_data":true,"has_cost":true}'
  exit 0
fi
echo "unexpected args: $@" >&2
exit 99
`)

	usage, err := FetchForSession(context.Background(), "s")
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, int64(300), usage.OutputTokens)
	assert.False(t, usage.HasCost)
	assert.Zero(t, usage.CostUSD)
}

func TestFetchForSessionCLIHasCostWithoutAmountOrTokensMeansNoUsage(t *testing.T) {
	installFakeAgentsview(t, `#!/bin/sh
if [ "$1" = "session" ] && [ "$2" = "usage" ]; then
  echo '{"session_id":"s","agent":"codex","has_token_data":false,"has_cost":true}'
  exit 0
fi
echo "unexpected args: $@" >&2
exit 99
`)

	usage, err := FetchForSession(context.Background(), "s")
	require.NoError(t, err)
	assert.Nil(t, usage)
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

	// A run priced at exactly $0 must persist the amount, not just the flag.
	// Dropping it would make a real free run byte-identical to the drifted
	// rows that recorded has_cost with no dollars.
	t.Run("explicit zero cost survives serialization", func(t *testing.T) {
		s := ToJSON(&Usage{OutputTokens: 300, CostUSD: 0, HasCost: true})
		assert.Contains(t, s, `"cost_usd":0`)
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
