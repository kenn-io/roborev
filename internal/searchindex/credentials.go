package searchindex

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/roborev/internal/embedding"
)

type embeddingObserver interface {
	ObserveEmbeddingResult(error)
}

type observedEmbedder struct {
	Embedder
	observer embeddingObserver
}

func (e observedEmbedder) Embed(ctx context.Context, kind embedding.InputKind, texts []string) ([][]float32, error) {
	vectors, err := e.Embedder.Embed(ctx, kind, texts)
	if len(texts) > 0 {
		e.observer.ObserveEmbeddingResult(err)
	}
	return vectors, err
}

func authenticationReason(err error) string {
	if apiErr, ok := errors.AsType[*embedding.APIError](err); ok &&
		(apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
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
