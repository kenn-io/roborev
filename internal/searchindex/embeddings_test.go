package searchindex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/searchdoc"
)

func TestEmbeddingsProviderErrorsAreClassifiedWithoutClientRetries(t *testing.T) {
	secret := `{"error":"provider-secret-detail"}`
	tests := []struct {
		name          string
		status        int
		body          string
		inputRejected bool
		definitive    bool
	}{
		{
			name: "input too long", status: http.StatusBadRequest, inputRejected: true, definitive: true,
			body: `{"error":{"message":"input is too long for the context length (provider-secret-detail)","code":"context_length_exceeded"}}`,
		},
		{
			name: "unsupported dimensions", status: http.StatusBadRequest, definitive: true,
			body: `{"error":{"message":"dimensions 2 is not supported (provider-secret-detail)"}}`,
		},
		{name: "unclassified bad request", status: http.StatusBadRequest, body: secret, definitive: true},
		{name: "unauthorized", status: http.StatusUnauthorized, body: secret, definitive: true},
		{name: "forbidden", status: http.StatusForbidden, body: secret, definitive: true},
		{name: "not found", status: http.StatusNotFound, body: secret, definitive: true},
		{name: "rate limited", status: http.StatusTooManyRequests, body: secret},
		{name: "unavailable", status: http.StatusServiceUnavailable, body: secret},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			embeddings, err := NewEmbeddings(config.SearchEmbeddingsConfig{BaseURL: server.URL, Model: "embed-large", Dims: 2}, "", searchdoc.RecipeVersion)
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
			assert.Equal(t, tt.inputRejected, isEmbeddingInputRejected(err))
		})
	}
}
