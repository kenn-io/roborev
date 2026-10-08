package testutil

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type PostHogMessage struct {
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	Properties map[string]any `json:"properties"`
}

// NewPostHogStub records plain or gzip-compressed batches from a reporter.
func NewPostHogStub(t *testing.T) (string, func() []PostHogMessage) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reader io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer gz.Close()
			reader = gz
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":1}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []PostHogMessage {
		mu.Lock()
		defer mu.Unlock()
		var messages []PostHogMessage
		for _, body := range bodies {
			var batch struct {
				Batch []PostHogMessage `json:"batch"`
			}
			require.NoError(t, json.Unmarshal(body, &batch))
			messages = append(messages, batch.Batch...)
		}
		return messages
	}
}
