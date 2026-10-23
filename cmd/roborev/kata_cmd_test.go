package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestKataCanonicalCommandDiscovery(t *testing.T) {
	cmd := kataCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--help"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, output.String(), "create")
	assert.Contains(t, output.String(), "show")
}

func TestKataCommandsUseDaemonPublicationAPI(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	repoPath, err := filepath.EvalSymlinks(repo.Path())
	require.NoError(t, err)
	repo.RunGit("branch", "42")
	t.Chdir(repoPath)
	for _, operation := range []string{"create", "show"} {
		for _, target := range []struct {
			name   string
			args   []string
			lookup bool
		}{{"id", []string{"43"}, false}, {"ref", []string{"HEAD"}, true}, {"numeric-ref", []string{"42"}, true}, {"forced-id", []string{"--job", "42"}, false}} {
			t.Run(operation+"/"+target.name, func(t *testing.T) {
				var requested, lookedUp atomic.Bool
				jobID := int64(42)
				if target.name == "id" {
					jobID = 43
				}
				body := daemon.KataPublicationsBody{JobID: jobID, Publications: []daemon.KataPublication{{OperationID: "publish:example", State: "pending", Policy: "summary", Findings: []daemon.KataFindingPublication{}}}}
				NewMockDaemon(t, MockRefineHooks{
					OnGetJobs: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
						lookedUp.Store(true)
						assert.Equal(t, repo.HeadSHA(), r.URL.Query().Get("git_ref"))
						assert.Equal(t, repoPath, r.URL.Query().Get("repo"))
						_ = json.NewEncoder(w).Encode(map[string]any{"jobs": []storage.ReviewJob{{ID: jobID, JobType: storage.JobTypeReview}}})
						return true
					},
					OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
						path, method := "/api/kata/publications", http.MethodGet
						if operation == "create" {
							path, method = "/api/kata/publish", http.MethodPost
						}
						if r.URL.Path != path {
							return false
						}
						requested.Store(true)
						assert.Equal(t, method, r.Method)
						if operation == "create" {
							var req daemon.KataPublishRequest
							require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
							assert.Equal(t, jobID, req.JobID)
						} else {
							assert.Equal(t, strconv.FormatInt(jobID, 10), r.URL.Query().Get("job_id"))
						}
						_ = json.NewEncoder(w).Encode(body)
						return true
					},
				})
				cmd := kataCmd()
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetArgs(append(append([]string{operation}, target.args...), "--json"))
				require.NoError(t, cmd.Execute())
				assert.True(t, requested.Load())
				assert.Equal(t, target.lookup, lookedUp.Load())
				var got daemon.KataPublicationsBody
				require.NoError(t, json.Unmarshal(out.Bytes(), &got))
				assert.Equal(t, body, got)
			})
		}
	}
}

func TestKataCommandsRejectInvalidSelectors(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	patchServerAddr(t, server.URL)
	for _, operation := range []string{"create", "show"} {
		for _, test := range []struct {
			args      []string
			errorText string
		}{
			{nil, "accepts 1 arg(s), received 0"},
			{[]string{"--job"}, "accepts 1 arg(s), received 0"},
			{[]string{"--job", "0"}, `argument "0" is not a valid git ref or positive job ID`},
			{[]string{"--job", "abc"}, `argument "abc" is not a valid git ref or positive job ID`},
			{[]string{"--job=42"}, `invalid argument "42" for "--job" flag`},
			{[]string{"42", "43"}, "accepts 1 arg(s), received 2"},
		} {
			cmd := kataCmd()
			cmd.SetArgs(append([]string{operation}, test.args...))
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			require.ErrorContains(t, cmd.Execute(), test.errorText)
			assert.Zero(t, requests.Load(), "invalid selectors must fail before probing or requesting the daemon")
		}
	}
}

func TestShowDoesNotFetchKata(t *testing.T) {
	review := structuredReviewFixture(t, showFindingsDocument)
	findings := showSampleFindings(t, review)
	var providerRequests atomic.Int32
	NewMockDaemon(t, MockRefineHooks{
		OnReview: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
			_ = json.NewEncoder(w).Encode(review)
			return true
		},
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if strings.HasPrefix(r.URL.Path, "/api/kata/") {
				providerRequests.Add(1)
				_ = json.NewEncoder(w).Encode(daemon.KataPublicationsBody{JobID: 42, Publications: []daemon.KataPublication{{ReviewUUID: review.UUID.String(), Findings: []daemon.KataFindingPublication{{ID: "f:example"}}}}})
				return true
			}
			if r.URL.Path != "/api/review/findings" {
				return false
			}
			_ = json.NewEncoder(w).Encode(findings)
			return true
		},
	})
	for _, args := range [][]string{{"--job", "42", "--json"}, {"--job", "42"}} {
		output := runShowCmd(t, args...)
		if strings.Contains(strings.Join(args, " "), "--json") {
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(output), &got))
			assert.NotContains(t, got, "kata")
			assert.NotEmpty(t, got["findings"])
		}
	}
	assert.Zero(t, providerRequests.Load())
}
