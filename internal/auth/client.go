// Package auth contains shared HTTP Bearer authentication helpers.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
)

// ErrUnexpectedOrigin reports a request outside the configured daemon origin.
var ErrUnexpectedOrigin = errors.New("daemon client request must use its configured origin")

// ErrUnverifiedServer reports that a daemon did not prove possession of the
// configured shared key before the client sent it.
var ErrUnverifiedServer = errors.New("daemon access denied")

const serverProofDomain = "roborev-daemon-auth-v1:server:"

// HTTPClient clones client, verifies server possession of a request-time key,
// then attaches that key only to baseURL's origin. Redirects are returned to
// callers without forwarding API bodies or credentials. The caller's client
// and requests are never mutated.
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
	if err := t.verifyServer(r, key); err != nil {
		return nil, err
	}
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+key)
	return t.base.RoundTrip(clone)
}

func (t *transport) verifyServer(r *http.Request, key string) error {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return ErrUnverifiedServer
	}
	challengeURL := *t.origin
	challengeURL.Path = "/api/ping"
	challengeURL.RawPath = ""
	challengeURL.RawQuery = ""
	challengeURL.Fragment = ""
	challenge, err := http.NewRequestWithContext(r.Context(), http.MethodGet, challengeURL.String(), nil)
	if err != nil {
		return ErrUnverifiedServer
	}
	challenge.Header.Set("X-Roborev-Auth-Nonce", hex.EncodeToString(nonce))
	response, err := t.base.RoundTrip(challenge)
	if err != nil {
		return err
	}
	if response.Body != nil {
		defer response.Body.Close()
	}
	if response.StatusCode != http.StatusUnauthorized {
		return ErrUnverifiedServer
	}
	proofs := response.Header.Values("X-Roborev-Auth-Proof")
	if len(proofs) != 1 {
		return ErrUnverifiedServer
	}
	proof, err := hex.DecodeString(proofs[0])
	if err != nil || !hmac.Equal(proof, ServerProof(key, nonce)) {
		return ErrUnverifiedServer
	}
	return nil
}

// ServerProof returns the keyed proof for a fresh client challenge nonce.
func ServerProof(key string, nonce []byte) []byte {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(serverProofDomain))
	_, _ = mac.Write(nonce)
	return mac.Sum(nil)
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
