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

	"go.kenn.io/roborev/internal/searchdoc"
)

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
			embeddings, err := NewEmbeddings(embedconfig.Embedder{BaseURL: server.URL, Model: "embed-large", Dims: 2}, "", searchdoc.RecipeVersion)
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
