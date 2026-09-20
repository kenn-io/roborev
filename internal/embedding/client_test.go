package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddingInputTypeModes(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		kind        InputKind
		wantType    string
		wantPresent bool
	}{
		{name: "none omits input type", mode: "none", kind: InputDocument},
		{name: "retrieval document", mode: "retrieval", kind: InputDocument, wantType: "document", wantPresent: true},
		{name: "retrieval query", mode: "retrieval", kind: InputQuery, wantType: "query", wantPresent: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var request map[string]any
			var decodeErr error
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				decodeErr = json.NewDecoder(r.Body).Decode(&request)
				_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[3,4]}]}`))
			}))
			defer server.Close()

			client, err := New(Config{BaseURL: server.URL, Model: "embed-large", Dims: 2, InputTypeMode: tt.mode})
			require.NoError(t, err)
			_, err = client.Embed(context.Background(), tt.kind, []string{"review text"})
			require.NoError(t, err)
			require.NoError(t, decodeErr)

			got, present := request["input_type"]
			assert.Equal(t, tt.wantPresent, present)
			if tt.wantPresent {
				assert.Equal(t, tt.wantType, got)
			}
			assert.Equal(t, "embed-large", request["model"])
			assert.Equal(t, []any{"review text"}, request["input"])
			assert.NotContains(t, request, "output_dimension")
		})
	}
}

func TestEmbeddingNormalizesVectors(t *testing.T) {
	client := newEmbeddingTestClient(t, http.StatusOK, `{"data":[{"index":0,"embedding":[3,4]}]}`, "")
	vectors, err := client.Embed(context.Background(), InputDocument, []string{"review text"})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.InDelta(t, 0.6, vectors[0][0], 1e-6)
	assert.InDelta(t, 0.8, vectors[0][1], 1e-6)
}

func TestEmbeddingNormalizesSmallFiniteVectors(t *testing.T) {
	client := newEmbeddingTestClient(t, http.StatusOK, `{"data":[{"index":0,"embedding":[1e-40,0]}]}`, "")
	vectors, err := client.Embed(context.Background(), InputDocument, []string{"review text"})
	require.NoError(t, err)
	require.Len(t, vectors, 1)
	assert.Equal(t, []float32{1, 0}, vectors[0])
}

func TestEmbeddingReordersResponseItemsByIndex(t *testing.T) {
	client := newEmbeddingTestClient(t, http.StatusOK,
		`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`, "")

	vectors, err := client.Embed(context.Background(), InputDocument, []string{"first", "second"})
	require.NoError(t, err)
	assert.Equal(t, [][]float32{{1, 0}, {0, 1}}, vectors)
}

func TestEmbeddingRejectsInvalidResponseIndexes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing",
			body: `{"data":[{"embedding":[1,0]},{"index":1,"embedding":[0,1]}]}`,
			want: "embedding response item 0 is missing index",
		},
		{
			name: "duplicate",
			body: `{"data":[{"index":0,"embedding":[1,0]},{"index":0,"embedding":[0,1]}]}`,
			want: "embedding response contains duplicate index 0",
		},
		{
			name: "negative",
			body: `{"data":[{"index":-1,"embedding":[1,0]},{"index":1,"embedding":[0,1]}]}`,
			want: "embedding response item 0 has index -1; expected 0..1",
		},
		{
			name: "out of range",
			body: `{"data":[{"index":0,"embedding":[1,0]},{"index":2,"embedding":[0,1]}]}`,
			want: "embedding response item 1 has index 2; expected 0..1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newEmbeddingTestClient(t, http.StatusOK, tt.body, "")
			_, err := client.Embed(context.Background(), InputDocument, []string{"first", "second"})
			require.EqualError(t, err, tt.want)
		})
	}
}

func TestEmbeddingRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "response count", body: `{"data":[]}`, want: "embedding response contained 0 vectors for 1 inputs"},
		{name: "dimensions", body: `{"data":[{"index":0,"embedding":[1]}]}`, want: "embedding vector 0 has 1 dimensions; expected 2"},
		{name: "null component", body: `{"data":[{"index":0,"embedding":[1,null]}]}`, want: "embedding vector 0 component 1 is null"},
		{name: "non-finite component", body: `{"data":[{"index":0,"embedding":[1,1e40]}]}`, want: "embedding vector 0 component 1 is not finite"},
		{name: "zero vector", body: `{"data":[{"index":0,"embedding":[0,0]}]}`, want: "embedding vector 0 has zero norm"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newEmbeddingTestClient(t, http.StatusOK, tt.body, "")
			_, err := client.Embed(context.Background(), InputDocument, []string{"review text"})
			require.EqualError(t, err, tt.want)
		})
	}
}

func TestEmbeddingProviderErrorsAreTypedAndSanitized(t *testing.T) {
	tests := []struct {
		status     int
		want       string
		definitive bool
	}{
		{status: http.StatusBadRequest, want: "embedding endpoint returned 400: embedding request rejected", definitive: true},
		{status: http.StatusUnauthorized, want: "embedding endpoint returned 401: embedding authentication rejected", definitive: true},
		{status: http.StatusForbidden, want: "embedding endpoint returned 403: embedding authentication rejected", definitive: true},
		{status: http.StatusNotFound, want: "embedding endpoint returned 404: embedding endpoint or model not found", definitive: true},
		{status: http.StatusTooManyRequests, want: "embedding endpoint returned 429: embedding rate limit exceeded"},
		{status: http.StatusServiceUnavailable, want: "embedding endpoint returned 503: embedding provider unavailable"},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			client := newEmbeddingTestClient(t, tt.status, `{"error":"provider-secret-detail"}`, "7")
			_, err := client.Embed(context.Background(), InputQuery, []string{"query"})
			require.EqualError(t, err, tt.want)
			assert.NotContains(t, err.Error(), "provider-secret-detail")
			var apiErr *APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, tt.status, apiErr.StatusCode)
			assert.Equal(t, 7*time.Second, apiErr.RetryAfter)
			assert.Equal(t, tt.definitive, apiErr.Definitive())
		})
	}
}

func TestEmbeddingRetryAfterHTTPDate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		future := time.Now().UTC().Add(30 * time.Second).Format(http.TimeFormat)
		assert.Equal(t, 30*time.Second, parseRetryAfter(future))
	})
}

func TestEmbeddingRejectsUnsafeHTTPBearerTransport(t *testing.T) {
	_, err := New(Config{
		BaseURL: "http://203.0.113.8/v1",
		Model:   "embed-large",
		Dims:    1024,
		APIKey:  "secret",
	})
	require.EqualError(t, err, "embedding: plaintext HTTP endpoint requires loopback or trusted private network")
}

func TestEmbeddingSanitizesCrossOriginRedirectError(t *testing.T) {
	const reflectedSecret = "provider-reflected-secret"
	var received bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/embeddings?reflected="+reflectedSecret, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	client, err := New(Config{BaseURL: redirector.URL, Model: "embed-large", Dims: 2, APIKey: "secret"})
	require.NoError(t, err)
	_, err = client.Embed(context.Background(), InputDocument, []string{"private review text"})
	require.EqualError(t, err, "embedding: request failed")
	assert.NotContains(t, err.Error(), target.URL)
	assert.NotContains(t, err.Error(), reflectedSecret)
	assert.False(t, received)
}

func TestEmbeddingRequestFailurePreservesContextIdentity(t *testing.T) {
	tests := []struct {
		name      string
		newCtx    func() (context.Context, context.CancelFunc)
		want      error
		wantError string
	}{
		{
			name: "canceled",
			newCtx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			want:      context.Canceled,
			wantError: "embedding: request failed: context canceled",
		},
		{
			name: "deadline exceeded",
			newCtx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Unix(1, 0))
			},
			want:      context.DeadlineExceeded,
			wantError: "embedding: request failed: context deadline exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := New(Config{BaseURL: "http://127.0.0.1:9", Model: "embed-large", Dims: 2})
			require.NoError(t, err)
			ctx, cancel := tt.newCtx()
			defer cancel()

			_, err = client.Embed(ctx, InputDocument, []string{"private review text"})
			require.ErrorIs(t, err, tt.want)
			require.EqualError(t, err, tt.wantError)
		})
	}
}

func TestEmbeddingGenerationFingerprintIncludesVectorSpaceInputs(t *testing.T) {
	base := Config{
		BaseURL:       "http://127.0.0.1:9/v1",
		Model:         "embed-large",
		Dims:          1024,
		RecipeVersion: 3,
		InputTypeMode: "none",
	}
	variants := []Config{
		base,
		withEmbeddingConfig(base, func(c *Config) { c.Model = "embed-other" }),
		withEmbeddingConfig(base, func(c *Config) { c.Dims = 768 }),
		withEmbeddingConfig(base, func(c *Config) { c.RecipeVersion = 4 }),
		withEmbeddingConfig(base, func(c *Config) { c.InputTypeMode = "retrieval" }),
		withEmbeddingConfig(base, func(c *Config) { c.Salt = "deployment-v2" }),
		withEmbeddingConfig(base, func(c *Config) { c.BaseURL = "http://127.0.0.1:10/v1" }),
		withEmbeddingConfig(base, func(c *Config) { c.BaseURL = "http://127.0.0.1:9/v2" }),
	}

	fingerprints := make(map[string]struct{}, len(variants))
	for _, cfg := range variants {
		client, err := New(cfg)
		require.NoError(t, err)
		fingerprints[client.Generation().Fingerprint()] = struct{}{}
	}
	assert.Len(t, fingerprints, len(variants))

	client, err := New(base)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"endpoint": "http://127.0.0.1:9/v1", "input_type_mode": "none", "recipe": "3",
	}, client.Generation().Params)
}

func TestEmbeddingGenerationEndpointIsCanonicalAndExcludesCredentials(t *testing.T) {
	credentialed, err := New(Config{
		BaseURL: "https://user:secret@EXAMPLE.com:443/v1/", Model: "embed-large", Dims: 2,
	})
	require.NoError(t, err)
	plain, err := New(Config{BaseURL: "https://example.com/v1", Model: "embed-large", Dims: 2})
	require.NoError(t, err)

	assert.Equal(t, plain.Generation().Fingerprint(), credentialed.Generation().Fingerprint())
	assert.Equal(t, "https://example.com/v1", credentialed.Generation().Params["endpoint"])
	assert.NotContains(t, credentialed.Generation().Params["endpoint"], "secret")
}

func TestEmbeddingInputTypeRejectsUnknownKind(t *testing.T) {
	client, err := New(Config{BaseURL: "http://127.0.0.1:9", Model: "embed-large", Dims: 2})
	require.NoError(t, err)
	_, err = client.Embed(context.Background(), InputKind("classification"), []string{"text"})
	require.EqualError(t, err, `embedding input kind must be "document" or "query"`)
}

func newEmbeddingTestClient(t *testing.T, status int, body, retryAfter string) *Client {
	return newEmbeddingTestClientWithConfig(t, status, body, retryAfter, Config{Dims: 2})
}

func newEmbeddingTestClientWithConfig(
	t *testing.T,
	status int,
	body, retryAfter string,
	cfg Config,
) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	cfg.BaseURL = server.URL
	cfg.Model = "embed-large"
	client, err := New(cfg)
	require.NoError(t, err)
	return client
}

func withEmbeddingConfig(base Config, mutate func(*Config)) Config {
	mutate(&base)
	return base
}
