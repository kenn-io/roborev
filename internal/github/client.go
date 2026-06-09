package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	googlegithub "github.com/google/go-github/v88/github"
)

type ClientOption func(*clientOptions) error

type clientOptions struct {
	baseURL    *url.URL
	httpClient *http.Client
}

type Client struct {
	api *googlegithub.Client
}

const defaultHTTPTimeout = 30 * time.Second

var ghAuthTokenFn = func(ctx context.Context, hostname string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	args := []string{"auth", "token"}
	if hostname != "" && !strings.EqualFold(hostname, "github.com") {
		args = append(args, "--hostname", hostname)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func ptr[T any](value T) *T {
	p := new(T)
	*p = value
	return p
}

func WithBaseURL(raw string) ClientOption {
	return func(opts *clientOptions) error {
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		parsed, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("parse base URL: %w", err)
		}
		if !strings.HasSuffix(parsed.Path, "/") {
			parsed.Path += "/"
		}
		opts.baseURL = parsed
		return nil
	}
}

func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(opts *clientOptions) error {
		opts.httpClient = httpClient
		return nil
	}
}

func EnvironmentToken() string {
	token := strings.TrimSpace(os.Getenv("GH_TOKEN"))
	if token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
}

// ResolveAuthToken returns the first non-empty token from: the
// provided token, GH_TOKEN/GITHUB_TOKEN env vars (via EnvironmentToken),
// or `gh auth token`. When hostname is provided and is not "github.com",
// the gh CLI fallback uses `--hostname` to request the correct token
// for GitHub Enterprise instances.
func ResolveAuthToken(ctx context.Context, token string, hostname ...string) string {
	token = strings.TrimSpace(token)
	if token != "" {
		return token
	}

	host := ""
	if len(hostname) > 0 {
		host = hostname[0]
	}
	token, err := ghAuthTokenFn(ctx, host)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(token)
}

// DefaultGitHubHost returns the GitHub hostname from GH_HOST, or
// "github.com" when unset. Callers can pass the result to
// ResolveAuthToken so the gh CLI fallback targets the correct host.
func DefaultGitHubHost() string {
	return defaultGitHubHost()
}

// HostnameFromAPIBaseURL extracts the hostname from a resolved API
// base URL (e.g., "https://api.github.com/" → "github.com",
// "https://ghe.example.com/api/v3/" → "ghe.example.com"). Falls
// back to DefaultGitHubHost when the URL is empty or unparseable.
func HostnameFromAPIBaseURL(apiBaseURL string) string {
	apiBaseURL = strings.TrimSpace(apiBaseURL)
	if apiBaseURL == "" {
		return defaultGitHubHost()
	}
	parsed, err := url.Parse(apiBaseURL)
	if err != nil || parsed.Host == "" {
		return defaultGitHubHost()
	}
	if strings.EqualFold(parsed.Hostname(), "api.github.com") {
		return "github.com"
	}
	return parsed.Host
}

func NewClient(token string, opts ...ClientOption) (*Client, error) {
	cfg := clientOptions{}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}

	httpClient := cfg.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}

	apiOpts := []googlegithub.ClientOptionsFunc{
		googlegithub.WithHTTPClient(httpClient),
	}
	if cfg.baseURL != nil {
		base := cfg.baseURL.String()
		apiOpts = append(apiOpts, googlegithub.WithEnterpriseURLs(base, base))
	}
	if strings.TrimSpace(token) != "" {
		apiOpts = append(apiOpts, googlegithub.WithAuthToken(strings.TrimSpace(token)))
	}

	api, err := googlegithub.NewClient(apiOpts...)
	if err != nil {
		return nil, err
	}
	return &Client{api: api}, nil
}

func GitHubAPIBaseURL(rawBase string) (string, error) {
	rawBase = strings.TrimSpace(rawBase)
	if rawBase == "" {
		host := defaultGitHubHost()
		if strings.EqualFold(host, "github.com") {
			return "https://api.github.com/", nil
		}
		return "https://" + host + "/api/v3/", nil
	}

	parsed, err := url.Parse(rawBase)
	if err != nil {
		return "", fmt.Errorf("parse GitHub API base URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid GitHub API base URL %q", rawBase)
	}
	if !strings.HasSuffix(parsed.Path, "/") {
		parsed.Path += "/"
	}
	return parsed.String(), nil
}

func GitHubWebBaseURL(rawBase string) (string, error) {
	apiBase, err := GitHubAPIBaseURL(rawBase)
	if err != nil {
		return "", err
	}

	parsed, err := url.Parse(apiBase)
	if err != nil {
		return "", fmt.Errorf("parse GitHub API base URL: %w", err)
	}
	if strings.EqualFold(parsed.Hostname(), "api.github.com") {
		parsed.Host = "github.com"
	}
	parsed.Path = "/"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func defaultGitHubHost() string {
	host := strings.TrimSpace(os.Getenv("GH_HOST"))
	if host == "" {
		return "github.com"
	}
	if strings.Contains(host, "://") {
		if parsed, err := url.Parse(host); err == nil && parsed.Host != "" {
			return parsed.Host
		}
	}
	return strings.TrimSuffix(host, "/")
}

func parseRepo(ghRepo string) (string, string, error) {
	owner, repo, ok := strings.Cut(strings.TrimSpace(ghRepo), "/")
	if !ok || owner == "" || repo == "" {
		return "", "", fmt.Errorf("invalid GitHub repo %q", ghRepo)
	}
	return owner, repo, nil
}
