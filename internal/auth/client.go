// Package auth contains shared HTTP Bearer authentication helpers.
package auth

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
)

// ErrUnexpectedOrigin reports a request outside the configured daemon origin.
var ErrUnexpectedOrigin = errors.New("daemon client request must use its configured origin")

// HTTPClient clones client and attaches a request-time key only to baseURL's
// origin. Redirects are returned to callers without forwarding API bodies or
// credentials. The caller's client and requests are never mutated.
func HTTPClient(baseURL string, client *http.Client, key func() (string, error)) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	base := clone.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	origin, parseErr := url.Parse(baseURL)
	clone.Transport = &transport{base: base, origin: origin, parseErr: parseErr, key: key}
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone
}

type transport struct {
	base     http.RoundTripper
	origin   *url.URL
	parseErr error
	key      func() (string, error)
}

func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.parseErr != nil || t.origin == nil || r.URL.Scheme != t.origin.Scheme || r.URL.Host != t.origin.Host || r.Host != "" && r.Host != t.origin.Host || r.URL.User != nil {
		return nil, ErrUnexpectedOrigin
	}
	key, err := t.key()
	if err != nil {
		return nil, err
	}
	if key == "" {
		return t.base.RoundTrip(r)
	}
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+key)
	return t.base.RoundTrip(clone)
}

// ValidateKey checks Bearer token syntax without exposing the key in errors.
func ValidateKey(key string) error {
	padding := false
	for i := range len(key) {
		c := key[i]
		if c == '=' && i > 0 {
			padding = true
			continue
		}
		valid := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		valid = valid || c == '-' || c == '.' || c == '_' || c == '~' || c == '+' || c == '/'
		if padding || !valid {
			return errors.New("auth_key must be a valid HTTP Bearer token")
		}
	}
	return nil
}

// EqualKey compares bearer credentials in constant time when they have equal
// length.
func EqualKey(expected, provided string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}
