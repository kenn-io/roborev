package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestWaitForJobCompletionReturnsNotFoundImmediately(t *testing.T) {
	var jobCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			jobCalls++
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	review, err := waitForJobCompletion(ctx, server.URL, 123, nil)
	require.Error(t, err)
	assert.Nil(t, review)
	require.ErrorIs(t, err, ErrJobNotFound)
	assert.Equal(t, 1, jobCalls, "expected not-found to fail fast instead of polling until timeout")
}
