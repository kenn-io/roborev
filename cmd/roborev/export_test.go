package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportReviewsCmdFollowsCursors(t *testing.T) {
	assert := assert.New(t)
	var calls []string
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/reviews" {
				return false
			}
			calls = append(calls, r.URL.RawQuery)
			assert.Equal(http.MethodGet, r.Method)
			assert.Equal("json", r.URL.Query().Get("format"))
			assert.Equal("metadata", r.URL.Query().Get("profile"))
			assert.Equal("2026-06-30", r.URL.Query().Get("until"))
			assert.Equal("true", r.URL.Query().Get("closed_only"))
			assert.Equal("github.com/acme/widgets", r.URL.Query().Get("repo"))
			assert.Equal("widgets", r.URL.Query().Get("project"))
			switch len(calls) {
			case 1:
				assert.Equal("2026-06-29", r.URL.Query().Get("since"))
				assert.Empty(r.URL.Query().Get("cursor"))
				writeExportTestPage(t, w, r.URL.Query().Get("profile"), true, new("cursor-1"), []map[string]any{
					{"review_id": testUUID("review-1"), "source_machine_id": testUUID("machine-a"), "exported_by_machine_id": testUUID("machine-b"), "content": nil},
				})
			case 2:
				assert.Empty(r.URL.Query().Get("since"))
				assert.Equal("cursor-1", r.URL.Query().Get("cursor"))
				writeExportTestPage(t, w, r.URL.Query().Get("profile"), false, new("cursor-2"), []map[string]any{
					{"review_id": testUUID("review-2"), "content": nil},
				})
			default:
				http.Error(w, "too many calls", http.StatusInternalServerError)
			}
			return true
		},
	})

	output := runExportCmd(t,
		"reviews",
		"--profile", "metadata",
		"--since", "2026-06-29",
		"--until", "2026-06-30",
		"--closed-only",
		"--repo", "github.com/acme/widgets",
		"--project", "widgets",
	)

	require.Len(t, calls, 2)
	var got struct {
		Profile    string           `json:"profile"`
		Truncated  bool             `json:"truncated"`
		NextCursor *string          `json:"next_cursor"`
		Reviews    []map[string]any `json:"reviews"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	assert.Equal("metadata", got.Profile)
	assert.False(got.Truncated)
	require.NotNil(t, got.NextCursor)
	assert.Equal("cursor-2", *got.NextCursor)
	require.Len(t, got.Reviews, 2)
	assert.Equal(testUUIDText("review-1"), got.Reviews[0]["review_id"])
	assert.Equal(testUUIDText("review-2"), got.Reviews[1]["review_id"])
	assert.Equal(testUUIDText("machine-a"), got.Reviews[0]["source_machine_id"])
	assert.Equal(testUUIDText("machine-b"), got.Reviews[0]["exported_by_machine_id"])
	assert.NotContains(got.Reviews[1], "source_machine_id")
	assert.NotContains(got.Reviews[1], "exported_by_machine_id")
}

func TestExportReviewsCmdSendsUpdatedSinceOnEveryPage(t *testing.T) {
	assert := assert.New(t)
	var calls int
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/reviews" {
				return false
			}
			calls++
			// --since is replaced by the cursor after the first page, but
			// --updated-since is a filter and must keep applying.
			assert.Equal("2026-07-01T00:00:00Z", r.URL.Query().Get("updated_since"))
			switch calls {
			case 1:
				assert.Equal("2026-01-01", r.URL.Query().Get("since"))
				writeExportTestPage(t, w, "", true, new("cursor-1"), []map[string]any{
					{"review_id": testUUID("review-1"), "closed": true, "updated_at": "2026-07-02T00:00:00Z"},
				})
			case 2:
				assert.Empty(r.URL.Query().Get("since"))
				assert.Equal("cursor-1", r.URL.Query().Get("cursor"))
				writeExportTestPage(t, w, "", false, new("cursor-2"), []map[string]any{
					{"review_id": testUUID("review-2"), "closed": false, "updated_at": "2026-07-03T00:00:00Z"},
				})
			default:
				http.Error(w, "too many calls", http.StatusInternalServerError)
			}
			return true
		},
	})

	output := runExportCmd(t, "reviews", "--since", "2026-01-01", "--updated-since", "2026-07-01T00:00:00Z")

	assert.Equal(2, calls)
	var got struct {
		SchemaVersion int              `json:"schema_version"`
		Reviews       []map[string]any `json:"reviews"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	assert.Equal(2, got.SchemaVersion)
	require.Len(t, got.Reviews, 2)
	assert.Equal(true, got.Reviews[0]["closed"])
	assert.Equal("2026-07-02T00:00:00Z", got.Reviews[0]["updated_at"])
	assert.Equal(false, got.Reviews[1]["closed"])
	assert.Equal("2026-07-03T00:00:00Z", got.Reviews[1]["updated_at"])
}

func TestExportReviewsCmdKeepsDocumentAsJSONObject(t *testing.T) {
	// The daemon exports every document in version 4. One finding is from a
	// reviewing agent; the other has no kind and no location, as goal reviews
	// and converted historical reviews may.
	const document = `{"schema_version":4,"summary":"Two problems.","verdict":"fail","findings":[` +
		`{"severity":"high","kind":"defect","topic":"security","confidence":0.8,"problem":"The token is logged.","fix":"Redact the token.",` +
		`"file":"auth/login.go","start_line":42,"end_line":42,"location":"auth/login.go:42"},` +
		`{"severity":"low","kind":null,"topic":null,"confidence":null,"problem":"Typo in a comment.","fix":"Fix the spelling.",` +
		`"file":null,"start_line":null,"end_line":null,"location":null}]}`
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/reviews" {
				return false
			}
			writeExportTestPage(t, w, "", false, new("cursor-1"), []map[string]any{
				{"review_id": testUUID("review-1"), "document": json.RawMessage(document), "subagents": []map[string]any{
					{"review_id": testUUID("member-1"), "document": json.RawMessage(document)},
				}},
				{"review_id": testUUID("review-2"), "document": nil},
			})
			return true
		},
	})

	output := runExportCmd(t, "reviews")

	var got struct {
		Reviews []struct {
			Document  json.RawMessage `json:"document"`
			Subagents []struct {
				Document json.RawMessage `json:"document"`
			} `json:"subagents"`
		} `json:"reviews"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	require.Len(t, got.Reviews, 2)
	// The CLI decodes and re-encodes each page. The document must come out
	// as the same object, including the null location.
	assert.JSONEq(t, document, string(got.Reviews[0].Document))
	assert.Contains(t, string(got.Reviews[0].Document), `"location"`)
	require.Len(t, got.Reviews[0].Subagents, 1)
	assert.JSONEq(t, document, string(got.Reviews[0].Subagents[0].Document))
	assert.Equal(t, "null", string(got.Reviews[1].Document))
}

func TestExportReviewsCmdLimitStopsAtCursor(t *testing.T) {
	assert := assert.New(t)
	var calls []string
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/reviews" {
				return false
			}
			calls = append(calls, r.URL.RawQuery)
			assert.Equal("1", r.URL.Query().Get("limit"))
			writeExportTestPage(t, w, r.URL.Query().Get("profile"), true, new("next-page"), []map[string]any{
				{"review_id": testUUID("review-1"), "content": "raw"},
			})
			return true
		},
	})

	output := runExportCmd(t, "reviews", "--limit", "1")

	require.Len(t, calls, 1)
	var got struct {
		Profile    string           `json:"profile"`
		Truncated  bool             `json:"truncated"`
		NextCursor *string          `json:"next_cursor"`
		Reviews    []map[string]any `json:"reviews"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	assert.Equal("content", got.Profile)
	assert.True(got.Truncated)
	require.NotNil(t, got.NextCursor)
	assert.Equal("next-page", *got.NextCursor)
	require.Len(t, got.Reviews, 1)
	assert.Equal("raw", got.Reviews[0]["content"])
}

func TestExportReviewsCmdStartsFromCursor(t *testing.T) {
	assert := assert.New(t)
	var calls []string
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/reviews" {
				return false
			}
			calls = append(calls, r.URL.RawQuery)
			assert.Equal("opaque-cursor", r.URL.Query().Get("cursor"))
			assert.Empty(r.URL.Query().Get("since"))
			assert.Equal("2026-06-30", r.URL.Query().Get("until"))
			assert.Equal("metadata", r.URL.Query().Get("profile"))
			writeExportTestPage(t, w, r.URL.Query().Get("profile"), false, new("resume-r2"), []map[string]any{
				{"review_id": testUUID("review-2"), "content": nil},
			})
			return true
		},
	})

	output := runExportCmd(t,
		"reviews",
		"--profile", "metadata",
		"--cursor", "opaque-cursor",
		"--until", "2026-06-30",
	)

	require.Len(t, calls, 1)
	var got struct {
		DatabaseID string           `json:"database_id"`
		NextCursor *string          `json:"next_cursor"`
		Reviews    []map[string]any `json:"reviews"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	assert.Equal(testUUIDText("export-database"), got.DatabaseID)
	require.NotNil(t, got.NextCursor)
	assert.Equal("resume-r2", *got.NextCursor)
	require.Len(t, got.Reviews, 1)
	assert.Equal(testUUIDText("review-2"), got.Reviews[0]["review_id"])
}

func TestExportReviewsCmdExplicitLargeLimitFollowsUntilLimit(t *testing.T) {
	assert := assert.New(t)
	var calls []string
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/reviews" {
				return false
			}
			calls = append(calls, r.URL.RawQuery)
			switch len(calls) {
			case 1:
				assert.Equal("5000", r.URL.Query().Get("limit"))
				assert.Empty(r.URL.Query().Get("cursor"))
				writeExportTestPage(t, w, r.URL.Query().Get("profile"), true, new("first-page"), exportTestReviews(5000, 0))
			case 2:
				assert.Equal("1000", r.URL.Query().Get("limit"))
				assert.Equal("first-page", r.URL.Query().Get("cursor"))
				writeExportTestPage(t, w, r.URL.Query().Get("profile"), true, new("second-page"), exportTestReviews(1000, 5000))
			default:
				http.Error(w, "too many calls", http.StatusInternalServerError)
			}
			return true
		},
	})

	output := runExportCmd(t, "reviews", "--limit", "6000")

	require.Len(t, calls, 2)
	var got struct {
		Truncated  bool             `json:"truncated"`
		NextCursor *string          `json:"next_cursor"`
		Reviews    []map[string]any `json:"reviews"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	assert.True(got.Truncated)
	require.NotNil(t, got.NextCursor)
	assert.Equal("second-page", *got.NextCursor)
	require.Len(t, got.Reviews, 6000)
	assert.Equal(testUUIDText("review-0000"), got.Reviews[0]["review_id"])
	assert.Equal(testUUIDText("review-5999"), got.Reviews[5999]["review_id"])
}

func TestExportReviewsCmdRejectsInvalidFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		code int
	}{
		{name: "format", code: 4, args: []string{"reviews", "--format", "yaml"}},
		{name: "profile", code: 4, args: []string{"reviews", "--profile", "full"}},
		{name: "schema-version-zero", code: 4, args: []string{"reviews", "--schema-version", "0"}},
		{name: "schema-version-negative", code: 4, args: []string{"reviews", "--schema-version", "-1"}},
		{name: "schema-version-non-integer", code: 4, args: []string{"reviews", "--schema-version", "foo"}},
		{name: "schema-version-empty", code: 4, args: []string{"reviews", "--schema-version", ""}},
		{name: "schema-version-float", code: 4, args: []string{"reviews", "--schema-version", "2.0"}},
		{name: "schema-version-spaces", code: 4, args: []string{"reviews", "--schema-version", " 2"}},
		{name: "limit", code: 4, args: []string{"reviews", "--limit", "0"}},
		{name: "cursor-since", code: 2, args: []string{"reviews", "--cursor", "opaque", "--since", "2026-06-29"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exportCmd()
			cmd.SetArgs(tt.args)
			executed, err := cmd.ExecuteC()
			require.Error(t, err)
			assert.True(t, executed.SilenceUsage)
			assert.Equal(t, tt.code, commandExitCode(err))
		})
	}
}

func TestExportReviewsCmdSendsSchemaVersionOnlyWhenExplicit(t *testing.T) {
	assert := assert.New(t)
	var seen []string
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/reviews" {
				return false
			}
			seen = append(seen, r.URL.Query().Get("schema_version"))
			writeExportTestPage(t, w, r.URL.Query().Get("profile"), false, new("cursor-1"), []map[string]any{
				{"review_id": testUUID("review-1"), "content": nil},
			})
			return true
		},
	})

	runExportCmd(t, "reviews")
	runExportCmd(t, "reviews", "--schema-version", "2")

	assert.Equal([]string{"", "2"}, seen)
}

func TestExportReviewsCmdEnforcesPinnedSchemaVersionAcrossPages(t *testing.T) {
	t.Run("rejects page 1 mismatch with exit code 4", func(t *testing.T) {
		NewMockDaemon(t, MockRefineHooks{
			OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
				if r.URL.Path != "/api/export/reviews" {
					return false
				}
				writeExportTestPageWithVersion(t, w, 1, r.URL.Query().Get("profile"), false, new("cursor-1"), []map[string]any{
					{"review_id": testUUID("review-1")},
				})
				return true
			},
		})

		cmd := exportCmd()
		cmd.SetArgs([]string{"reviews", "--schema-version", "2"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, 4, commandExitCode(err))
		assert.Contains(t, err.Error(), "schema_version 1, requested 2")
	})

	t.Run("rejects continuation page mismatch with exit code 4", func(t *testing.T) {
		var calls int
		NewMockDaemon(t, MockRefineHooks{
			OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
				if r.URL.Path != "/api/export/reviews" {
					return false
				}
				calls++
				if calls == 1 {
					writeExportTestPageWithVersion(t, w, 2, r.URL.Query().Get("profile"), true, new("cursor-1"), []map[string]any{
						{"review_id": testUUID("review-1")},
					})
					return true
				}
				writeExportTestPageWithVersion(t, w, 1, r.URL.Query().Get("profile"), false, new("cursor-2"), []map[string]any{
					{"review_id": testUUID("review-2")},
				})
				return true
			},
		})

		cmd := exportCmd()
		cmd.SetArgs([]string{"reviews", "--schema-version", "2"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, 4, commandExitCode(err))
		assert.Contains(t, err.Error(), "schema_version 1, requested 2")
	})

	t.Run("allows older daemon default when flag is omitted", func(t *testing.T) {
		NewMockDaemon(t, MockRefineHooks{
			OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
				if r.URL.Path != "/api/export/reviews" {
					return false
				}
				assert.Empty(t, r.URL.Query().Get("schema_version"))
				writeExportTestPageWithVersion(t, w, 1, r.URL.Query().Get("profile"), false, new("cursor-1"), []map[string]any{
					{"review_id": testUUID("review-1")},
				})
				return true
			},
		})

		output := runExportCmd(t, "reviews")
		var got struct {
			SchemaVersion int `json:"schema_version"`
		}
		require.NoError(t, json.Unmarshal([]byte(output), &got))
		assert.Equal(t, 1, got.SchemaVersion)
	})

	t.Run("rejects mid-export schema_version drift across pages with exit code 4", func(t *testing.T) {
		var calls int
		NewMockDaemon(t, MockRefineHooks{
			OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
				if r.URL.Path != "/api/export/reviews" {
					return false
				}
				calls++
				if calls == 1 {
					writeExportTestPageWithVersion(t, w, 2, "content", true, new("cursor-1"), []map[string]any{
						{"review_id": testUUID("review-1")},
					})
					return true
				}
				writeExportTestPageWithVersion(t, w, 3, "content", false, new("cursor-2"), []map[string]any{
					{"review_id": testUUID("review-2")},
				})
				return true
			},
		})

		cmd := exportCmd()
		cmd.SetArgs([]string{"reviews"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, 4, commandExitCode(err))
		assert.Contains(t, err.Error(), "changed export schema_version mid-export")
	})

	t.Run("rejects mid-export database_id drift with cursor reset exit code 3", func(t *testing.T) {
		var calls int
		NewMockDaemon(t, MockRefineHooks{
			OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
				if r.URL.Path != "/api/export/reviews" {
					return false
				}
				calls++
				if calls == 1 {
					writeExportTestPageWithVersion(t, w, 2, "content", true, new("cursor-1"), []map[string]any{
						{"review_id": testUUID("review-1")},
					})
					return true
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
					"schema_version": 2,
					"tool":           "roborev",
					"tool_version":   "dev",
					"generated_at":   "2026-06-29T00:00:00Z",
					"database_id":    testUUID("other-database"),
					"profile":        "content",
					"window":         map[string]any{"field": "completed_at", "since": nil, "until": nil},
					"truncated":      false,
					"next_cursor":    "cursor-2",
					"reviews":        []map[string]any{{"review_id": testUUID("review-2")}},
				}))
				return true
			},
		})

		cmd := exportCmd()
		cmd.SetArgs([]string{"reviews"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, exportReviewsCursorResetExitCode, commandExitCode(err))
		assert.Contains(t, err.Error(), "changed export database_id mid-export")
	})

	t.Run("rejects mid-export profile drift", func(t *testing.T) {
		var calls int
		NewMockDaemon(t, MockRefineHooks{
			OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
				if r.URL.Path != "/api/export/reviews" {
					return false
				}
				calls++
				if calls == 1 {
					writeExportTestPageWithVersion(t, w, 2, "content", true, new("cursor-1"), []map[string]any{
						{"review_id": testUUID("review-1")},
					})
					return true
				}
				writeExportTestPageWithVersion(t, w, 2, "metadata", false, new("cursor-2"), []map[string]any{
					{"review_id": testUUID("review-2")},
				})
				return true
			},
		})

		cmd := exportCmd()
		cmd.SetArgs([]string{"reviews"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, 1, commandExitCode(err))
		assert.Contains(t, err.Error(), "changed export profile mid-export")
	})

	t.Run("allows pinning a future daemon schema version", func(t *testing.T) {
		NewMockDaemon(t, MockRefineHooks{
			OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
				if r.URL.Path != "/api/export/reviews" {
					return false
				}
				assert.Equal(t, "3", r.URL.Query().Get("schema_version"))
				writeExportTestPageWithVersion(t, w, 3, "content", false, new("cursor-1"), []map[string]any{
					{"review_id": testUUID("review-1"), "v3_only_field": "ok"},
				})
				return true
			},
		})

		output := runExportCmd(t, "reviews", "--schema-version", "3")
		var got map[string]any
		require.NoError(t, json.Unmarshal([]byte(output), &got))
		assert.InDelta(t, 3, got["schema_version"], 0)
	})

	t.Run("maps daemon problem+json schema_version HTTP 400 to exit code 4", func(t *testing.T) {
		NewMockDaemon(t, MockRefineHooks{
			OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
				if r.URL.Path != "/api/export/reviews" {
					return false
				}
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusBadRequest)
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
					"status": http.StatusBadRequest,
					"detail": "unsupported export schema_version 99 (supported: 2)",
				}))
				return true
			},
		})

		cmd := exportCmd()
		cmd.SetArgs([]string{"reviews", "--schema-version", "99"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, 4, commandExitCode(err))
		assert.Contains(t, err.Error(), "unsupported export schema_version")
	})

	t.Run("does not misclassify cursor HTTP 400 mentioning schema_version as exit code 4", func(t *testing.T) {
		NewMockDaemon(t, MockRefineHooks{
			OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
				if r.URL.Path != "/api/export/reviews" {
					return false
				}
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusBadRequest)
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
					"status": http.StatusBadRequest,
					"detail": `invalid export cursor: duplicate object name "schema_version"`,
				}))
				return true
			},
		})

		cmd := exportCmd()
		cmd.SetArgs([]string{"reviews", "--schema-version", "2", "--cursor", "corrupt"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Equal(t, 1, commandExitCode(err))
	})

	t.Run("retains prior cursor when final continuation page is empty", func(t *testing.T) {
		var calls int
		NewMockDaemon(t, MockRefineHooks{
			OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
				if r.URL.Path != "/api/export/reviews" {
					return false
				}
				calls++
				if calls == 1 {
					writeExportTestPageWithVersion(t, w, 2, "metadata", true, new("cursor-page-1"), []map[string]any{
						{"review_id": testUUID("review-1")},
					})
					return true
				}
				writeExportTestPageWithVersion(t, w, 2, "metadata", false, nil, []map[string]any{})
				return true
			},
		})

		output := runExportCmd(t, "reviews", "--profile", "metadata", "--closed-only")
		var got struct {
			Truncated  bool             `json:"truncated"`
			NextCursor *string          `json:"next_cursor"`
			Reviews    []map[string]any `json:"reviews"`
		}
		require.NoError(t, json.Unmarshal([]byte(output), &got))
		assert.False(t, got.Truncated)
		require.NotNil(t, got.NextCursor)
		assert.Equal(t, "cursor-page-1", *got.NextCursor)
		require.Len(t, got.Reviews, 1)
	})

	t.Run("rejects HTTP 200 null body without panicking", func(t *testing.T) {
		NewMockDaemon(t, MockRefineHooks{
			OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
				if r.URL.Path != "/api/export/reviews" {
					return false
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("null"))
				return true
			},
		})

		cmd := exportCmd()
		cmd.SetArgs([]string{"reviews"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected JSON object")
	})
}

func TestExportReviewsCmdPreservesAdditiveFieldsAndNestedDocument(t *testing.T) {
	// Simulate a newer daemon emitting additive fields on the envelope, review,
	// subagent, and nested document (including an unknown field on a finding),
	// plus Huma's transport-level $schema link which must be stripped.
	const futureDoc = `{"schema_version":3,"summary":"All good.","verdict":"pass","extra_doc_field":"keep-me","findings":[{"severity":"low","problem":"Minor","fix":"Fix","location":null,"extra_finding_field":123}]}`
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/reviews" {
				return false
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"$schema":          "http://127.0.0.1:7373/schemas/ExportReviewsDocument.json",
				"schema_version":   2,
				"tool":             "roborev",
				"tool_version":     "dev",
				"generated_at":     "2026-06-29T00:00:00Z",
				"database_id":      testUUID("export-database"),
				"profile":          "content",
				"window":           map[string]any{"field": "completed_at", "since": nil, "until": nil, "extra_window_field": true},
				"truncated":        false,
				"next_cursor":      "cursor-1",
				"extra_header_key": "preserve-me",
				"reviews": []map[string]any{{
					"review_id":          testUUID("review-1"),
					"extra_review_field": "keep-review",
					"document":           json.RawMessage(futureDoc),
					"subagents": []map[string]any{{
						"review_id":            testUUID("member-1"),
						"extra_subagent_field": 42,
						"document":             json.RawMessage(futureDoc),
					}},
				}},
			}))
			return true
		},
	})

	output := runExportCmd(t, "reviews", "--schema-version", "2")

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	assert.NotContains(t, got, "$schema", "transport-level $schema URL must be stripped")
	assert.Equal(t, "preserve-me", got["extra_header_key"])
	window, ok := got["window"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, window["extra_window_field"])

	reviews, ok := got["reviews"].([]any)
	require.True(t, ok)
	require.Len(t, reviews, 1)
	review, ok := reviews[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "keep-review", review["extra_review_field"])
	doc, ok := review["document"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "keep-me", doc["extra_doc_field"])
	findings, ok := doc["findings"].([]any)
	require.True(t, ok)
	require.Len(t, findings, 1)
	finding, ok := findings[0].(map[string]any)
	require.True(t, ok)
	assert.InDelta(t, 123, finding["extra_finding_field"], 0)

	subagents, ok := review["subagents"].([]any)
	require.True(t, ok)
	require.Len(t, subagents, 1)
	sub, ok := subagents[0].(map[string]any)
	require.True(t, ok)
	assert.InDelta(t, 42, sub["extra_subagent_field"], 0)
}

func TestExportReviewsCmdPropagatesCursorErrors(t *testing.T) {
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/reviews" {
				return false
			}
			assert.Equal(t, "corrupt", r.URL.Query().Get("cursor"))
			http.Error(w, "invalid export cursor: illegal base64 data", http.StatusBadRequest)
			return true
		},
	})

	cmd := exportCmd()
	cmd.SetArgs([]string{"reviews", "--cursor", "corrupt"})
	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon returned 400 Bad Request")
	assert.Contains(t, err.Error(), "invalid export cursor")
}

func TestExportReviewsCmdUsesDistinctExitCodeForCursorDatabaseReset(t *testing.T) {
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/reviews" {
				return false
			}
			assert.Equal(t, "old-cursor", r.URL.Query().Get("cursor"))
			http.Error(w, "export cursor database reset", http.StatusConflict)
			return true
		},
	})

	cmd := exportCmd()
	cmd.SetArgs([]string{"reviews", "--cursor", "old-cursor"})
	err := cmd.Execute()

	require.Error(t, err)
	var exitErr *exitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, exportReviewsCursorResetExitCode, exitErr.code)
	assert.Contains(t, err.Error(), "daemon returned 409 Conflict")
	assert.Contains(t, err.Error(), "database reset")
}

func runExportCmd(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := exportCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	require.NoError(t, cmd.Execute())
	return out.String()
}

func writeExportTestPage(t *testing.T, w http.ResponseWriter, profile string, truncated bool, nextCursor *string, reviews []map[string]any) {
	t.Helper()
	writeExportTestPageWithVersion(t, w, 2, profile, truncated, nextCursor, reviews)
}

func writeExportTestPageWithVersion(t *testing.T, w http.ResponseWriter, schemaVersion int, profile string, truncated bool, nextCursor *string, reviews []map[string]any) {
	t.Helper()
	if profile == "" {
		profile = "content"
	}
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
		"schema_version": schemaVersion,
		"tool":           "roborev",
		"tool_version":   "dev",
		"generated_at":   "2026-06-29T00:00:00Z",
		"database_id":    testUUID("export-database"),
		"profile":        profile,
		"window": map[string]any{
			"field": "completed_at",
			"since": nil,
			"until": nil,
		},
		"truncated":   truncated,
		"next_cursor": nextCursor,
		"reviews":     reviews,
	}))
}

func exportTestReviews(n, offset int) []map[string]any {
	reviews := make([]map[string]any, n)
	for i := range n {
		reviews[i] = map[string]any{
			"review_id": testUUID(fmt.Sprintf("review-%04d", offset+i)),
		}
	}
	return reviews
}
