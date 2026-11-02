//go:build !windows

package provenance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestAgentsviewCommandsBoundInheritedPipesOnCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, string, string) error
	}{
		{
			name: "why",
			run: func(ctx context.Context, bin, repo string) error {
				_, err := runWhy(ctx, bin, repo, testSHA)
				return err
			},
		},
		{
			name: "probe",
			run: func(ctx context.Context, bin, repo string) error {
				return (&Client{Command: bin}).Probe(ctx, repo, config.DefaultConfig())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "child.pid")
			bin := filepath.Join(dir, "agentsview")
			script := fmt.Sprintf("#!/bin/sh\nsleep 60 &\nchild=$!\nprintf '%%s' \"$child\" > %s\nif [ \"$2\" = --help ]; then printf '%%s' '--commit'; else printf '%%s' '{\"refs\":[]}'; fi\nwait \"$child\"\n", strconv.Quote(pidFile))
			require.NoError(t, os.WriteFile(bin, []byte(script), 0o700))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tc.run(ctx, bin, dir) }()

			var child *os.Process
			t.Cleanup(func() {
				if child != nil {
					_ = child.Kill()
				}
			})
			var pid int
			// Wall-clock wait: the synthetic subprocess must publish its PID before cleanup.
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(pidFile)
				if err != nil {
					return false
				}
				pid, err = strconv.Atoi(string(data))
				return err == nil
			}, time.Second, 10*time.Millisecond)
			var err error
			child, err = os.FindProcess(pid)
			require.NoError(t, err)
			cancel()

			// Wall-clock wait: cancellation kills the parent, while its child retains stdout.
			var runErr error
			require.Eventually(t, func() bool {
				select {
				case runErr = <-done:
					return true
				default:
					return false
				}
			}, 2*time.Second, 10*time.Millisecond, "command should finish after cancellation despite its child retaining the output pipe")
			require.Error(t, runErr)
		})
	}
}

func TestLookupStopsAfterCLITimeout(t *testing.T) {
	dir := t.TempDir()
	firstFile := filepath.Join(dir, "first")
	timedOutFile := filepath.Join(dir, "timed-out")
	pidFile := filepath.Join(dir, "child.pid")
	subsequentCalls := filepath.Join(dir, "subsequent-calls")
	bin := filepath.Join(dir, "agentsview")
	script := fmt.Sprintf("#!/bin/sh\nif [ ! -e %s ]; then : > %s; printf '%%s' '{\"refs\":[]}'; exit 0; fi\nif [ ! -e %s ]; then : > %s; sleep 60 & child=$!; printf '%%s' \"$child\" > %s; wait \"$child\"; fi\nprintf x >> %s\nprintf '%%s' '{\"refs\":[]}'\n", strconv.Quote(firstFile), strconv.Quote(firstFile), strconv.Quote(timedOutFile), strconv.Quote(timedOutFile), strconv.Quote(pidFile), strconv.Quote(subsequentCalls))
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o700))
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(string(data))
		if err != nil {
			return
		}
		child, err := os.FindProcess(pid)
		if err == nil {
			_ = child.Kill()
		}
	})

	shas := make([]string, 181)
	for i := range shas {
		shas[i] = fmt.Sprintf("%040x", i+1)
	}
	cfg := config.DefaultConfig()
	cfg.Cost.Timeout = "100ms"
	result, err := (&Client{Command: bin}).Lookup(context.Background(), dir, shas, cfg)

	require.Error(t, err)
	require.FileExists(t, timedOutFile)
	require.NoFileExists(t, subsequentCalls, "a provider timeout must stop later commit lookups")
	require.Equal(t, map[string]string{shas[0]: "why-selected"}, result.Coverage)
}
