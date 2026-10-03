package client

import (
	"net/http"

	"go.kenn.io/roborev/internal/auth"
)

// NewWithAuthKey creates a client for a daemon configured with auth_key.
// The URL must use HTTPS with a certificate trusted by the default HTTP client.
func NewWithAuthKey(baseURL, key string) (*Client, error) {
	return NewWithAuthKeyAndHTTPClient(baseURL, key, http.DefaultClient)
}

// NewWithAuthKeyAndHTTPClient creates an authenticated client using the
// caller's HTTP transport, including any TLS roots needed to verify a local
// daemon certificate. The key is scoped to baseURL's origin and redirects are
// not followed.
func NewWithAuthKeyAndHTTPClient(baseURL, key string, httpClient *http.Client) (*Client, error) {
	if err := auth.ValidateKey(key); err != nil {
		return nil, err
	}
	return NewWithHTTPClient(baseURL, auth.HTTPClient(baseURL, httpClient, func() (string, error) { return key, nil }))
}
