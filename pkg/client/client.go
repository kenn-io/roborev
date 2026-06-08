package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/roborev/pkg/client/generated"
)

// Client is a typed roborev daemon API client generated from the Huma
// OpenAPI contract, with hand-written raw helpers for endpoints whose bodies
// must not be JSON-decoded or buffered.
type Client struct {
	*generated.Client

	apiClient runtime.APIClient
	doer      contextDoer
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
		apiClient: apiClient,
		doer:      doer,
	}, nil
}

// GetJobLogRaw returns the raw /api/job/log HTTP response. The caller owns and
// must close the response body.
func (c *Client) GetJobLogRaw(
	ctx context.Context,
	options *generated.GetJobLogRequestOptions,
	reqEditors ...runtime.RequestEditorFn,
) (*http.Response, error) {
	query := url.Values{}
	if options != nil && options.Query != nil {
		setQueryString(query, "job_id", options.Query.JobID)
		setQueryString(query, "offset", options.Query.Offset)
	}
	return c.doRaw(ctx, http.MethodGet, "/api/job/log", query, reqEditors...)
}

// GetJobOutputRaw returns the raw /api/job/output HTTP response. Use it for
// stream=1 so NDJSON can be consumed incrementally. The caller owns and must
// close the response body.
func (c *Client) GetJobOutputRaw(
	ctx context.Context,
	options *generated.GetJobOutputRequestOptions,
	reqEditors ...runtime.RequestEditorFn,
) (*http.Response, error) {
	query := url.Values{}
	if options != nil && options.Query != nil {
		setQueryString(query, "job_id", options.Query.JobID)
		setQueryString(query, "stream", options.Query.Stream)
	}
	return c.doRaw(ctx, http.MethodGet, "/api/job/output", query, reqEditors...)
}

// GetJobPatchRaw returns the raw /api/job/patch HTTP response. The caller owns
// and must close the response body.
func (c *Client) GetJobPatchRaw(
	ctx context.Context,
	options *generated.GetJobPatchRequestOptions,
	reqEditors ...runtime.RequestEditorFn,
) (*http.Response, error) {
	query := url.Values{}
	if options != nil && options.Query != nil {
		setQueryString(query, "job_id", options.Query.JobID)
	}
	return c.doRaw(ctx, http.MethodGet, "/api/job/patch", query, reqEditors...)
}

// StreamEventsRaw returns the raw /api/stream/events HTTP response so daemon
// events can be consumed incrementally. The caller owns and must close the
// response body.
func (c *Client) StreamEventsRaw(
	ctx context.Context,
	options *generated.StreamEventsRequestOptions,
	reqEditors ...runtime.RequestEditorFn,
) (*http.Response, error) {
	query := url.Values{}
	if options != nil && options.Query != nil {
		setQueryString(query, "repo", options.Query.Repo)
	}
	return c.doRaw(ctx, http.MethodGet, "/api/stream/events", query, reqEditors...)
}

// SyncNowRaw returns the raw /api/sync/now HTTP response. Use it for stream=1
// so sync progress can be consumed incrementally. The caller owns and must
// close the response body.
func (c *Client) SyncNowRaw(
	ctx context.Context,
	options *generated.SyncNowRequestOptions,
	reqEditors ...runtime.RequestEditorFn,
) (*http.Response, error) {
	query := url.Values{}
	if options != nil && options.Query != nil {
		setQueryString(query, "stream", options.Query.Stream)
	}
	return c.doRaw(ctx, http.MethodPost, "/api/sync/now", query, reqEditors...)
}

func (c *Client) doRaw(
	ctx context.Context,
	method string,
	path string,
	query url.Values,
	reqEditors ...runtime.RequestEditorFn,
) (*http.Response, error) {
	requestURL := c.apiClient.GetBaseURL() + path
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	req, err := c.apiClient.CreateRequest(ctx, runtime.RequestOptionsParameters{
		RequestURL: requestURL,
		Method:     method,
	}, reqEditors...)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}
	resp, err := c.doer.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("error executing request: %w", err)
	}
	return resp, nil
}

func setQueryString(query url.Values, key string, value *string) {
	if value != nil {
		query.Set(key, *value)
	}
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
