package searchindex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"

	"go.kenn.io/roborev/internal/embedding"
)

func TestCredentialObservationRecovery(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var response atomic.Int32
			response.Store(int32(status))
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if response.Load() != 200 {
					w.WriteHeader(int(response.Load()))
					return
				}
				_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
			}))
			defer server.Close()
			client, err := embedding.New(embedding.Config{BaseURL: server.URL, Model: "test", Dims: 2, APIKey: "example-key", TrustPrivateNetwork: true})
			require.NoError(t, err)
			index := openGenerationTestIndex(t)
			r := NewReconciler(&reconcilerStore{}, index, client, ReconcilerConfig{CredentialSource: "inline"})
			_, err = r.embedder.Embed(t.Context(), embedding.InputDocument, []string{"document"})
			require.Error(t, err)
			assert.Equal(t, "rejected", r.Health().Credential)
			assert.Equal(t, "authentication", r.Health().LastError)
			assert.Equal(t, status, r.Health().LastErrorStatus)
			_, err = r.refreshMirror(t.Context())
			require.NoError(t, err)
			_, err = r.fillGeneration(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "rejected", r.Health().Credential)
			assert.Equal(t, status, r.Health().LastErrorStatus)
			_, err = r.embedder.Embed(t.Context(), embedding.InputDocument, nil)
			require.NoError(t, err)
			assert.Equal(t, "rejected", r.Health().Credential)
			assert.Equal(t, status, r.Health().LastErrorStatus)
			assert.Equal(t, int32(1), requests.Load())
			response.Store(200)
			service := NewService(nil, index, client, r)
			_, err = service.embedder.Embed(t.Context(), embedding.InputQuery, []string{"query"})
			require.NoError(t, err)
			assert.Equal(t, "ok", r.Health().Credential)
			assert.Empty(t, r.Health().CredentialReason)
			assert.Empty(t, r.Health().LastError)
			assert.Zero(t, r.Health().LastErrorStatus)
			assert.Equal(t, int32(2), requests.Load())
		})
	}
}

func TestCredentialDelayedDocumentErrorAfterQueryRecovery(t *testing.T) {
	index := openGenerationTestIndex(t)
	embedder := &reconcilerEmbedder{model: vector.Generation{Model: "test", Dimensions: 2}, batchSize: 1}
	r := NewReconciler(&reconcilerStore{}, index, embedder, ReconcilerConfig{CredentialSource: "inline"})
	delayed := make(chan error, 1)
	resume := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		err := &embedding.APIError{StatusCode: 401}
		r.ObserveEmbeddingResult(err)
		delayed <- err
		<-resume
		r.recordError(err)
		close(finished)
	}()
	<-delayed
	r.ObserveEmbeddingResult(nil)
	close(resume)
	<-finished
	assert.Equal(t, "ok", r.Health().Credential)
	assert.Empty(t, r.Health().CredentialReason)
	assert.NotEqual(t, "authentication", r.Health().LastError)
}

func TestCredentialMissingConfigurationHealth(t *testing.T) {
	r := NewReconciler(&reconcilerStore{}, openGenerationTestIndex(t), nil, ReconcilerConfig{
		CredentialSource: "env:EMBEDDING_KEY", CredentialReason: "no embedding API key (env EMBEDDING_KEY is unset)",
	})
	_, err := r.refreshMirror(context.Background())
	require.NoError(t, err)
	health := r.Health()
	assert.True(t, health.EmbeddingsConfigured)
	assert.Equal(t, "missing", health.Credential)
	assert.Equal(t, "env:EMBEDDING_KEY", health.CredentialSource)
	assert.Contains(t, health.CredentialReason, "no embedding API key")
}

func TestCredentialAuthenticationEvidenceSurvivesUnrelatedErrors(t *testing.T) {
	r := NewReconciler(&reconcilerStore{}, openGenerationTestIndex(t), nil, ReconcilerConfig{CredentialSource: "inline"})
	r.ObserveEmbeddingResult(&embedding.APIError{StatusCode: 401})
	r.recordError(errors.New("synthetic mirror failure"))
	_, err := r.refreshMirror(t.Context())
	require.NoError(t, err)
	health := r.Health()
	assert := assert.New(t)
	assert.Equal("rejected", health.Credential)
	assert.Equal("embedding authentication rejected (401)", health.CredentialReason)
	assert.Equal("authentication", health.LastError)
	assert.Equal(401, health.LastErrorStatus)
	r.ObserveEmbeddingResult(nil)
	health = r.Health()
	assert.Equal("ok", health.Credential)
	assert.Empty(health.LastError)
	assert.Zero(health.LastErrorStatus)
}
