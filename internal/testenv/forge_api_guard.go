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

// ErrGitLabAPIBlocked marks a test-time request to the public GitLab API.
var ErrGitLabAPIBlocked = errors.New("blocked GitLab API request during tests")

// blockedForgeHosts maps a public forge host to the error returned when a
// test tries to reach it. Hosts are compared case-insensitively.
var blockedForgeHosts = map[string]error{
	"api.github.com": ErrGitHubAPIBlocked,
	"gitlab.com":     ErrGitLabAPIBlocked,
}

type forgeAPIGuardTransport struct {
	base http.RoundTripper
}

var (
	forgeAPIGuardMu                   sync.Mutex
	defaultForgeAPIGuardBaseTransport = http.DefaultTransport
)

// InstallForgeAPIGuard blocks Go HTTP requests to the public forge APIs
// (api.github.com, gitlab.com) for tests.
func InstallForgeAPIGuard() {
	forgeAPIGuardMu.Lock()
	defer forgeAPIGuardMu.Unlock()

	if ForgeAPIGuardInstalled() {
		return
	}
	base := http.DefaultTransport
	if base == nil {
		base = defaultForgeAPIGuardBaseTransport
	}
	http.DefaultTransport = forgeAPIGuardTransport{base: base}
}

// ForgeAPIGuardInstalled reports whether the test HTTP guard is active.
func ForgeAPIGuardInstalled() bool {
	_, ok := http.DefaultTransport.(forgeAPIGuardTransport)
	return ok
}

// blockedForgeHost returns the block error for a host, or nil when the host
// is allowed.
func blockedForgeHost(host string) error {
	for blocked, blockErr := range blockedForgeHosts {
		if strings.EqualFold(host, blocked) {
			return blockErr
		}
	}
	return nil
}

func (transport forgeAPIGuardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil {
		if blockErr := blockedForgeHost(req.URL.Hostname()); blockErr != nil {
			return nil, fmt.Errorf("%w: %s %s", blockErr, req.Method, req.URL.Redacted())
		}
	}
	base := transport.base
	if base == nil {
		base = defaultForgeAPIGuardBaseTransport
	}
	return base.RoundTrip(req)
}
