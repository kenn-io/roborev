package client

import (
	"context"
	"net/http"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/roborev/pkg/client/generated"
)

// Client is a typed roborev daemon API client generated from the Huma
// OpenAPI contract.
type Client = generated.Client

// New creates a client using http.DefaultClient.
func New(baseURL string) (*Client, error) {
	return NewWithHTTPClient(baseURL, http.DefaultClient)
}

// NewWithHTTPClient creates a client using the supplied HTTP client.
func NewWithHTTPClient(baseURL string, httpClient *http.Client) (*Client, error) {
	return generated.NewDefaultClient(
		strings.TrimRight(baseURL, "/"),
		runtime.WithHTTPClient(contextDoer{client: httpClient}),
	)
}

type contextDoer struct {
	client *http.Client
}

func (d contextDoer) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	client := d.client
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req.WithContext(ctx))
}
