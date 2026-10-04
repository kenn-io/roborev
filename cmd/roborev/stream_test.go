package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamReconnectWaitUsesContextAndTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		require.NoError(t, waitForStreamReconnect(context.Background(), time.Second))
		assert.Equal(t, time.Second, time.Since(start))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- waitForStreamReconnect(ctx, time.Hour) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		require.ErrorIs(t, <-done, context.Canceled)
		assert.Equal(t, time.Second, time.Since(start))
	})
}

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
