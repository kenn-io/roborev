package searchindex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
)

func TestEmbeddingsPlaintextEndpointPolicy(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		trust   bool
		allowed bool
	}{
		{name: "loopback", baseURL: "http://127.0.0.1:9/v1", allowed: true},
		{name: "localhost", baseURL: "http://localhost:9/v1", allowed: true},
		{name: "public address", baseURL: "http://203.0.113.8/v1"},
		{name: "public address trusted", baseURL: "http://203.0.113.8/v1", trust: true},
		{name: "private address", baseURL: "http://10.1.2.3:8080/v1"},
		{name: "private address trusted", baseURL: "http://10.1.2.3:8080/v1", trust: true, allowed: true},
		{name: "cgnat address trusted", baseURL: "http://100.64.0.8/v1", trust: true, allowed: true},
		{name: "host name trusted", baseURL: "http://gpu-box.local:11434/v1", trust: true},
		{name: "https host name", baseURL: "https://gpu-box.example/v1", allowed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewEmbeddings(EmbeddingSettings{
				BaseURL: tt.baseURL, Model: "embed-large", Dims: 2, APIKey: "secret",
				TrustPrivateNetwork: tt.trust,
			})
			if tt.allowed {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
		})
	}
}

func TestEmbeddingsProviderErrorsAreClassifiedWithoutClientRetries(t *testing.T) {
	tests := []struct {
		status     int
		badRequest bool
		definitive bool
	}{
		{status: http.StatusBadRequest, badRequest: true, definitive: true},
		{status: http.StatusUnauthorized, definitive: true},
		{status: http.StatusForbidden, definitive: true},
		{status: http.StatusNotFound, definitive: true},
		{status: http.StatusTooManyRequests},
		{status: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error":"provider-secret-detail"}`))
			}))
			defer server.Close()
			embeddings, err := NewEmbeddings(EmbeddingSettings{BaseURL: server.URL, Model: "embed-large", Dims: 2})
			require.NoError(t, err)

			_, err = embeddings.EncodeFunc(embedconfig.RoleDocument)(context.Background(), []string{"text"})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "provider-secret-detail")
			assert.Equal(t, int32(1), requests.Load(), "the reconciler owns retries")
			apiErr, ok := embeddingAPIError(err)
			require.True(t, ok)
			assert.Equal(t, tt.status, apiErr.StatusCode)
			assert.Equal(t, 7*time.Second, apiErr.RetryAfter)
			assert.Equal(t, tt.definitive, embeddingDefinitive(apiErr))
			assert.Equal(t, tt.badRequest, isEmbeddingBadRequest(err))
		})
	}
}

func TestEmbeddingsRefuseCrossOriginRedirect(t *testing.T) {
	var received atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Store(true)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/embeddings", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	embeddings, err := NewEmbeddings(EmbeddingSettings{
		BaseURL: redirector.URL, Model: "embed-large", Dims: 2, APIKey: "secret",
	})
	require.NoError(t, err)
	_, err = embeddings.EncodeFunc(embedconfig.RoleDocument)(context.Background(), []string{"private review text"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), target.URL)
	assert.False(t, received.Load())
}

func TestEmbeddingsSendRoleOnlyInRetrievalMode(t *testing.T) {
	for _, mode := range []string{"none", "retrieval"} {
		t.Run(mode, func(t *testing.T) {
			var inputTypes []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input     []string `json:"input"`
					InputType string   `json:"input_type"`
				}
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				inputTypes = append(inputTypes, request.InputType)
				data := make([]map[string]any, len(request.Input))
				for i := range request.Input {
					data[i] = map[string]any{"index": i, "embedding": []float64{3, 4}}
				}
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
			}))
			defer server.Close()
			embeddings, err := NewEmbeddings(EmbeddingSettings{
				BaseURL: server.URL, Model: "embed-large", Dims: 2, InputTypeMode: mode,
			})
			require.NoError(t, err)

			vectors, err := embeddings.EncodeFunc(embedconfig.RoleDocument)(context.Background(), []string{"doc"})
			require.NoError(t, err)
			assert.InDeltaSlice(t, []float32{0.6, 0.8}, vectors[0], 1e-6)
			_, err = embeddings.EncodeFunc(embedconfig.RoleQuery)(context.Background(), []string{"query"})
			require.NoError(t, err)
			if mode == "retrieval" {
				assert.Equal(t, []string{"document", "query"}, inputTypes)
			} else {
				assert.Equal(t, []string{"", ""}, inputTypes)
			}
		})
	}
}
