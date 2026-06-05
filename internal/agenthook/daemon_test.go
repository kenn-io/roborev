package agenthook

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestShutdownRouteSignalsOnlyOnPost(t *testing.T) {
	assert := assert.New(t)
	state := &StateStore{
		path:     filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{},
	}
	shutdown := make(chan struct{}, 1)
	mux := http.NewServeMux()
	registerRoutes(mux, state, shutdown)

	get := httptest.NewRecorder()
	mux.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/shutdown", nil))
	assert.Equal(http.StatusMethodNotAllowed, get.Code)
	assert.False(channelSignaled(shutdown), "a rejected request must not signal shutdown")

	post := httptest.NewRecorder()
	mux.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/api/shutdown", nil))
	assert.Equal(http.StatusOK, post.Code)
	assert.True(channelSignaled(shutdown), "post must signal shutdown")
}

func channelSignaled(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
