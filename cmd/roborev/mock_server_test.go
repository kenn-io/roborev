package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMockServerHandler_HandleEnqueue_MethodRouting(t *testing.T) {
	h := &mockServerHandler{
		state: &MockServerState{},
	}

	tests := []struct {
		method     string
		wantStatus int
	}{
		{http.MethodPost, http.StatusCreated},
		{http.MethodGet, http.StatusMethodNotAllowed},
		{http.MethodPut, http.StatusMethodNotAllowed},
		{http.MethodDelete, http.StatusMethodNotAllowed},
		{http.MethodPatch, http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/enqueue", nil)
			w := httptest.NewRecorder()

			h.handleEnqueue(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)
		})
	}
}
