package gitlab

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghpkg "go.kenn.io/roborev/internal/github"
)

// stubGlabAuthToken replaces the `glab auth token` fallback for a single test.
func stubGlabAuthToken(t *testing.T, token string, err error) *string {
	t.Helper()
	original := glabAuthTokenFn
	t.Cleanup(func() { glabAuthTokenFn = original })

	var gotHost string
	glabAuthTokenFn = func(_ context.Context, hostname string) (string, error) {
		gotHost = hostname
		return token, err
	}
	return &gotHost
}

// clearGitLabEnv removes every env var the resolvers look at so tests start
// from a known state.
func clearGitLabEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"GITLAB_TOKEN", "GL_TOKEN", "CI_SERVER_URL", "GITLAB_HOST", "GL_HOST",
	} {
		t.Setenv(key, "")
	}
}

func TestCommentMarkerMatchesGitHub(t *testing.T) {
	// Both forges must use the same marker so comment bodies stay
	// byte-identical across GitHub and GitLab.
	assert.Equal(t, ghpkg.CommentMarker, CommentMarker)
}

func TestEnvironmentToken(t *testing.T) {
	tests := []struct {
		name        string
		gitlabToken string
		glToken     string
		want        string
	}{
		{"NoneSet", "", "", ""},
		{"GitLabToken", "gitlab-token", "", "gitlab-token"},
		{"GLToken", "", "gl-token", "gl-token"},
		{"GitLabTokenWins", "gitlab-token", "gl-token", "gitlab-token"},
		{"TrimsWhitespace", "  spaced  ", "", "spaced"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGitLabEnv(t)
			t.Setenv("GITLAB_TOKEN", tt.gitlabToken)
			t.Setenv("GL_TOKEN", tt.glToken)

			assert.Equal(t, tt.want, EnvironmentToken())
		})
	}
}

func TestResolveAuthToken_ExplicitTokenSkipsCLI(t *testing.T) {
	clearGitLabEnv(t)
	gotHost := stubGlabAuthToken(t, "cli-token", nil)

	token := ResolveAuthToken(context.Background(), "explicit", "gitlab.com")

	assert.Equal(t, "explicit", token)
	assert.Empty(t, *gotHost, "glab CLI must not be consulted")
}

func TestResolveAuthToken_FallsBackToGlabCLI(t *testing.T) {
	clearGitLabEnv(t)
	gotHost := stubGlabAuthToken(t, "  cli-token\n", nil)

	token := ResolveAuthToken(context.Background(), EnvironmentToken(), "gitlab.example.com")

	assert.Equal(t, "cli-token", token)
	assert.Equal(t, "gitlab.example.com", *gotHost)
}

func TestResolveAuthToken_CLIFailureReturnsEmpty(t *testing.T) {
	clearGitLabEnv(t)
	stubGlabAuthToken(t, "", errors.New("glab: not logged in"))

	assert.Empty(t, ResolveAuthToken(context.Background(), "", ""))
}

func TestResolveAuthToken_EnvironmentTokenWinsOverCLI(t *testing.T) {
	clearGitLabEnv(t)
	t.Setenv("GL_TOKEN", "env-token")
	stubGlabAuthToken(t, "cli-token", nil)

	assert.Equal(t, "env-token",
		ResolveAuthToken(context.Background(), EnvironmentToken()))
}

func TestDefaultServerURL(t *testing.T) {
	tests := []struct {
		name        string
		ciServerURL string
		gitlabHost  string
		glHost      string
		want        string
	}{
		{"Default", "", "", "", "https://gitlab.com"},
		{"CIServerURLWins", "https://ci.example.com", "https://other.example.com", "", "https://ci.example.com"},
		{"GitLabHost", "", "https://gitlab.example.com", "", "https://gitlab.example.com"},
		{"GLHost", "", "", "https://gl.example.com", "https://gl.example.com"},
		{"BareHostGetsScheme", "", "gitlab.example.com", "", "https://gitlab.example.com"},
		{"TrailingSlashTrimmed", "https://ci.example.com/", "", "", "https://ci.example.com"},
		{"GitLabHostBeatsGLHost", "", "https://a.example.com", "https://b.example.com", "https://a.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGitLabEnv(t)
			t.Setenv("CI_SERVER_URL", tt.ciServerURL)
			t.Setenv("GITLAB_HOST", tt.gitlabHost)
			t.Setenv("GL_HOST", tt.glHost)

			assert.Equal(t, tt.want, DefaultServerURL())
		})
	}
}

func TestGitLabAPIBaseURL(t *testing.T) {
	tests := []struct {
		name        string
		rawBase     string
		ciServerURL string
		want        string
	}{
		{"DefaultPublic", "", "", "https://gitlab.com/api/v4/"},
		{"FromCIServerURL", "", "https://gitlab.example.com", "https://gitlab.example.com/api/v4/"},
		{"ExplicitBeatsEnv", "https://explicit.example.com", "https://gitlab.example.com", "https://explicit.example.com/api/v4/"},
		{"ExplicitWithTrailingSlash", "https://gitlab.example.com/", "", "https://gitlab.example.com/api/v4/"},
		{"ExplicitAlreadyAPIURL", "https://gitlab.example.com/api/v4/", "", "https://gitlab.example.com/api/v4/"},
		{"BareHostname", "gitlab.example.com", "", "https://gitlab.example.com/api/v4/"},
		{"SubpathInstance", "https://example.com/gitlab", "", "https://example.com/gitlab/api/v4/"},
		{"HostWithPort", "http://localhost:8080", "", "http://localhost:8080/api/v4/"},
		{"LoopbackIPv4HTTP", "http://127.0.0.1:9999", "", "http://127.0.0.1:9999/api/v4/"},
		{"LoopbackIPv6HTTP", "http://[::1]:8080", "", "http://[::1]:8080/api/v4/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGitLabEnv(t)
			t.Setenv("CI_SERVER_URL", tt.ciServerURL)

			got, err := GitLabAPIBaseURL(tt.rawBase)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGitLabAPIBaseURL_Invalid(t *testing.T) {
	clearGitLabEnv(t)

	_, err := GitLabAPIBaseURL("https://%zz")
	require.Error(t, err)
}

// TestGitLabAPIBaseURL_RejectsUntrustedOrigins pins the origin validation. The
// resolved origin receives the token in a request header, so an origin that
// would expose it in transit (plaintext http) or that smuggles credentials of
// its own (userinfo) is rejected outright — whichever way it was resolved,
// flag or environment. Loopback stays exempt from the https requirement:
// traffic to it never crosses a network, and local test stubs listen on http.
func TestGitLabAPIBaseURL_RejectsUntrustedOrigins(t *testing.T) {
	tests := []struct {
		name        string
		rawBase     string
		ciServerURL string
		wantErr     string
	}{
		{
			name:    "PlaintextHTTP",
			rawBase: "http://gitlab.example.com",
			wantErr: "https",
		},
		{
			name:        "PlaintextHTTPFromEnv",
			ciServerURL: "http://gitlab.example.com",
			wantErr:     "https",
		},
		{
			name:    "UserinfoWithPassword",
			rawBase: "https://user:pass@gitlab.example.com",
			wantErr: "credentials",
		},
		{
			name:    "UserinfoBareUser",
			rawBase: "https://user@gitlab.example.com",
			wantErr: "credentials",
		},
		{
			name:    "UnsupportedScheme",
			rawBase: "ftp://gitlab.example.com",
			wantErr: "scheme",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGitLabEnv(t)
			t.Setenv("CI_SERVER_URL", tt.ciServerURL)

			_, err := GitLabAPIBaseURL(tt.rawBase)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestHostnameFromAPIBaseURL(t *testing.T) {
	tests := []struct {
		name       string
		apiBaseURL string
		gitlabHost string
		want       string
	}{
		{"Public", "https://gitlab.com/api/v4/", "", "gitlab.com"},
		{"SelfHosted", "https://gitlab.example.com/api/v4/", "", "gitlab.example.com"},
		{"WithPort", "http://localhost:8080/api/v4/", "", "localhost:8080"},
		{"EmptyFallsBackToDefault", "", "gitlab.example.com", "gitlab.example.com"},
		{"UnparseableFallsBackToDefault", "://nope", "", "gitlab.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGitLabEnv(t)
			t.Setenv("GITLAB_HOST", tt.gitlabHost)

			assert.Equal(t, tt.want, HostnameFromAPIBaseURL(tt.apiBaseURL))
		})
	}
}

func TestDefaultGitLabHost(t *testing.T) {
	clearGitLabEnv(t)
	assert.Equal(t, "gitlab.com", DefaultGitLabHost())

	t.Setenv("CI_SERVER_URL", "https://gitlab.example.com")
	assert.Equal(t, "gitlab.example.com", DefaultGitLabHost())
}

func TestParseProject(t *testing.T) {
	tests := []struct {
		name    string
		project string
		want    string
		wantErr bool
	}{
		{"Simple", "group/project", "group/project", false},
		{"Subgroup", "group/subgroup/project", "group/subgroup/project", false},
		{"TrimsSlashes", "/group/project/", "group/project", false},
		{"Empty", "", "", true},
		{"NoGroup", "project", "", true},
		{"EmptySegment", "group//project", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProject(tt.project)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewClient_RejectsInvalidBaseURL(t *testing.T) {
	_, err := NewClient("token", WithBaseURL("://bad"))
	require.Error(t, err)
}

// An explicit origin pin must also pin where the bytes go. HTTP(S)_PROXY are
// ordinary environment variables, so a GitLab pipeline starter can set them as
// pipeline variables and route the token through a host of their choosing
// without ever changing the pinned hostname.
func TestProxylessTransportDropsEnvironmentProxy(t *testing.T) {
	stock := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 7 * time.Second,
	}

	pinned, ok := proxylessTransport(stock).(*http.Transport)
	require.True(t, ok)
	assert.Nil(t, pinned.Proxy,
		"a pinned client must not consult HTTP(S)_PROXY")
	assert.Equal(t, 7*time.Second, pinned.TLSHandshakeTimeout,
		"unrelated transport settings must be preserved")
	assert.NotNil(t, stock.Proxy, "the input must not be mutated")
}

// The test-time forge API guard replaces http.DefaultTransport with a wrapper
// that is not an *http.Transport. Leaving it untouched keeps the guard active.
func TestProxylessTransportLeavesNonStockTransportAlone(t *testing.T) {
	wrapped := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, nil
	})
	assert.NotNil(t, proxylessTransport(wrapped))
}

// A pinned origin has to actually get the proxyless transport; an unpinned one
// must keep a nil Transport so http.DefaultTransport — and with it the
// test-time forge API guard — stays in play.
func TestNewHTTPClientPinnedOriginInstallsTransport(t *testing.T) {
	assert.NotNil(t, newHTTPClient(true).Transport,
		"a pinned client must carry its own transport")
	assert.Nil(t, newHTTPClient(false).Transport,
		"an unpinned client must keep using the default transport")
}

// WithPinnedOrigin must reach the client options; otherwise the pin silently
// resolves to an ordinary client.
func TestWithPinnedOriginSetsOption(t *testing.T) {
	var cfg clientOptions
	require.NoError(t, WithPinnedOrigin()(&cfg))
	assert.True(t, cfg.pinnedOrigin)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
