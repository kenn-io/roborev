package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	googlegithub "github.com/google/go-github/v91/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghpkg "go.kenn.io/roborev/internal/github"
	"go.kenn.io/roborev/internal/storage"
)

type diagnosticTransport func(*http.Request) (*http.Response, error)

func (f diagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCIHealthSafeReadDiagnosticsAndRecovery(t *testing.T) {
	for _, category := range []string{"HTTP 503", "timeout", "network interruption", "rate limit (HTTP 429)"} {
		t.Run(category, func(t *testing.T) {
			var readErr error
			synctest.Test(t, func(t *testing.T) {
				client, err := ghpkg.NewClient("synthetic-token", ghpkg.WithHTTPClient(&http.Client{Timeout: time.Second, Transport: diagnosticTransport(func(r *http.Request) (*http.Response, error) {
					if category == "timeout" {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					if category == "network interruption" {
						return nil, io.ErrUnexpectedEOF
					}
					status := 503
					if category == "rate limit (HTTP 429)" {
						status = 429
					}
					return &http.Response{StatusCode: status, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"sensitive-provider-text synthetic-token"}`))}, nil
				})}))
				require.NoError(t, err)
				_, readErr = client.ListOpenPullRequests(t.Context(), "acme/api")
				require.Error(t, readErr)
			})
			h, server := newCIHealthHarness(t)
			h.Cfg.CI.Repos = []string{"acme/api"}
			logPath := filepath.Join(t.TempDir(), "errors.log")
			errorLog, err := NewErrorLog(logPath)
			require.NoError(t, err)
			require.NoError(t, server.errorLog.Close())
			server.errorLog = errorLog
			h.Poller.errorLog = errorLog
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, readErr }
			h.Poller.poll(t.Context())
			health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
			message := "polling failed for acme/api: list pull requests: " + category
			assert.False(t, health.Healthy)
			assert.False(t, health.Ready)
			assert.Contains(t, health.Components, storage.ComponentHealth{Name: "ci", Healthy: false, Message: message})
			require.NotEmpty(t, health.RecentErrors)
			assert.Contains(t, health.RecentErrors[0].Message, message)
			data, err := os.ReadFile(logPath)
			require.NoError(t, err)
			assert.Contains(t, string(data), message)
			assert.NotContains(t, string(data), "sensitive-provider-text")
			assert.NotContains(t, string(data), "synthetic-token")
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, nil }
			h.Poller.poll(t.Context())
			health = decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
			assert.True(t, health.Healthy)
			assert.True(t, health.Ready)
			assert.NotEmpty(t, health.RecentErrors)
		})
	}
}

func TestCIHealthSafeDiscoveryDiagnosticsAndRecovery(t *testing.T) {
	var readErr error
	synctest.Test(t, func(t *testing.T) {
		client, err := ghpkg.NewClient("", ghpkg.WithHTTPClient(&http.Client{Transport: diagnosticTransport(func(r *http.Request) (*http.Response, error) {
			assert.Equal(t, "/orgs/acme/repos", r.URL.Path)
			return &http.Response{StatusCode: 503, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"sensitive-provider-text"}`))}, nil
		})}))
		require.NoError(t, err)
		_, readErr = client.ListOwnerRepos(t.Context(), "acme", 100)
		require.Error(t, readErr)
	})
	h, server := newCIHealthHarness(t)
	h.Cfg.CI.Repos = []string{"acme/*"}
	h.Poller.repoResolver.listReposFn = func(context.Context, string, string) ([]string, error) { return nil, readErr }
	h.Poller.poll(t.Context())
	health := decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet))
	assert.False(t, health.Healthy)
	assert.Contains(t, health.Components, storage.ComponentHealth{Name: "ci", Healthy: false, Message: "repository discovery failed: list organization repositories: HTTP 503"})
	require.NotEmpty(t, health.RecentErrors)
	assert.Contains(t, health.RecentErrors[0].Message, "list organization repositories: HTTP 503")
	assert.NotContains(t, health.RecentErrors[0].Message, "sensitive-provider-text")
	h.Poller.repoResolver.listReposFn = func(context.Context, string, string) ([]string, error) { return []string{"acme/api"}, nil }
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return nil, nil }
	h.Poller.poll(t.Context())
	assert.True(t, decodeHealthStatus(t, executeHealthCheck(server, http.MethodGet)).Healthy)
	h.Poller.repoResolver.mu.Lock()
	h.Poller.repoResolver.cachedAt = time.Now().Add(-2 * time.Hour)
	h.Poller.repoResolver.mu.Unlock()
	h.Poller.repoResolver.listReposFn = func(context.Context, string, string) ([]string, error) { return nil, readErr }
	repos, err := h.Poller.repoResolver.Resolve(t.Context(), &h.Cfg.CI, nil)
	require.ErrorIs(t, err, errRepoDiscoveryIncomplete)
	assert.Equal(t, []string{"acme/api"}, repos)
	responseError, ok := errors.AsType[*googlegithub.ErrorResponse](err)
	require.True(t, ok)
	assert.Equal(t, 503, responseError.Response.StatusCode)
	assert.Equal(t, "list organization repositories: HTTP 503", ghpkg.ReadErrorSummary(err))
}
