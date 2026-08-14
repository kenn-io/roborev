// Package gitlab wraps the official GitLab API client with the token,
// base-URL, and merge-request comment helpers roborev needs. It mirrors the
// shape of the internal/github package so both forges behave the same way.
package gitlab

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	gogitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"go.kenn.io/roborev/internal/procutil"
)

// DefaultHost is the public GitLab hostname used when nothing else is
// configured.
const DefaultHost = "gitlab.com"

// defaultServerURL is the public GitLab instance URL.
const defaultServerURL = "https://" + DefaultHost

// apiVersionPath is the REST API prefix appended to the server URL.
const apiVersionPath = "/api/v4"

const defaultHTTPTimeout = 30 * time.Second

type ClientOption func(*clientOptions) error

type clientOptions struct {
	baseURL        string
	disableRetries bool
	pinnedOrigin   bool
}

type Client struct {
	api *gogitlab.Client
	// disableRetries mirrors WithoutRetries so the per-request retry policy in
	// comment.go can honor it; the library checks it only in its own policy,
	// which a per-request override replaces.
	disableRetries bool
}

// glabAuthTokenFn shells out to the glab CLI for a token. It is a variable so
// tests can stub the fallback instead of running the real CLI.
var glabAuthTokenFn = func(ctx context.Context, hostname string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	args := []string{"auth", "token"}
	if hostname != "" && !strings.EqualFold(hostname, DefaultHost) {
		args = append(args, "--hostname", hostname)
	}
	out, err := buildGlabAuthCmd(ctx, args).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func buildGlabAuthCmd(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "glab", args...)
	procutil.HideConsole(cmd)
	return cmd
}

func ptr[T any](value T) *T {
	p := new(T)
	*p = value
	return p
}

// WithBaseURL points the client at a specific API base URL. The GitLab client
// appends the "api/v4/" suffix itself when it is missing, so both a server URL
// and a full API URL are accepted.
func WithBaseURL(raw string) ClientOption {
	return func(opts *clientOptions) error {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil
		}
		if _, err := url.Parse(raw); err != nil {
			return fmt.Errorf("parse base URL: %w", err)
		}
		opts.baseURL = raw
		return nil
	}
}

// WithPinnedOrigin marks the base URL as a security pin rather than a mere
// default. The client then stops consulting HTTP_PROXY/HTTPS_PROXY, which are
// ordinary environment variables a GitLab pipeline starter can set as pipeline
// variables: a proxy is a redirect of where the token's bytes go, so honoring
// one would defeat the pin without ever changing the pinned hostname.
func WithPinnedOrigin() ClientOption {
	return func(opts *clientOptions) error {
		opts.pinnedOrigin = true
		return nil
	}
}

// WithoutRetries disables the client's built-in retry loop. Useful in tests
// where a deliberate server error should fail fast.
func WithoutRetries() ClientOption {
	return func(opts *clientOptions) error {
		opts.disableRetries = true
		return nil
	}
}

// EnvironmentToken returns the GitLab token from GITLAB_TOKEN, falling back to
// GL_TOKEN (the glab CLI's alternate variable).
func EnvironmentToken() string {
	token := strings.TrimSpace(os.Getenv("GITLAB_TOKEN"))
	if token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("GL_TOKEN"))
}

// ResolveAuthToken returns the first non-empty token from: the provided token,
// or `glab auth token`. When hostname is provided and is not "gitlab.com", the
// glab CLI fallback uses --hostname to request the correct token for
// self-hosted instances. Callers pass EnvironmentToken() as the token argument
// to keep the env vars ahead of the CLI fallback.
func ResolveAuthToken(ctx context.Context, token string, hostname ...string) string {
	token = strings.TrimSpace(token)
	if token != "" {
		return token
	}

	host := ""
	if len(hostname) > 0 {
		host = hostname[0]
	}
	token, err := glabAuthTokenFn(ctx, host)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(token)
}

// DefaultServerURL returns the GitLab server URL from CI_SERVER_URL (set
// inside GitLab CI), then GITLAB_HOST, then GL_HOST, falling back to
// https://gitlab.com.
func DefaultServerURL() string {
	for _, key := range []string{"CI_SERVER_URL", "GITLAB_HOST", "GL_HOST"} {
		if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
			return normalizeServerURL(raw)
		}
	}
	return defaultServerURL
}

// DefaultGitLabHost returns the hostname of the resolved default server URL.
// Callers can pass the result to ResolveAuthToken so the glab CLI fallback
// targets the correct host.
func DefaultGitLabHost() string {
	parsed, err := url.Parse(DefaultServerURL())
	if err != nil || parsed.Host == "" {
		return DefaultHost
	}
	return parsed.Host
}

// HostnameFromAPIBaseURL extracts the hostname from a resolved API base URL
// (e.g. "https://gitlab.com/api/v4/" -> "gitlab.com",
// "https://gitlab.example.com/api/v4/" -> "gitlab.example.com"). Falls back to
// DefaultGitLabHost when the URL is empty or unparseable.
func HostnameFromAPIBaseURL(apiBaseURL string) string {
	apiBaseURL = strings.TrimSpace(apiBaseURL)
	if apiBaseURL == "" {
		return DefaultGitLabHost()
	}
	parsed, err := url.Parse(apiBaseURL)
	if err != nil || parsed.Host == "" {
		return DefaultGitLabHost()
	}
	return parsed.Host
}

// GitLabAPIBaseURL resolves the REST API base URL. An empty rawBase falls back
// to DefaultServerURL (CI_SERVER_URL / GITLAB_HOST / GL_HOST / gitlab.com).
// The "/api/v4/" suffix is appended when missing.
func GitLabAPIBaseURL(rawBase string) (string, error) {
	rawBase = strings.TrimSpace(rawBase)
	if rawBase == "" {
		rawBase = DefaultServerURL()
	} else {
		rawBase = normalizeServerURL(rawBase)
	}

	parsed, err := url.Parse(rawBase)
	if err != nil {
		return "", fmt.Errorf("parse GitLab API base URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid GitLab API base URL %q", rawBase)
	}
	// The resolved origin receives the token in a request header, so refuse
	// origins that would expose it. This runs on every resolution path — flag,
	// CI_SERVER_URL, GITLAB_HOST/GL_HOST — which matters because CI_SERVER_URL
	// is a predefined variable a pipeline starter can override.
	if parsed.User != nil {
		return "", fmt.Errorf(
			"GitLab API base URL %q must not embed credentials", rawBase)
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(parsed.Hostname()) {
			return "", fmt.Errorf(
				"GitLab API base URL %q uses plaintext http, which would "+
					"expose the token in transit — use https "+
					"(http is allowed for loopback addresses only)", rawBase)
		}
	default:
		return "", fmt.Errorf(
			"invalid GitLab API base URL %q (unsupported scheme %q)",
			rawBase, parsed.Scheme)
	}

	path := strings.TrimSuffix(parsed.Path, "/")
	if !strings.HasSuffix(path, apiVersionPath) {
		path += apiVersionPath
	}
	parsed.Path = path + "/"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// newHTTPClient builds the HTTP client the API client runs on.
//
// A nil Transport means http.DefaultTransport, unlike the pooled client the
// GitLab library installs by default; that is what keeps the test-time forge
// API guard effective. A pinned origin is the one case that needs its own
// transport, to stop the proxy environment from redirecting the token.
func newHTTPClient(pinnedOrigin bool) *http.Client {
	client := &http.Client{Timeout: defaultHTTPTimeout}
	if pinnedOrigin {
		client.Transport = proxylessTransport(http.DefaultTransport)
	}
	return client
}

// proxylessTransport returns base with environment proxy lookup disabled.
//
// Only the stock *http.Transport is rewritten. Anything else is returned
// untouched: the test-time forge API guard swaps http.DefaultTransport for a
// wrapper, and replacing it here would disable that guard. A test binary is
// not where a pipeline variable is the threat, so leaving it alone is safe.
func proxylessTransport(base http.RoundTripper) http.RoundTripper {
	transport, ok := base.(*http.Transport)
	if !ok {
		return base
	}
	pinned := transport.Clone()
	pinned.Proxy = nil
	return pinned
}

// isLoopbackHost reports whether host names the local machine. Loopback is
// exempt from the https requirement in GitLabAPIBaseURL: traffic to it never
// crosses a network, and local test stubs listen on plain http.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// normalizeServerURL accepts either a bare hostname ("gitlab.example.com") or
// a full URL and returns a scheme-qualified URL without a trailing slash.
func normalizeServerURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultServerURL
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	return strings.TrimSuffix(raw, "/")
}

func NewClient(token string, opts ...ClientOption) (*Client, error) {
	cfg := clientOptions{}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}

	httpClient := newHTTPClient(cfg.pinnedOrigin)

	apiOpts := []gogitlab.ClientOptionFunc{
		gogitlab.WithHTTPClient(httpClient),
	}
	if cfg.baseURL != "" {
		apiOpts = append(apiOpts, gogitlab.WithBaseURL(cfg.baseURL))
	}
	if cfg.disableRetries {
		apiOpts = append(apiOpts, gogitlab.WithoutRetries())
	}

	api, err := gogitlab.NewClient(strings.TrimSpace(token), apiOpts...)
	if err != nil {
		return nil, err
	}
	return &Client{api: api, disableRetries: cfg.disableRetries}, nil
}

// parseProject validates a GitLab project path. GitLab projects live under one
// or more groups, so "group/project" and "group/subgroup/project" are both
// valid. The client library URL-encodes the path when building requests.
func parseProject(project string) (string, error) {
	project = strings.Trim(strings.TrimSpace(project), "/")
	if project == "" {
		return "", fmt.Errorf("invalid GitLab project %q", project)
	}
	segments := strings.Split(project, "/")
	if len(segments) < 2 {
		return "", fmt.Errorf(
			"invalid GitLab project %q (want group/project)", project)
	}
	for _, segment := range segments {
		if strings.TrimSpace(segment) == "" {
			return "", fmt.Errorf(
				"invalid GitLab project %q (empty path segment)", project)
		}
	}
	return project, nil
}
