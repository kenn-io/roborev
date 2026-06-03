package testenv

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// ErrGitHubAPIBlocked marks a test-time request to the public GitHub API.
var ErrGitHubAPIBlocked = errors.New("blocked GitHub API request during tests")

type githubAPIGuardTransport struct {
	base http.RoundTripper
}

var (
	githubAPIGuardMu                   sync.Mutex
	defaultGitHubAPIGuardBaseTransport = http.DefaultTransport
)

// InstallGitHubAPIGuard blocks Go HTTP requests to api.github.com for tests.
func InstallGitHubAPIGuard() {
	githubAPIGuardMu.Lock()
	defer githubAPIGuardMu.Unlock()

	if GitHubAPIGuardInstalled() {
		return
	}
	base := http.DefaultTransport
	if base == nil {
		base = defaultGitHubAPIGuardBaseTransport
	}
	http.DefaultTransport = githubAPIGuardTransport{base: base}
}

// GitHubAPIGuardInstalled reports whether the test HTTP guard is active.
func GitHubAPIGuardInstalled() bool {
	_, ok := http.DefaultTransport.(githubAPIGuardTransport)
	return ok
}

func (transport githubAPIGuardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil && strings.EqualFold(req.URL.Hostname(), "api.github.com") {
		return nil, fmt.Errorf("%w: %s %s", ErrGitHubAPIBlocked, req.Method, req.URL.Redacted())
	}
	base := transport.base
	if base == nil {
		base = defaultGitHubAPIGuardBaseTransport
	}
	return base.RoundTrip(req)
}
