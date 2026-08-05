package testenv

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func newCountingGuard(calls *int) forgeAPIGuardTransport {
	return forgeAPIGuardTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		*calls++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})}
}

func TestForgeAPIGuardTransportBlocksForgeHosts(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr error
	}{
		{"GitHubAPI", "https://api.github.com/rate_limit", ErrGitHubAPIBlocked},
		{"GitHubAPIMixedCase", "https://API.GitHub.com/rate_limit", ErrGitHubAPIBlocked},
		{"GitLabAPI", "https://gitlab.com/api/v4/projects/1", ErrGitLabAPIBlocked},
		{"GitLabAPIMixedCase", "https://GitLab.com/api/v4/projects/1", ErrGitLabAPIBlocked},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseCalls := 0
			transport := newCountingGuard(&baseCalls)
			req, err := http.NewRequest(http.MethodGet, tt.url, nil)
			require.NoError(t, err)

			resp, err := transport.RoundTrip(req)

			require.ErrorIs(t, err, tt.wantErr)
			assert.Nil(t, resp)
			assert.Equal(t, 0, baseCalls)
		})
	}
}

func TestForgeAPIGuardTransportAllowsOtherHosts(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"UnrelatedHost", "https://example.com/status"},
		{"SelfHostedGitLab", "https://gitlab.example.com/api/v4/projects/1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseCalls := 0
			transport := newCountingGuard(&baseCalls)
			req, err := http.NewRequest(http.MethodGet, tt.url, nil)
			require.NoError(t, err)

			resp, err := transport.RoundTrip(req)

			require.NoError(t, err)
			require.NotNil(t, resp)
			assert.Equal(t, http.StatusNoContent, resp.StatusCode)
			assert.Equal(t, 1, baseCalls)
		})
	}
}

func TestInstalledForgeAPIGuardBlocksDefaultClient(t *testing.T) {
	assert.True(t, ForgeAPIGuardInstalled())

	resp, err := http.Get("https://api.github.com/rate_limit")
	require.ErrorIs(t, err, ErrGitHubAPIBlocked)
	assert.Nil(t, resp)

	resp, err = http.Get("https://gitlab.com/api/v4/version")
	require.ErrorIs(t, err, ErrGitLabAPIBlocked)
	assert.Nil(t, resp)
}

func TestInstallForgeAPIGuardWrapsDefaultTransportOnce(t *testing.T) {
	original := http.DefaultTransport
	defer func() {
		http.DefaultTransport = original
	}()
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})
	http.DefaultTransport = base

	InstallForgeAPIGuard()
	InstallForgeAPIGuard()

	assert.True(t, ForgeAPIGuardInstalled())
	guard, ok := http.DefaultTransport.(forgeAPIGuardTransport)
	require.True(t, ok)
	_, nested := guard.base.(forgeAPIGuardTransport)
	assert.False(t, nested)
}
