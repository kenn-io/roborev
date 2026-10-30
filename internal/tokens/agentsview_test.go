package tokens

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentsViewRawID(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/base/api/v1/sessions/raw-id/usage":
			assert.Equal(t, "true", r.URL.Query().Get("subagents"))
			w.WriteHeader(http.StatusNotFound)
		case "/base/api/v1/session-ids/resolve":
			assert.Equal(t, "raw-id", r.URL.Query().Get("partial"))
			assert.Equal(t, "true", r.URL.Query().Get("raw_suffix"))
			assert.Equal(t, "2", r.URL.Query().Get("limit"))
			fmt.Fprint(w, `{"raw_suffix":true,"ids":["codex:raw-id"]}`)
		case "/base/api/v1/sessions/codex:raw-id/usage":
			assert.Equal(t, "true", r.URL.Query().Get("subagents"))
			fmt.Fprint(w, `{"has_token_data":true,"total_output_tokens":25,"peak_context_tokens":100,"has_cost":true,"cost":{"microdollars":1250000}}`)
		default:
			assert.Contains(t, []string{"/base/api/v1/sessions/raw-id/usage", "/base/api/v1/session-ids/resolve", "/base/api/v1/sessions/codex:raw-id/usage"}, r.URL.Path)
		}
	}))
	defer srv.Close()
	usage, err := FetchForSessionWithConfig(context.Background(), "raw-id", FetchConfig{AgentsViewURL: srv.URL + "/base", AgentsViewToken: "fixture-token"})
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.InDelta(t, 1.25, usage.CostUSD, 1e-9)
	assert.Equal(t, int64(25), usage.OutputTokens)
	assert.Len(t, paths, 3)
}

func TestAgentsViewResponses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []int
		resolve  string
		want     string
		wantCost float64
	}{
		{"direct", []int{200}, "", "", 0.5},
		{"not indexed", []int{404, 200}, `{"raw_suffix":true,"ids":[]}`, "", 0},
		{"resolved missing", []int{404, 200, 404}, `{"raw_suffix":true,"ids":["codex:raw-id"]}`, "", 0},
		{"ambiguous resolve", []int{404, 200}, `{"raw_suffix":true,"ids":["codex:a","host~codex:a"]}`, "ambiguous", 0},
		{"ambiguous direct", []int{409}, "", "ambiguous", 0},
		{"usage unauthorized", []int{401}, "", "rejected the token", 0},
		{"usage forbidden", []int{403}, "", "rejected the token", 0},
		{"resolve unauthorized", []int{404, 401}, "", "rejected the token", 0},
		{"resolve forbidden", []int{404, 403}, "", "rejected the token", 0},
		{"retry unauthorized", []int{404, 200, 401}, `{"raw_suffix":true,"ids":["codex:a"]}`, "rejected the token", 0},
		{"retry forbidden", []int{404, 200, 403}, `{"raw_suffix":true,"ids":["codex:a"]}`, "rejected the token", 0},
		{"server error", []int{500}, "", "HTTP", 0},
		{"malformed resolve", []int{404, 200}, `{`, "parse", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
				if calls >= len(tc.statuses) {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				status := tc.statuses[calls]
				calls++
				w.WriteHeader(status)
				if r.URL.Path == "/api/v1/session-ids/resolve" {
					fmt.Fprint(w, tc.resolve)
				} else if status == 200 {
					fmt.Fprint(w, `{"has_token_data":false,"has_cost":true,"cost_usd":0.5}`)
				} else {
					fmt.Fprint(w, `{"error":{"code":"ambiguous_session_id","candidates":["codex:a","host~codex:a"],"detail":"private-server-body"}}`)
				}
			}))
			defer srv.Close()
			usage, err := FetchForSessionWithConfig(context.Background(), "raw-id", FetchConfig{AgentsViewURL: srv.URL, AgentsViewToken: "fixture-token", RequireCLI: true})
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
				assert.NotContains(t, err.Error(), "private-server-body")
				assert.NotContains(t, err.Error(), "fixture-token")
			} else {
				require.NoError(t, err)
				if tc.wantCost == 0 {
					assert.Nil(t, usage)
				} else {
					require.NotNil(t, usage)
					assert.InDelta(t, tc.wantCost, usage.CostUSD, 1e-9)
				}
			}
			assert.Equal(t, len(tc.statuses), calls)
		})
	}
}

func FuzzAgentsViewIDEscaping(f *testing.F) {
	for _, s := range []string{"raw-id", "codex:raw-id", "host~codex:raw/id", "a?b#c%2F", "../x", "", "\x00", "a+b &c"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, id string) {
		endpoint := agentsViewUsageURL("https://usage.example/base", id)
		u, err := url.Parse(endpoint)
		require.NoError(t, err)
		assert.Equal(t, "usage.example", u.Host)
		assert.Equal(t, url.Values{"subagents": {"true"}}, u.Query())
		escaped := strings.TrimSuffix(strings.TrimPrefix(u.EscapedPath(), "/base/api/v1/sessions/"), "/usage")
		assert.NotContains(t, escaped, "/")
		decoded, err := url.PathUnescape(escaped)
		require.NoError(t, err)
		assert.Equal(t, id, decoded)
		assert.Empty(t, u.Fragment)
	})
}

func TestAgentsViewNeverFallsBackToCLI(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "invoked")
	t.Setenv("USAGE_CLI_MARKER", marker)
	installFakeAgentsview(t, "#!/bin/sh\nprintf invoked > \"$USAGE_CLI_MARKER\"\nexit 99\n")
	for _, status := range []int{401, 404, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/session-ids/resolve" {
				fmt.Fprint(w, `{"raw_suffix":true,"ids":[]}`)
				return
			}
			w.WriteHeader(status)
		}))
		_, err := FetchForSessionWithConfig(t.Context(), "raw-id", FetchConfig{AgentsViewURL: srv.URL})
		if status == 404 {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
		srv.Close()
	}
	assert.NoFileExists(t, marker)
}

type agentsViewRoundTripper func(*http.Request) (*http.Response, error)

func (f agentsViewRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAgentsViewTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		cfg := FetchConfig{AgentsViewURL: "https://usage.example", Timeout: 2 * time.Second, Client: &http.Client{Transport: agentsViewRoundTripper(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}}
		_, err := FetchForSessionWithConfig(t.Context(), "raw-id", cfg)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, 2*time.Second, time.Since(start))
	})
}
