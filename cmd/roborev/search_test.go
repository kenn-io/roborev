package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchCmdUsesGlobalDefaults(t *testing.T) {
	var calls int
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/search" {
				return false
			}
			calls++
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "memory leak", r.URL.Query().Get("q"))
			assert.Equal(t, "auto", r.URL.Query().Get("mode"))
			assert.Equal(t, "all", r.URL.Query().Get("state"))
			assert.Equal(t, "20", r.URL.Query().Get("limit"))
			assert.False(t, r.URL.Query().Has("repo"), "search must remain global without --repo")
			writeSearchResponse(t, w, emptySearchResponse("memory leak", "lexical"))
			return true
		},
	})

	_, _, err := executeSearchCmd(t, "memory leak")
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestSearchCmdKataQueryAndAgentConvention(t *testing.T) {
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/search" {
				return false
			}
			assert.Equal(t, "memory leak", r.URL.Query().Get("q"))
			assert.Equal(t, "auto", r.URL.Query().Get("mode"))
			writeSearchResponse(t, w, emptySearchResponse("memory leak", "lexical"))
			return true
		},
	})
	for _, args := range [][]string{{"memory", "leak", "--agent"}, {"memory leak", "--agent"}} {
		output, _, err := executeSearchCmd(t, args...)
		require.NoError(t, err)
		assert.Equal(t, "OK search count=0 query=\"memory leak\" mode=lexical partial=false bounded=false\n", output)
	}
}

func TestSearchCmdAgentOutputReportsResultsAndDegradation(t *testing.T) {
	response := emptySearchResponse("related race", "lexical")
	response["degraded"] = true
	response["degraded_reason"] = "semantic search is unavailable"
	response["partial"] = true
	response["bounded"] = true
	response["bounded_reason"] = "semantic candidate ceiling exhausted"
	response["hits"] = []map[string]any{{
		"job_id": 42, "review_id": 9, "repo_name": "acme/widgets", "repo_path": "/src/widgets",
		"git_ref": "main", "review_type": "code", "agent": "codex", "closed": true,
		"finished_at": "2026-09-15T10:11:12Z", "score": 0.5,
		"matched_in": []string{"lexical"}, "excerpt": "Lock the \x1b[31mshared\x1b[0m map.\nNext line.",
	}}
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/search" {
				return false
			}
			writeSearchResponse(t, w, response)
			return true
		},
	})
	output, _, err := executeSearchCmd(t, "related race", "--agent")
	require.NoError(t, err)
	assert.Equal(t, "OK search count=1 query=\"related race\" mode=lexical partial=true bounded=true degraded=\"semantic search is unavailable\" bounded_reason=\"semantic candidate ceiling exhausted\"\n"+
		"- job=42 review=9 score=0.5 state=closed matched=lexical repo=acme/widgets ref=main verdict=unknown excerpt=\"Lock the shared map.\\nNext line.\"\n", output)
}

func TestSearchCmdEncodesEveryFreeFormValueAndTranslatesFlags(t *testing.T) {
	query := "panic + path/β ?&="
	repo := "/tmp/acme + api?&="
	branch := "feature/a+b ?&="
	since := "2026-09-15T10:11:12+02:00"
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/search" {
				return false
			}
			values := r.URL.Query()
			assert.Equal(t, query, values.Get("q"))
			assert.Equal(t, repo, values.Get("repo"))
			assert.Equal(t, branch, values.Get("branch"))
			assert.Equal(t, since, values.Get("since"))
			assert.Equal(t, "fail", values.Get("verdict"))
			assert.Equal(t, "closed", values.Get("state"))
			assert.Equal(t, "semantic", values.Get("mode"))
			assert.Equal(t, "7", values.Get("limit"))
			for key, value := range map[string]string{
				"q": query, "repo": repo, "branch": branch, "since": since,
			} {
				assert.Contains(t, r.URL.RawQuery, key+"="+url.QueryEscape(value))
			}
			writeSearchResponse(t, w, emptySearchResponse(query, "semantic"))
			return true
		},
	})

	_, _, err := executeSearchCmd(t,
		query,
		"--repo", repo,
		"--branch", branch,
		"--since", since,
		"--verdict", "fail",
		"--closed",
		"--semantic",
		"--limit", "7",
	)
	require.NoError(t, err)
}

func TestSearchCmdAcceptsTwoThousandRuneQuery(t *testing.T) {
	query := strings.Repeat("界", 2000)
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/search" {
				return false
			}
			assert.Equal(t, query, r.URL.Query().Get("q"))
			writeSearchResponse(t, w, emptySearchResponse(query, "lexical"))
			return true
		},
	})

	_, _, err := executeSearchCmd(t, query, "--lexical")
	require.NoError(t, err)
}

func TestSearchCmdTranslatesStateAndModeSwitches(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantState string
		wantMode  string
	}{
		{name: "lexical open", args: []string{"--lexical", "--open"}, wantState: "open", wantMode: "lexical"},
		{name: "hybrid all", args: []string{"--hybrid"}, wantState: "all", wantMode: "hybrid"},
		{name: "semantic closed", args: []string{"--semantic", "--closed"}, wantState: "closed", wantMode: "semantic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			NewMockDaemon(t, MockRefineHooks{
				OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
					if r.URL.Path != "/api/search" {
						return false
					}
					assert.Equal(t, tt.wantState, r.URL.Query().Get("state"))
					assert.Equal(t, tt.wantMode, r.URL.Query().Get("mode"))
					writeSearchResponse(t, w, emptySearchResponse("query", tt.wantMode))
					return true
				},
			})

			args := append([]string{"query"}, tt.args...)
			_, _, err := executeSearchCmd(t, args...)
			require.NoError(t, err)
		})
	}
}

func TestSearchCmdRejectsInvalidInputWithExitCodeTwoAndUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing query"},
		{name: "conflicting output", args: []string{"query", "--agent", "--json"}},
		{name: "blank query", args: []string{" \t\n "}},
		{name: "overlong query", args: []string{strings.Repeat("界", 2001)}},
		{name: "invalid verdict", args: []string{"query", "--verdict", "PASS"}},
		{name: "invalid since", args: []string{"query", "--since", "yesterday"}},
		{name: "zero since duration", args: []string{"query", "--since", "0s"}},
		{name: "negative since duration", args: []string{"query", "--since", "-1h"}},
		{name: "nonnumeric limit", args: []string{"query", "--limit", "many"}},
		{name: "zero limit", args: []string{"query", "--limit", "0"}},
		{name: "large limit", args: []string{"query", "--limit", "101"}},
		{name: "open and closed", args: []string{"query", "--open", "--closed"}},
		{name: "lexical and hybrid", args: []string{"query", "--lexical", "--hybrid"}},
		{name: "all search modes", args: []string{"query", "--lexical", "--hybrid", "--semantic"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, executed, err := executeSearchCmd(t, tt.args...)
			require.Error(t, err)
			assert.Equal(t, 2, commandExitCode(err))
			require.NotNil(t, executed)
			assert.False(t, executed.SilenceUsage)
		})
	}
}

func TestSearchCmdValidatesSinceBeforeCallingDaemon(t *testing.T) {
	var calls int
	NewMockDaemon(t, MockRefineHooks{
		OnPing: func(http.ResponseWriter, *http.Request, *mockRefineState) bool {
			calls++
			return false
		},
		OnUnhandled: func(http.ResponseWriter, *http.Request, *mockRefineState) bool {
			calls++
			return false
		},
	})

	for _, since := range []string{"yesterday", "0s", "-1h"} {
		_, executed, err := executeSearchCmd(t, "query", "--since", since)
		require.Error(t, err)
		assert.Equal(t, 2, commandExitCode(err))
		assert.False(t, executed.SilenceUsage)
	}
	assert.Zero(t, calls)
}

func TestSearchCmdPassesThroughValidSinceValues(t *testing.T) {
	for _, since := range []string{"90m", "2026-09-15T10:11:12+02:00"} {
		t.Run(since, func(t *testing.T) {
			NewMockDaemon(t, MockRefineHooks{
				OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
					if r.URL.Path != "/api/search" {
						return false
					}
					assert.Equal(t, since, r.URL.Query().Get("since"))
					writeSearchResponse(t, w, emptySearchResponse("query", "lexical"))
					return true
				},
			})

			_, _, err := executeSearchCmd(t, "query", "--since", since)
			require.NoError(t, err)
		})
	}
}

func TestSearchCmdExposesSwitchFlagsInsteadOfStateAndMode(t *testing.T) {
	cmd := searchCmd()
	assert.Nil(t, cmd.Flags().Lookup("state"))
	assert.Nil(t, cmd.Flags().Lookup("mode"))
	for _, name := range []string{"open", "closed", "lexical", "hybrid", "semantic"} {
		assert.NotNil(t, cmd.Flags().Lookup(name), name)
	}
}

func TestSearchCmdReturnsSanitizedTypedProblems(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "bad request", status: http.StatusBadRequest},
		{name: "service unavailable", status: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			NewMockDaemon(t, MockRefineHooks{
				OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
					if r.URL.Path != "/api/search" {
						return false
					}
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(tt.status)
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
						"title":    "\x1b[31mInvalid\x1b[0m\r\n\trequest",
						"status":   tt.status,
						"detail":   "\x1b]0;owned\x07semantic\x00 search\nunavailable\t",
						"instance": "https://provider.invalid/?token=raw-secret",
					}))
					return true
				},
			})

			_, executed, err := executeSearchCmd(t, "query-secret", "--semantic")
			require.Error(t, err)
			assert.Equal(t, 1, commandExitCode(err))
			require.NotNil(t, executed)
			assert.True(t, executed.SilenceUsage)
			assert.Contains(t, err.Error(), http.StatusText(tt.status))
			assert.Contains(t, err.Error(), "Invalidrequest")
			assert.Contains(t, err.Error(), "semantic searchunavailable")
			assert.NotContains(t, err.Error(), "\x1b")
			assert.NotContains(t, err.Error(), "\n")
			assert.NotContains(t, err.Error(), "\r")
			assert.NotContains(t, err.Error(), "\t")
			assert.NotContains(t, err.Error(), "raw-secret")
			assert.NotContains(t, err.Error(), "provider.invalid")
			assert.NotContains(t, err.Error(), "query-secret")
		})
	}
}

func TestSearchCmdUsesStatusOnlyForUnsafeProblemBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty"},
		{name: "malformed", body: `{"detail":"safe","raw":"raw-secret"`},
		{name: "unrecognized", body: `{"provider_body":"raw-secret","endpoint":"https://provider.invalid","query":"body-secret"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			NewMockDaemon(t, MockRefineHooks{
				OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
					if r.URL.Path != "/api/search" {
						return false
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					if tt.body != "" {
						_, writeErr := w.Write([]byte(tt.body))
						assert.NoError(t, writeErr)
					}
					return true
				},
			})

			_, executed, err := executeSearchCmd(t, "request-secret", "--semantic")
			require.Error(t, err)
			assert.Equal(t, 1, commandExitCode(err))
			assert.True(t, executed.SilenceUsage)
			assert.Equal(t, "search reviews: Service Unavailable (status 503)", err.Error())
			assert.NotContains(t, err.Error(), "raw-secret")
			assert.NotContains(t, err.Error(), "provider.invalid")
			assert.NotContains(t, err.Error(), "request-secret")
		})
	}
}

func TestSearchCmdSanitizesTransportFailures(t *testing.T) {
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/search" {
				return false
			}
			hijacker, ok := w.(http.Hijacker)
			assert.True(t, ok)
			if !ok {
				return true
			}
			conn, _, err := hijacker.Hijack()
			assert.NoError(t, err)
			if err == nil {
				assert.NoError(t, conn.Close())
			}
			return true
		},
	})

	_, executed, err := executeSearchCmd(t, "transport-query-secret")
	require.Error(t, err)
	assert.Equal(t, 1, commandExitCode(err))
	assert.True(t, executed.SilenceUsage)
	assert.Equal(t, "search reviews: request failed", err.Error())
}

func TestSearchCmdJSONPreservesUnsanitizedResponseBytes(t *testing.T) {
	raw := `{"query":"q","mode":"\u001b[31mhybrid\u001b[0m","degraded":false,"bounded":false,"partial":false,"coverage":{"mirror_complete":true,"embeddings_configured":false,"vector_state":"disabled","embedding_backlog":0,"skipped":0},"hits":[{"job_id":1,"review_id":2,"repo_name":"repo\nname","repo_path":"/repo","git_ref":"main","review_type":"code","agent":"codex","closed":false,"finished_at":"2026-09-15T10:11:12Z","score":0.5,"matched_in":[],"excerpt":"raw\t\u001b]0;title\u0007"}]}`
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/search" {
				return false
			}
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(raw))
			assert.NoError(t, err)
			return true
		},
	})

	output, _, err := executeSearchCmd(t, "q", "--json")
	require.NoError(t, err)
	assert.Equal(t, raw, output)
}

func TestSearchCmdJSONPreservesResponse(t *testing.T) {
	want := map[string]any{
		"query":          "related race",
		"mode":           "hybrid",
		"degraded":       false,
		"bounded":        true,
		"bounded_reason": "candidate ceiling reached",
		"partial":        true,
		"coverage": map[string]any{
			"mirror_complete":       false,
			"mirror_backlog":        float64(4),
			"embeddings_configured": true,
			"vector_state":          "building",
			"embedding_backlog":     float64(7),
			"skipped":               float64(2),
		},
		"hits": []any{
			map[string]any{
				"job_id": float64(42), "job_uuid": "job-uuid",
				"review_id": float64(9), "review_uuid": "review-uuid",
				"repo_name": "acme/widgets", "repo_path": "/src/widgets",
				"git_ref": "feature/search", "commit_sha": "abcdef0123456789",
				"commit_subject": "Fix search race", "branch": "feature/search",
				"review_type": "code", "panel_role": "member", "agent": "codex",
				"verdict": "fail", "closed": false,
				"finished_at": "2026-09-15T10:11:12Z", "score": 0.01639344262295082,
				"matched_in": []any{"lexical", "semantic"}, "excerpt": "Lock the shared map.",
			},
		},
	}
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/search" {
				return false
			}
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.NewEncoder(w).Encode(want))
			return true
		},
	})

	output, _, err := executeSearchCmd(t, "related race", "--json")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	assert.Equal(t, want, got)
}

func TestSearchCmdJSONKeepsEmptyHitsAsArray(t *testing.T) {
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/search" {
				return false
			}
			writeSearchResponse(t, w, emptySearchResponse("nothing", "lexical"))
			return true
		},
	})

	output, _, err := executeSearchCmd(t, "nothing", "--json")
	require.NoError(t, err)
	var got struct {
		Hits []json.RawMessage `json:"hits"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	assert.NotNil(t, got.Hits)
	assert.Empty(t, got.Hits)
}

func TestSearchCmdHumanOutputShowsModeCoverageAndZeroHits(t *testing.T) {
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/search" {
				return false
			}
			writeSearchResponse(t, w, emptySearchResponse("nothing", "lexical"))
			return true
		},
	})

	output, _, err := executeSearchCmd(t, "nothing")
	require.NoError(t, err)
	assert.Contains(t, output, "Mode: lexical")
	assert.Contains(t, output, "Coverage: mirror complete; vectors disabled")
	assert.Contains(t, output, "No matching reviews.")
	assert.Less(t, strings.Index(output, "Coverage:"), strings.Index(output, "No matching reviews."))
}

func TestSearchCmdHumanOutputShowsNoticesAndCompactHit(t *testing.T) {
	response := map[string]any{
		"query": "related race", "mode": "lexical",
		"degraded": true, "degraded_reason": "embedding provider unavailable",
		"bounded": true, "bounded_reason": "candidate ceiling reached", "partial": true,
		"coverage": map[string]any{
			"mirror_complete": false, "mirror_backlog": 3,
			"embeddings_configured": true, "vector_state": "building",
			"embedding_backlog": 7, "skipped": 2,
		},
		"hits": []map[string]any{{
			"job_id": 42, "job_uuid": "job-uuid", "review_id": 9, "review_uuid": "review-uuid",
			"repo_name": "acme/widgets", "repo_path": "/src/widgets",
			"git_ref": "feature/search", "commit_sha": "abcdef0123456789",
			"commit_subject": "Fix search race", "branch": "feature/search",
			"review_type": "code", "panel_role": "member", "agent": "codex",
			"verdict": "fail", "closed": false, "finished_at": "2026-09-15T10:11:12Z",
			"score": 0.01639344262295082, "matched_in": []string{"lexical", "semantic"},
			"excerpt": "Lock the shared map before writing.",
		}},
	}
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/search" {
				return false
			}
			writeSearchResponse(t, w, response)
			return true
		},
	})

	output, _, err := executeSearchCmd(t, "related race")
	require.NoError(t, err)
	for _, want := range []string{
		"Mode: lexical",
		"Degraded: embedding provider unavailable",
		"Bounded: candidate ceiling reached",
		"Partial: search coverage is incomplete",
		"Coverage: mirror incomplete (3 pending); vectors building (7 pending, 2 skipped)",
		"Job 42 / Review 9 | acme/widgets | feature/search | fail/open",
		"Score: 0.01639344262295082 | Matched: lexical, semantic",
		"Lock the shared map before writing.",
	} {
		assert.Contains(t, output, want)
	}
	assert.Less(t, strings.Index(output, "Coverage:"), strings.Index(output, "Job 42"))
}

func TestSearchCmdHumanOutputSanitizesDaemonStrings(t *testing.T) {
	response := map[string]any{
		"query": "query", "mode": "\x1b[31mhybrid\x1b[0m\rMODE\nfake\tcell\x00",
		"degraded": true, "degraded_reason": "\x1b]0;owned\x07provider\rdown\nnext\tcol\x01",
		"bounded": true, "bounded_reason": "\x1bP1;2|hidden\x1b\\candidate\nceiling\treached",
		"partial": true,
		"coverage": map[string]any{
			"mirror_complete": false, "mirror_backlog": 3,
			"embeddings_configured": true,
			"vector_state":          "\x1b[2Jactive\rvector\nstate\t\u0085",
			"embedding_backlog":     7, "skipped": 2,
		},
		"hits": []map[string]any{{
			"job_id": 42, "review_id": 9,
			"repo_name": "\x1b]52;c;YQ==\x07acme\r\n\twidgets\x00", "repo_path": "/src/widgets",
			"git_ref": "\x1b[Hfeature\r\n\t/ref", "review_type": "code", "agent": "codex",
			"verdict": "\x1b[31mfail\x1b[0m\r\n\t", "closed": false,
			"finished_at": "2026-09-15T10:11:12Z", "score": 0.5,
			"matched_in": []string{"\x1b[1mlexical\x1b[0m\nleg", "\x1b]0;x\x07semantic\tleg"},
			"excerpt":    "hello\x1b[31mred\x1b[0m\rline\n\tindent\x00tail\u0085\x1b]0;evil\x07",
		}},
	}
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/search" {
				return false
			}
			writeSearchResponse(t, w, response)
			return true
		},
	})

	output, _, err := executeSearchCmd(t, "query")
	require.NoError(t, err)
	for _, want := range []string{
		"Mode: hybridMODEfakecell",
		"Degraded: providerdownnextcol",
		"Bounded: candidateceilingreached",
		"Coverage: mirror incomplete (3 pending); vectors activevectorstate (7 pending, 2 skipped)",
		"Job 42 / Review 9 | acmewidgets | feature/ref | fail/open",
		"Score: 0.5 | Matched: lexicalleg, semanticleg",
		"helloredline\n\tindenttail",
	} {
		assert.Contains(t, output, want)
	}
	assert.NotContains(t, output, "\x1b")
	assert.NotContains(t, output, "\r")
	assert.NotContains(t, output, "\x00")
	assert.NotContains(t, output, "\u0085")
	assert.NotContains(t, output, "hidden")
}

func TestSearchCmdHumanOutputPreservesCosineScorePrecisionAndClosedState(t *testing.T) {
	response := map[string]any{
		"query": "permissions", "mode": "semantic", "degraded": false,
		"bounded": false, "partial": false,
		"coverage": map[string]any{
			"mirror_complete": true, "embeddings_configured": true,
			"vector_state": "active", "embedding_backlog": 0, "skipped": 0,
		},
		"hits": []map[string]any{{
			"job_id": 84, "review_id": 12, "repo_name": "acme/auth", "repo_path": "/src/auth",
			"git_ref": "deadbeef", "review_type": "code", "agent": "claude",
			"verdict": "pass", "closed": true, "finished_at": "2026-09-15T12:00:00Z",
			"score": 0.9234567890123457, "matched_in": []string{"semantic"}, "excerpt": "Check permissions.",
		}},
	}
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/search" {
				return false
			}
			writeSearchResponse(t, w, response)
			return true
		},
	})

	output, _, err := executeSearchCmd(t, "permissions", "--semantic")
	require.NoError(t, err)
	assert.Contains(t, output, "pass/closed")
	assert.Contains(t, output, "0.9234567890123457")
}

func executeSearchCmd(t *testing.T, args ...string) (string, *cobra.Command, error) {
	t.Helper()
	var output bytes.Buffer
	root := &cobra.Command{
		Use:           "roborev",
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return nil
		},
	}
	root.SetOut(&output)
	root.SetErr(&output)
	root.AddCommand(searchCmd())
	root.SetArgs(append([]string{"search"}, args...))
	executed, err := root.ExecuteC()
	return output.String(), executed, err
}

func commandExitCode(err error) int {
	if exitErr, ok := errors.AsType[*exitError](err); ok {
		return exitErr.code
	}
	return 1
}

func emptySearchResponse(query, mode string) map[string]any {
	return map[string]any{
		"query": query, "mode": mode, "degraded": false, "bounded": false, "partial": false,
		"coverage": map[string]any{
			"mirror_complete": true, "embeddings_configured": false,
			"vector_state": "disabled", "embedding_backlog": 0, "skipped": 0,
		},
		"hits": []any{},
	}
}

func writeSearchResponse(t *testing.T, w http.ResponseWriter, response any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(t, json.NewEncoder(w).Encode(response))
}
