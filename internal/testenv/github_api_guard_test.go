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

func TestGitHubAPIGuardTransportBlocksAPIGitHub(t *testing.T) {
	baseCalls := 0
	transport := githubAPIGuardTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		baseCalls++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})}
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/rate_limit", nil)
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)

	require.ErrorIs(t, err, ErrGitHubAPIBlocked)
	assert.Nil(t, resp)
	assert.Equal(t, 0, baseCalls)
}

func TestGitHubAPIGuardTransportAllowsOtherHosts(t *testing.T) {
	baseCalls := 0
	transport := githubAPIGuardTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		baseCalls++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})}
	req, err := http.NewRequest(http.MethodGet, "https://example.com/status", nil)
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, 1, baseCalls)
}

func TestInstalledGitHubAPIGuardBlocksDefaultClient(t *testing.T) {
	assert.True(t, GitHubAPIGuardInstalled())

	resp, err := http.Get("https://api.github.com/rate_limit")

	require.ErrorIs(t, err, ErrGitHubAPIBlocked)
	assert.Nil(t, resp)
}

func TestInstallGitHubAPIGuardWrapsDefaultTransportOnce(t *testing.T) {
	original := http.DefaultTransport
	defer func() {
		http.DefaultTransport = original
	}()
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})
	http.DefaultTransport = base

	InstallGitHubAPIGuard()
	InstallGitHubAPIGuard()

	assert.True(t, GitHubAPIGuardInstalled())
	guard, ok := http.DefaultTransport.(githubAPIGuardTransport)
	require.True(t, ok)
	_, nested := guard.base.(githubAPIGuardTransport)
	assert.False(t, nested)
}
