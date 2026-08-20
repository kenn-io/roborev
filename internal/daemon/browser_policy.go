package daemon

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type BrowserPolicy struct {
	authentication  string
	publicHost      string
	acceptedHosts   map[string]struct{}
	acceptedOrigins map[string]struct{}
	loopbackHosts   map[string]struct{}
	loopbackOrigins map[string]struct{}
}

func NewBrowserPolicy(endpoint BrowserEndpoint, developmentOrigin string) (BrowserPolicy, error) {
	policy := BrowserPolicy{
		authentication:  endpoint.authentication,
		acceptedHosts:   make(map[string]struct{}),
		acceptedOrigins: make(map[string]struct{}),
		loopbackHosts:   make(map[string]struct{}),
		loopbackOrigins: make(map[string]struct{}),
	}
	listenerHost, err := normalizeAuthority(endpoint.Address)
	if err != nil {
		return BrowserPolicy{}, fmt.Errorf("browser listener authority: %w", err)
	}
	policy.acceptedHosts[listenerHost] = struct{}{}
	policy.loopbackHosts[listenerHost] = struct{}{}

	if err := policy.addOrigin(endpoint.Origin, false); err != nil {
		return BrowserPolicy{}, err
	}
	parsedOrigin, err := url.Parse(endpoint.Origin)
	if err != nil {
		return BrowserPolicy{}, fmt.Errorf("invalid browser origin")
	}
	policy.publicHost, err = normalizeAuthority(parsedOrigin.Host)
	if err != nil {
		return BrowserPolicy{}, err
	}
	if developmentOrigin != "" {
		parsed, err := url.Parse(developmentOrigin)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || !isLoopbackAuthority(parsed.Host) {
			return BrowserPolicy{}, fmt.Errorf("development origin must be an exact loopback HTTP or HTTPS origin")
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return BrowserPolicy{}, fmt.Errorf("development origin must be an exact loopback HTTP or HTTPS origin")
		}
		if err := policy.addOrigin(developmentOrigin, true); err != nil {
			return BrowserPolicy{}, err
		}
	}
	return policy, nil
}

func (p *BrowserPolicy) addOrigin(raw string, forceLoopback bool) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid browser origin")
	}
	origin := strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
	host, err := normalizeAuthority(parsed.Host)
	if err != nil {
		return err
	}
	p.acceptedOrigins[origin] = struct{}{}
	p.acceptedHosts[host] = struct{}{}
	if forceLoopback || isLoopbackAuthority(parsed.Host) {
		p.loopbackOrigins[origin] = struct{}{}
		p.loopbackHosts[host] = struct{}{}
	}
	return nil
}

func (p BrowserPolicy) ValidateHost(request *http.Request) error {
	host, err := normalizeAuthority(request.Host)
	if err != nil {
		return err
	}
	if _, found := p.acceptedHosts[host]; !found {
		return fmt.Errorf("host is not accepted")
	}
	return nil
}

func (p BrowserPolicy) ValidateOrigin(request *http.Request) error {
	origin, err := normalizeOriginHeader(request.Header.Get("Origin"))
	if err != nil {
		return err
	}
	if _, found := p.acceptedOrigins[origin]; !found {
		return fmt.Errorf("origin is not accepted")
	}
	return nil
}

func (p BrowserPolicy) AllowsLocalSession(request *http.Request) bool {
	if p.authentication != "local" || hasForwardingHeader(request.Header) {
		return false
	}
	host, err := normalizeAuthority(request.Host)
	if err != nil {
		return false
	}
	if _, found := p.loopbackHosts[host]; !found {
		return false
	}
	if rawOrigin := request.Header.Get("Origin"); rawOrigin != "" {
		origin, err := normalizeOriginHeader(rawOrigin)
		if err != nil {
			return false
		}
		if _, found := p.loopbackOrigins[origin]; !found {
			return false
		}
	}
	peer, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(peer, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (p BrowserPolicy) AllowsProxySession(request *http.Request) bool {
	if p.authentication != "proxy" || !hasForwardingHeader(request.Header) {
		return false
	}
	host, err := normalizeAuthority(request.Host)
	if err != nil || host != p.publicHost {
		return false
	}
	return p.ValidateOrigin(request) == nil
}

func normalizeAuthority(raw string) (string, error) {
	if raw == "" || strings.Contains(raw, "@") {
		return "", fmt.Errorf("invalid authority")
	}
	parsed, err := url.Parse("//" + raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" {
		return "", fmt.Errorf("invalid authority")
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" {
		return "", fmt.Errorf("invalid authority")
	}
	port := parsed.Port()
	if port != "" {
		if _, err := net.LookupPort("tcp", port); err != nil {
			return "", fmt.Errorf("invalid authority")
		}
		return strings.ToLower(net.JoinHostPort(hostname, port)), nil
	}
	return hostname, nil
}

func normalizeOriginHeader(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid origin")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid origin")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func isLoopbackAuthority(authority string) bool {
	parsed, err := url.Parse("//" + authority)
	if err != nil {
		return false
	}
	host := strings.Trim(strings.ToLower(parsed.Hostname()), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func hasForwardingHeader(header http.Header) bool {
	for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP"} {
		if header.Get(name) != "" {
			return true
		}
	}
	return false
}
