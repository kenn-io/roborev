package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/roborev/pkg/client/generated"
)

// Client is a typed roborev daemon API client generated from the Huma
// OpenAPI contract, including raw calls for streaming response bodies.
type Client struct {
	*generated.Client
	*generated.RawClient
}

// New creates a client using http.DefaultClient.
func New(baseURL string) (*Client, error) {
	return NewWithHTTPClient(baseURL, http.DefaultClient)
}

// NewWithHTTPClient creates a client using the supplied HTTP client.
func NewWithHTTPClient(baseURL string, httpClient *http.Client) (*Client, error) {
	doer := contextDoer{client: httpClient}
	apiClient, err := runtime.NewAPIClient(
		strings.TrimRight(baseURL, "/"),
		runtime.WithHTTPClient(doer),
	)
	if err != nil {
		return nil, fmt.Errorf("error creating API client: %w", err)
	}
	return &Client{
		Client:    generated.NewClient(apiClient),
		RawClient: generated.NewRawClient(apiClient, doer),
	}, nil
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

// WithBody supplies an already encoded request body to a generated raw call.
func WithBody(body []byte) runtime.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		req.Header.Set("Content-Type", "application/json")
		return nil
	}
}

// WithQuery applies query values to a generated call.
func WithQuery(query url.Values) runtime.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		req.URL.RawQuery = query.Encode()
		return nil
	}
}
