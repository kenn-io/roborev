package client

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/pkg/client/generated"
)

func TestNewWithHTTPClientNormalizesBaseURL(t *testing.T) {
	assert := assert.New(t)

	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	api, err := NewWithHTTPClient(server.URL+"/", server.Client())
	require.NoError(t, err)

	resp, err := api.PingWithResponse(t.Context())
	require.NoError(t, err)

	assert.Equal(http.StatusOK, resp.StatusCode)
	assert.Equal("/api/ping", gotPath)
}

func TestGetJobPatchRawReturnsPlainTextBody(t *testing.T) {
	assert := assert.New(t)
	const patch = "diff --git a/file.go b/file.go\n+added\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/job/patch", r.URL.Path)
		assert.Equal("123", r.URL.Query().Get("job_id"))
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(patch))
	}))
	defer server.Close()

	api, err := NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)

	jobID := "123"
	resp, err := api.GetJobPatchRaw(t.Context(), &generated.GetJobPatchRequestOptions{
		Query: &generated.GetJobPatchQuery{JobID: &jobID},
	})
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(http.StatusOK, resp.StatusCode)
	assert.Equal(patch, string(body))
}

func TestStreamEventsRawReturnsBeforeStreamCloses(t *testing.T) {
	assert := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/stream/events", r.URL.Path)
		assert.Equal("/repo", r.URL.Query().Get("repo"))
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte("{\"type\":\"ready\"}\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	api, err := NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	repo := "/repo"
	resp, err := api.StreamEventsRaw(ctx, &generated.StreamEventsRequestOptions{
		Query: &generated.StreamEventsQuery{Repo: &repo},
	})
	require.NoError(t, err)
	defer resp.Body.Close()

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	require.NoError(t, err)
	assert.Equal(http.StatusOK, resp.StatusCode)
	assert.JSONEq(`{"type":"ready"}`, line)
}

func TestRawHelpersRouteRequests(t *testing.T) {
	jobID := "123"
	offset := "456"
	stream := "1"

	tests := []struct {
		name       string
		wantPath   string
		wantQuery  map[string]string
		wantMethod string
		call       func(context.Context, *Client) (*http.Response, error)
	}{
		{
			name:       "job log",
			wantPath:   "/api/job/log",
			wantMethod: http.MethodGet,
			wantQuery: map[string]string{
				"job_id": jobID,
				"offset": offset,
			},
			call: func(ctx context.Context, api *Client) (*http.Response, error) {
				return api.GetJobLogRaw(ctx, &generated.GetJobLogRequestOptions{
					Query: &generated.GetJobLogQuery{JobID: &jobID, Offset: &offset},
				})
			},
		},
		{
			name:       "job output stream",
			wantPath:   "/api/job/output",
			wantMethod: http.MethodGet,
			wantQuery: map[string]string{
				"job_id": jobID,
				"stream": stream,
			},
			call: func(ctx context.Context, api *Client) (*http.Response, error) {
				return api.GetJobOutputRaw(ctx, &generated.GetJobOutputRequestOptions{
					Query: &generated.GetJobOutputQuery{JobID: &jobID, Stream: &stream},
				})
			},
		},
		{
			name:       "sync stream",
			wantPath:   "/api/sync/now",
			wantMethod: http.MethodPost,
			wantQuery: map[string]string{
				"stream": stream,
			},
			call: func(ctx context.Context, api *Client) (*http.Response, error) {
				return api.SyncNowRaw(ctx, &generated.SyncNowRequestOptions{
					Query: &generated.SyncNowQuery{Stream: &stream},
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(tt.wantPath, r.URL.Path)
				assert.Equal(tt.wantMethod, r.Method)
				for key, value := range tt.wantQuery {
					assert.Equal(value, r.URL.Query().Get(key))
				}
				_, _ = w.Write([]byte("ok"))
			}))
			defer server.Close()

			api, err := NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)

			resp, err := tt.call(t.Context(), api)
			require.NoError(t, err)
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal("ok", string(body))
		})
	}
}
