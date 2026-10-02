package client

import (
	"net/http"

	"go.kenn.io/roborev/internal/auth"
)

// NewWithAuthKey creates a client for a daemon configured with auth_key.
// The key is scoped to baseURL's origin and redirects are not followed.
func NewWithAuthKey(baseURL, key string) (*Client, error) {
	if err := auth.ValidateKey(key); err != nil {
		return nil, err
	}
	return NewWithHTTPClient(baseURL, auth.HTTPClient(baseURL, http.DefaultClient, func() (string, error) { return key, nil }))
}
