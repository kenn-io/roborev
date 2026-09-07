package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamPreservesLargeFindingsEvent(t *testing.T) {
	event := `{"type":"review.completed","findings":"` + strings.Repeat("x", 2*1024*1024) + `"}` + "\n"
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/stream/events", r.URL.Path)
		_, _ = w.Write([]byte(event))
	}))
	var output bytes.Buffer
	cmd := streamCmd()
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	assert.Equal(t, event, output.String())
}
