// Package auth contains shared HTTP Bearer authentication helpers.
package auth

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// ErrUnexpectedOrigin reports a request outside the configured daemon origin.
var ErrUnexpectedOrigin = errors.New("daemon client request must use its configured origin")

// ErrUnverifiedServer reports that a daemon connection does not use a
// transport that authenticates the server before the client sends its key.
var ErrUnverifiedServer = errors.New("daemon access denied")

// ErrPlaintextTransport reports that a request carrying a shared key was
// refused because the endpoint does not authenticate the server over TLS.
var ErrPlaintextTransport = errors.New("refusing to send daemon key over plaintext transport")

// HTTPClient clones client and attaches a request-time key only to baseURL's
// HTTPS origin. It refuses to send keys over plaintext HTTP. Redirects are
// returned to callers without forwarding API bodies or credentials. The
// caller's client and requests are never mutated.
func HTTPClient(baseURL string, client *http.Client, key func() (string, error)) *http.Client {
	return httpClient(baseURL, client, key, false)
}

// HTTPClientWithLocalTransport is for a transport whose endpoint is protected
// by operating-system access controls, such as a private Unix domain socket.
// Callers must not use it with a plaintext TCP transport.
func HTTPClientWithLocalTransport(baseURL string, client *http.Client, key func() (string, error)) *http.Client {
	return httpClient(baseURL, client, key, true)
}

func httpClient(baseURL string, client *http.Client, key func() (string, error), localTransport bool) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	base := clone.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	origin, parseErr := url.Parse(baseURL)
	clone.Transport = &transport{
		base: base, origin: origin, parseErr: parseErr, key: key, localTransport: localTransport,
	}
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone
}

type transport struct {
	base           http.RoundTripper
	origin         *url.URL
	parseErr       error
	key            func() (string, error)
	localTransport bool
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
		return t.roundTrip(r)
	}
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	if r.URL.Scheme != "https" && !t.localTransport {
		return nil, ErrPlaintextTransport
	}
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+key)
	return t.roundTrip(clone)
}

func (t *transport) roundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil && r.URL.Scheme == "https" {
		if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
			return nil, fmt.Errorf("%w: daemon TLS certificate verification failed", ErrUnverifiedServer)
		}
	}
	return resp, err
}

// ValidateKey requires a canonical lowercase hex encoding of a 32-byte shared key.
// Its 256-bit size follows the entropy recommendation for PKCE verifiers in RFC 7636
// Section 7.1.
func ValidateKey(key string) error {
	if key == "" {
		return nil
	}
	decoded, err := hex.DecodeString(key)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != key {
		return errors.New("auth_key must contain 64 lowercase hex characters; generate a key with openssl rand -hex 32")
	}
	return nil
}

// EqualKey compares bearer credentials in constant time when they have equal
// length.
func EqualKey(expected, provided string) bool {
	return subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}
