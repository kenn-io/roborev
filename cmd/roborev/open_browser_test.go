package main

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenBrowserUsesPlatformCommandWithoutShell(t *testing.T) {
	tests := []struct {
		goos     string
		wantName string
		wantArgs []string
	}{
		{goos: "darwin", wantName: "open", wantArgs: []string{"https://example.com/reviews"}},
		{goos: "windows", wantName: "rundll32", wantArgs: []string{"url.dll,FileProtocolHandler", "https://example.com/reviews"}},
		{goos: "linux", wantName: "xdg-open", wantArgs: []string{"https://example.com/reviews"}},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			originalGOOS := browserGOOS
			originalStart := startBrowserCommand
			browserGOOS = tt.goos
			var gotName string
			var gotArgs []string
			startBrowserCommand = func(name string, args ...string) (func() error, error) {
				gotName = name
				gotArgs = append([]string(nil), args...)
				return func() error { return nil }, nil
			}
			t.Cleanup(func() {
				browserGOOS = originalGOOS
				startBrowserCommand = originalStart
			})

			require.NoError(t, platformOpenBrowserURL("https://example.com/reviews"))
			assert.Equal(t, tt.wantName, gotName)
			assert.Equal(t, tt.wantArgs, gotArgs)
			assert.NotEqual(t, "sh", gotName)
			assert.NotEqual(t, "cmd", gotName)
		})
	}
}

func TestOpenBrowserDoesNotWaitForLongRunningOpener(t *testing.T) {
	originalGOOS := browserGOOS
	originalStart := startBrowserCommand
	browserGOOS = "linux"
	release := make(chan struct{})
	startBrowserCommand = func(string, ...string) (func() error, error) {
		return func() error {
			<-release
			return nil
		}, nil
	}
	t.Cleanup(func() {
		close(release)
		browserGOOS = originalGOOS
		startBrowserCommand = originalStart
	})

	returned := make(chan error, 1)
	go func() {
		returned <- platformOpenBrowserURL("https://example.com/reviews")
	}()
	require.Eventually(t, func() bool {
		return len(returned) == 1
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, <-returned)
}

func TestOpenBrowserReturnsStartFailure(t *testing.T) {
	originalGOOS := browserGOOS
	originalStart := startBrowserCommand
	browserGOOS = "linux"
	want := errors.New("opener unavailable")
	startBrowserCommand = func(string, ...string) (func() error, error) {
		return nil, want
	}
	t.Cleanup(func() {
		browserGOOS = originalGOOS
		startBrowserCommand = originalStart
	})

	require.ErrorIs(t, platformOpenBrowserURL("https://example.com/reviews"), want)
}
