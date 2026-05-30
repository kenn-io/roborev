package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startStreamHandler starts the stream handler in a goroutine and waits for subscription.
// Returns a cancel function, the recorder, and a done channel.
func startStreamHandler(t *testing.T, server *Server, url string) (context.CancelFunc, *safeRecorder, chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, url, nil).WithContext(ctx)
	w := newSafeRecorder()

	initialCount := server.broadcaster.SubscriberCount()

	// Run handler in goroutine
	done := make(chan struct{})
	go func() {
		server.httpServer.Handler.ServeHTTP(w, req)
		close(done)
	}()

	// Wait for handler to subscribe
	if !waitForSubscriberIncrease(server.broadcaster, initialCount, time.Second) {
		cancel()
		require. // Clean up context if timeout
				Condition(t, func() bool {
				return false
			}, "Timed out waiting for subscriber")
	}

	return cancel, w, done
}

func TestHandleStreamEvents(t *testing.T) {
	server, _, _ := newTestServer(t)

	t.Run("returns correct headers", func(t *testing.T) {
		cancel, w, done := startStreamHandler(t, server, "/api/stream/events")

		// Cancel request to stop the handler
		cancel()
		<-done

		// Check headers
		if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected Content-Type 'application/x-ndjson', got '%s'", ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected Cache-Control 'no-cache', got '%s'", cc)
		}
		if conn := w.Header().Get("Connection"); conn != "keep-alive" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected Connection 'keep-alive', got '%s'", conn)
		}
	})

	t.Run("wrong method fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/stream/events", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 405 for POST, got %d", w.Code)
		}
	})

	t.Run("streams events as JSONL", func(t *testing.T) {
		cancel, w, done := startStreamHandler(t, server, "/api/stream/events")

		// Broadcast test events
		event1 := Event{
			Type:     "review.completed",
			TS:       time.Now(),
			JobID:    1,
			Repo:     "/test/repo1",
			RepoName: "repo1",
			SHA:      "abc123",
			Agent:    "test",
			Verdict:  "pass",
		}
		event2 := Event{
			Type:     "review.failed",
			TS:       time.Now(),
			JobID:    2,
			Repo:     "/test/repo2",
			RepoName: "repo2",
			SHA:      "def456",
			Agent:    "test",
			Error:    "agent timeout",
		}
		server.broadcaster.Broadcast(event1)
		server.broadcaster.Broadcast(event2)

		// Wait for events to be written
		if !waitForEvents(w, 2, time.Second) {
			cancel()
			require.Condition(t, func() bool {
				return false
			}, "Timed out waiting for events")
		}

		// Cancel and wait for handler to finish
		cancel()
		<-done

		// Parse JSONL output (safe to read body after handler done)
		body := w.bodyString()
		scanner := bufio.NewScanner(bytes.NewBufferString(body))
		var events []Event
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				require.Condition(t, func() bool {
					return false
				}, "Failed to parse JSONL line: %v, line: %s", err, line)
			}
			events = append(events, ev)
		}

		if len(events) != 2 {
			require.Condition(t, func() bool {
				return false
			}, "Expected 2 events, got %d", len(events))
		}

		if events[0].Type != "review.completed" || events[0].JobID != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "First event mismatch: %+v", events[0])
		}
		if events[0].RepoName != "repo1" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected RepoName 'repo1', got '%s'", events[0].RepoName)
		}
		if events[0].Verdict != "pass" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected Verdict 'pass', got '%s'", events[0].Verdict)
		}
		if events[1].Type != "review.failed" || events[1].JobID != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "Second event mismatch: %+v", events[1])
		}
		if events[1].Error != "agent timeout" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected Error 'agent timeout', got '%s'", events[1].Error)
		}
	})

	t.Run("all event types are supported", func(t *testing.T) {
		cancel, w, done := startStreamHandler(t, server, "/api/stream/events")

		// Broadcast all event types
		server.broadcaster.Broadcast(Event{
			Type:     "review.started",
			JobID:    1,
			Repo:     "/test/repo",
			RepoName: "repo",
			SHA:      "abc",
			Agent:    "codex",
		})
		server.broadcaster.Broadcast(Event{
			Type:     "review.completed",
			JobID:    1,
			Repo:     "/test/repo",
			RepoName: "repo",
			SHA:      "abc",
			Agent:    "codex",
			Verdict:  "pass",
		})
		server.broadcaster.Broadcast(Event{
			Type:     "review.failed",
			JobID:    2,
			Repo:     "/test/repo",
			RepoName: "repo",
			SHA:      "def",
			Agent:    "codex",
			Error:    "connection refused",
		})
		server.broadcaster.Broadcast(Event{
			Type:     "review.canceled",
			JobID:    3,
			Repo:     "/test/repo",
			RepoName: "repo",
			SHA:      "ghi",
			Agent:    "codex",
		})

		if !waitForEvents(w, 4, time.Second) {
			cancel()
			require.Condition(t, func() bool {
				return false
			}, "Timed out waiting for events")
		}
		cancel()
		<-done

		body := w.bodyString()
		lines := strings.Split(strings.TrimSpace(body), "\n")

		// Parse into maps to check actual key presence (not just empty values)
		var rawEvents []map[string]any
		for _, line := range lines {
			if line == "" {
				continue
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(line), &raw); err != nil {
				require.Condition(t, func() bool {
					return false
				}, "Failed to parse JSONL: %v", err)
			}
			rawEvents = append(rawEvents, raw)
		}

		expectedTypes := []string{"review.started", "review.completed", "review.failed", "review.canceled"}
		if len(rawEvents) != len(expectedTypes) {
			require.Condition(t, func() bool {
				return false
			}, "Expected %d events, got %d", len(expectedTypes), len(rawEvents))
		}
		for i, exp := range expectedTypes {
			if rawEvents[i]["type"] != exp {
				assert.Condition(t, func() bool {
					return false
				}, "Event %d: expected type '%s', got '%v'", i, exp, rawEvents[i]["type"])
			}
		}

		// Verify error field is present (key exists) for failed event and absent for others
		for i, raw := range rawEvents {
			eventType, ok := raw["type"].(string)
			if !ok {
				require.Condition(t, func() bool {
					return false
				}, "Event %d: 'type' key missing or not a string", i)
			}
			errorVal, hasError := raw["error"]
			if eventType == "review.failed" {
				if !hasError {
					assert.Condition(t, func() bool {
						return false
					}, "Expected 'error' key present in review.failed event")
				} else if errorStr, ok := errorVal.(string); !ok || errorStr != "connection refused" {
					assert.Condition(t, func() bool {
						return false
					}, "Expected error 'connection refused', got %v", errorVal)
				}
			} else {
				if hasError {
					assert.Condition(t, func() bool {
						return false
					}, "Unexpected 'error' key in %s event", eventType)
				}
			}
		}

		// Verify verdict field is present (key exists) only in completed event
		for i, raw := range rawEvents {
			eventType, ok := raw["type"].(string)
			if !ok {
				require.Condition(t, func() bool {
					return false
				}, "Event %d: 'type' key missing or not a string", i)
			}
			verdictVal, hasVerdict := raw["verdict"]
			if eventType == "review.completed" {
				if !hasVerdict {
					assert.Condition(t, func() bool {
						return false
					}, "Expected 'verdict' key present in review.completed event")
				} else if verdictStr, ok := verdictVal.(string); !ok || verdictStr != "pass" {
					assert.Condition(t, func() bool {
						return false
					}, "Expected verdict 'pass', got %v", verdictVal)
				}
			} else {
				if hasVerdict {
					assert.Condition(t, func() bool {
						return false
					}, "Unexpected 'verdict' key in %s event", eventType)
				}
			}
		}
	})

	t.Run("omitempty fields are excluded when empty", func(t *testing.T) {
		cancel, w, done := startStreamHandler(t, server, "/api/stream/events")

		// Event with no optional fields set
		server.broadcaster.Broadcast(Event{
			Type:     "review.started",
			JobID:    1,
			Repo:     "/test/repo",
			RepoName: "repo",
			SHA:      "abc",
			// Agent, Verdict, Error all empty
		})

		if !waitForEvents(w, 1, time.Second) {
			cancel()
			require.Condition(t, func() bool {
				return false
			}, "Timed out waiting for events")
		}
		cancel()
		<-done

		body := w.bodyString()
		// Check that empty fields are not in the JSON output
		if bytes.Contains([]byte(body), []byte(`"verdict"`)) {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 'verdict' to be omitted when empty")
		}
		if bytes.Contains([]byte(body), []byte(`"error"`)) {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 'error' to be omitted when empty")
		}
	})

	t.Run("repo filter only sends matching events", func(t *testing.T) {
		// Filter for repo1 only
		cancel, w, done := startStreamHandler(t, server, "/api/stream/events?repo="+url.QueryEscape("/test/repo1"))

		// Broadcast events for different repos
		server.broadcaster.Broadcast(Event{
			Type:     "review.completed",
			JobID:    10,
			Repo:     "/test/repo1",
			RepoName: "repo1",
			SHA:      "aaa",
		})
		server.broadcaster.Broadcast(Event{
			Type:     "review.completed",
			JobID:    11,
			Repo:     "/test/repo2", // Should be filtered out
			RepoName: "repo2",
			SHA:      "bbb",
		})
		server.broadcaster.Broadcast(Event{
			Type:     "review.completed",
			JobID:    12,
			Repo:     "/test/repo1",
			RepoName: "repo1",
			SHA:      "ccc",
		})

		// Wait for events to be written (expect 2 for repo1, repo2 event is filtered)
		if !waitForEvents(w, 2, time.Second) {
			cancel()
			require.Condition(t, func() bool {
				return false
			}, "Timed out waiting for events")
		}

		// Cancel and wait
		cancel()
		<-done

		// Parse JSONL output
		body := w.bodyString()
		scanner := bufio.NewScanner(bytes.NewBufferString(body))
		var events []Event
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				require.Condition(t, func() bool {
					return false
				}, "Failed to parse JSONL line: %v", err)
			}
			events = append(events, ev)
		}

		// Should only have 2 events (repo1), not the repo2 event
		if len(events) != 2 {
			require.Condition(t, func() bool {
				return false
			}, "Expected 2 events (filtered), got %d", len(events))
		}

		for _, ev := range events {
			if ev.Repo != "/test/repo1" {
				assert.Condition(t, func() bool {
					return false
				}, "Expected all events for /test/repo1, got event for %s", ev.Repo)
			}
		}
	})

	t.Run("special characters in repo filter are handled", func(t *testing.T) {
		repoPath := "/test/my repo with spaces"
		cancel, w, done := startStreamHandler(t, server, "/api/stream/events?repo="+url.QueryEscape(repoPath))

		// Broadcast event for the repo with spaces
		server.broadcaster.Broadcast(Event{
			Type:     "review.completed",
			JobID:    20,
			Repo:     repoPath,
			RepoName: "my repo with spaces",
			SHA:      "xyz",
		})
		// Broadcast event for different repo (should be filtered)
		server.broadcaster.Broadcast(Event{
			Type:     "review.completed",
			JobID:    21,
			Repo:     "/test/other",
			RepoName: "other",
			SHA:      "zzz",
		})

		// Wait for the event that passes filter
		if !waitForEvents(w, 1, time.Second) {
			cancel()
			require.Condition(t, func() bool {
				return false
			}, "Timed out waiting for events")
		}
		cancel()
		<-done

		// Parse output
		body := w.bodyString()
		scanner := bufio.NewScanner(bytes.NewBufferString(body))
		var events []Event
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				require.Condition(t, func() bool {
					return false
				}, "Failed to parse JSONL line: %v", err)
			}
			events = append(events, ev)
		}

		if len(events) != 1 {
			require.Condition(t, func() bool {
				return false
			}, "Expected 1 event for repo with spaces, got %d", len(events))
		}
		if events[0].Repo != repoPath {
			assert.Condition(t, func() bool {
				return false
			}, "Expected repo '%s', got '%s'", repoPath, events[0].Repo)
		}
	})
}
