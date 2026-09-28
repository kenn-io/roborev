package searchindex

import (
	"context"
	"fmt"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/vector"
)

type embeddingObserver interface {
	ObserveEmbeddingResult(error)
}

type observedEmbedder struct {
	Embedder
	observer embeddingObserver
}

func (e observedEmbedder) EncodeFunc(role embedconfig.Role) vector.EncodeFunc {
	encode := e.Embedder.EncodeFunc(role)
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		vectors, err := encode(ctx, texts)
		if len(texts) > 0 {
			e.observer.ObserveEmbeddingResult(err)
		}
		return vectors, err
	}
}

func authenticationReason(err error) string {
	if apiErr, ok := embeddingAPIError(err); ok && apiErr.CredentialsRejected() {
		return fmt.Sprintf("embedding authentication rejected (%d)", apiErr.StatusCode)
	}
	return ""
}

// ObserveEmbeddingResult updates credentials only after an actual provider call.
// Mirror scans, imported vectors and generation progress are not authentication
// evidence and must never clear a rejection.
func (r *Reconciler) ObserveEmbeddingResult(err error) {
	reason := authenticationReason(err)
	r.mu.Lock()
	defer r.mu.Unlock()
	if reason != "" {
		r.health.Credential = "rejected"
		r.health.CredentialReason = reason
		r.health.LastError = "authentication"
		_, r.health.LastErrorStatus = errorCategory(err)
	} else if err == nil {
		r.health.Credential = "ok"
		r.health.CredentialReason = ""
		if r.health.LastError == "authentication" {
			r.health.LastError = ""
			r.health.LastErrorStatus = 0
		}
	}
}

func clearNonAuthenticationError(health *HealthSnapshot) {
	if health.LastError != "authentication" {
		health.LastError = ""
		health.LastErrorStatus = 0
	}
}
