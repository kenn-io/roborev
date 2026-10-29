//go:build integration

package testutil

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// WriteCodexResponse sends one Responses API output item to a Codex test client.
func WriteCodexResponse(t *testing.T, w http.ResponseWriter, item any) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range []struct {
		name string
		data any
	}{
		{"response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_test"}}},
		{"response.output_item.done", map[string]any{"type": "response.output_item.done", "item": item}},
		{"response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_test", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}}},
	} {
		raw, err := json.Marshal(event.data)
		assert.NoError(t, err) //nolint:testifylint // Called by HTTP server goroutines.
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.name, raw)
		assert.NoError(t, err)
	}
}
