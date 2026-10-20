package telemetry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"
)

func TestClientGateReceiptsStayWithEndpoint(t *testing.T) {
	for _, receipt := range []string{"disabled", "queued"} {
		t.Run(receipt, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			var requestsA, requestsB atomic.Int32
			a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestsA.Add(1)
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"` + receipt + `"}`))
			}))
			defer a.Close()
			b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestsB.Add(1)
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"queued"}`))
			}))
			defer b.Close()
			gate := &ClientGate{}
			_, err := gate.Post(t.Context(), a.Client(), "endpoint-a", a.URL, EventAppOpened, nil, true)
			require.NoError(t, err)
			assert.False(t, gate.ReadyEvent("endpoint-a", EventAppOpened, "", ""))
			assert.True(t, gate.ReadyEvent("endpoint-b", EventAppOpened, "", ""))
			gate = &ClientGate{}
			_, err = gate.Post(t.Context(), b.Client(), "endpoint-b", b.URL, EventAppOpened, nil, true)
			require.NoError(t, err)
			_, err = gate.Post(t.Context(), a.Client(), "endpoint-a", a.URL, EventAppOpened, nil, true)
			require.NoError(t, err)
			assert.Equal(t, int32(1), requestsA.Load())
			assert.Equal(t, int32(1), requestsB.Load())
		})
	}
}

func TestClientGateLockContentionSkipsAndRecovers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)
	path := filepath.Join(dir, "telemetry-client.json")
	receipt := `{"version":1,"day":"` + time.Now().UTC().Format(time.DateOnly) + `","endpoints":{"endpoint-a":{"disabled":true,"events":{}}}}`
	require.NoError(t, os.WriteFile(path, []byte(receipt), 0o600))
	lock := flock.New(path + ".lock")
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { _ = lock.Unlock() })
	transport := &failedGateTransport{}
	client := &http.Client{Transport: transport}
	gate := &ClientGate{}
	// The OS file lock remains held through the acquisition timeout.
	status, err := gate.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAppOpened, nil, true)
	require.NoError(t, err)
	assert.Equal(t, posthog.StatusSkipped, status)
	assert.Zero(t, transport.requests.Load())
	require.NoError(t, lock.Unlock())
	status, err = gate.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
	require.NoError(t, err)
	assert.Equal(t, posthog.StatusSkipped, status)
	assert.Zero(t, transport.requests.Load())
	assert.False(t, gate.ReadyEvent("endpoint-a", EventAgentActive, "", ""))
}

func TestClientGateStorageFailureUsesMemory(t *testing.T) {
	for _, failure := range []string{"lock", "read"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ROBOREV_DATA_DIR", dir)
			name := "telemetry-client.json"
			if failure == "lock" {
				name += ".lock"
			}
			require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0o700))
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"queued"}`))
			}))
			defer srv.Close()
			gate := &ClientGate{}
			status, err := gate.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAppOpened, nil, true)
			require.NoError(t, err)
			assert.Equal(t, posthog.StatusQueued, status)
			status, err = gate.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAppOpened, nil, true)
			require.NoError(t, err)
			assert.Equal(t, posthog.StatusSkipped, status)
			assert.Equal(t, int32(1), requests.Load())
			assert.False(t, gate.ReadyEvent("endpoint-a", EventAppOpened, "", ""))
		})
	}
}

func TestClientGateUnversionedStateStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)
	path := filepath.Join(dir, "telemetry-client.json")
	receipt := `{"day":"` + time.Now().UTC().Format(time.DateOnly) + `","endpoints":{"endpoint-a":{"disabled":true,"events":{}}}}`
	require.NoError(t, os.WriteFile(path, []byte(receipt), 0o600))
	gate := &ClientGate{}
	gate.Load()
	assert.True(t, gate.ReadyEvent("endpoint-a", EventAppOpened, "", ""))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	}))
	defer srv.Close()
	status, err := gate.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAppOpened, nil, true)
	require.NoError(t, err)
	assert.Equal(t, posthog.StatusQueued, status)
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(contents), `"version":1`)
}

func TestClientGateSettledReceiptsDoNotReplaceStateFile(t *testing.T) {
	for _, receipt := range []string{"queued", "disabled"} {
		t.Run(receipt, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ROBOREV_DATA_DIR", dir)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"` + receipt + `"}`))
			}))
			defer srv.Close()
			gate := &ClientGate{}
			for _, scope := range []string{"endpoint-a", "endpoint-b"} {
				_, err := gate.Post(t.Context(), srv.Client(), scope, srv.URL, EventAppOpened, nil, true)
				require.NoError(t, err)
			}
			path := filepath.Join(dir, "telemetry-client.json")
			before, err := os.Stat(path)
			require.NoError(t, err)
			for range 10 {
				gate = &ClientGate{}
				gate.Load()
				assert.False(t, gate.ReadyEvent("endpoint-a", EventAppOpened, "", ""))
				status, err := gate.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAppOpened, nil, true)
				require.NoError(t, err)
				assert.Equal(t, posthog.StatusSkipped, status)
			}
			after, err := os.Stat(path)
			require.NoError(t, err)
			assert.True(t, os.SameFile(before, after), "settled receipts must leave the existing state file in place")
		})
	}
}

func TestClientGateNormalizesMissingEventMap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)
	receipt := `{"version":1,"day":"` + time.Now().UTC().Format(time.DateOnly) + `","endpoints":{"endpoint-a":{"disabled":false}}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry-client.json"), []byte(receipt), 0o600))
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	}))
	defer srv.Close()
	status, err := (&ClientGate{}).Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAppOpened, nil, true)
	require.NoError(t, err)
	assert.Equal(t, posthog.StatusQueued, status)
	assert.Equal(t, int32(1), requests.Load())
}

func TestClientGateLeavesUnsupportedStateUntouched(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)
	path := filepath.Join(dir, "telemetry-client.json")
	contents := []byte(`{"version":2,"day":"` + time.Now().UTC().Format(time.DateOnly) + `","future":true,"endpoints":{"endpoint-a":{"events":{}}}}`)
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	gate := &ClientGate{}
	gate.Load()
	assert.False(t, gate.ReadyEvent("endpoint-a", EventAppOpened, "", ""))
	transport := &failedGateTransport{}
	status, err := gate.Post(t.Context(), &http.Client{Transport: transport}, "endpoint-a", "http://example.com", EventAppOpened, nil, true)
	require.NoError(t, err)
	assert.Equal(t, posthog.StatusSkipped, status)
	assert.Zero(t, transport.requests.Load())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, contents, after)
}

func TestClientGateWaitUsesCallerDeadline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)
	lock := flock.New(filepath.Join(dir, "telemetry-client.json.lock"))
	require.NoError(t, lock.Lock())
	lockHeld := true
	t.Cleanup(func() {
		if lockHeld {
			_ = lock.Unlock()
		}
	})

	synctest.Test(t, func(t *testing.T) {
		const callers = 8
		gate := &ClientGate{}
		transport := &failedGateTransport{}
		client := &http.Client{Transport: transport}
		done := make(chan struct{}, callers)
		start := time.Now()
		for range callers {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
				defer cancel()
				_, _ = gate.Post(ctx, client, "endpoint-a", "http://example.com", EventAppOpened, nil, true)
				done <- struct{}{}
			}()
		}
		for range callers {
			<-done
		}
		assert.LessOrEqual(t, time.Since(start), 40*time.Millisecond, "receipt updates waited past their caller deadline")
		assert.Zero(t, transport.requests.Load())
	})
	require.NoError(t, lock.Unlock())
	lockHeld = false
}

type overlappingGateTransport struct {
	requests     atomic.Int32
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func (t *overlappingGateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.requests.Add(1) == 1 {
		close(t.firstStarted)
		<-t.releaseFirst
		return nil, errors.New("late request failure")
	}
	return &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"status":"queued"}`)),
		Request:    req,
	}, nil
}

func TestClientGateLateFailureAfterSuccessDoesNotResetReceipt(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	var nowMu sync.Mutex
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	transport := &overlappingGateTransport{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
	client := &http.Client{Transport: transport}
	gateA, gateB := &ClientGate{now: clock}, &ClientGate{now: clock}
	firstDone := make(chan error, 1)
	go func() {
		_, err := gateA.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAppOpened, nil, true)
		firstDone <- err
	}()
	<-transport.firstStarted
	nowMu.Lock()
	now = now.Add(time.Second)
	nowMu.Unlock()
	status, err := gateB.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAppOpened, nil, true)
	require.NoError(t, err)
	assert.Equal(t, posthog.StatusQueued, status)
	close(transport.releaseFirst)
	require.Error(t, <-firstDone)
	status, err = (&ClientGate{now: clock}).Post(t.Context(), client, "endpoint-a", "http://example.com", EventAppOpened, nil, true)
	require.NoError(t, err)
	assert.Equal(t, posthog.StatusSkipped, status)
	assert.Equal(t, int32(2), transport.requests.Load())
}

func TestClientGateReceiptsAndRollover(t *testing.T) {
	for _, receipt := range []string{"queued", "skipped", "disabled"} {
		t.Run(receipt, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"` + receipt + `"}`))
			}))
			defer srv.Close()
			gate := &ClientGate{now: func() time.Time { return now }}
			props := map[string]any{PropertySurface: SurfaceCLI}
			status, err := gate.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAppOpened, props, true)
			require.NoError(t, err)
			assert.Equal(t, posthog.Status(receipt), status)
			// A new process reads the persisted receipt.
			gate = &ClientGate{now: func() time.Time { return now }}
			_, err = gate.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAppOpened, props, true)
			require.NoError(t, err)
			assert.Equal(t, 1, requests)
			if receipt == "disabled" {
				_, err = gate.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventSessionEnded, props, false)
				require.NoError(t, err)
				assert.Equal(t, 1, requests)
			}
			now = now.AddDate(0, 0, 1)
			_, err = gate.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAppOpened, props, true)
			require.NoError(t, err)
			assert.Equal(t, 2, requests)
		})
	}
}

func TestClientGatePersistsAgentAttemptWhenCallerTimesOut(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		transport := &timeoutGateTransport{}
		client := &http.Client{Transport: transport}

		gate := &ClientGate{}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := gate.Post(ctx, client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
			done <- err
		}()
		synctest.Wait()
		require.Error(t, <-done)
		assert.Equal(t, int32(1), transport.requests.Load())
		// A call during backoff may still queue; the gate paces its delivery.
		assert.True(t, gate.ReadyEvent("endpoint-a", EventAgentActive, "", ""))

		restarted := &ClientGate{}
		restarted.Load()
		assert.True(t, restarted.ReadyEvent("endpoint-a", EventAgentActive, "", ""))
		status, err := restarted.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
		require.NoError(t, err)
		assert.Equal(t, posthog.StatusSkipped, status)
		assert.Equal(t, int32(1), transport.requests.Load())
	})
}

func TestClientGateKeepsAgentBackoffInMemoryWhenLockStaysHeld(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		lock := flock.New(filepath.Join(os.Getenv("ROBOREV_DATA_DIR"), "telemetry-client.json.lock"))
		lockHeld := make(chan struct{})
		lockErrors := make(chan error, 1)
		transport := &timeoutGateTransport{onStart: func() {
			if err := lock.Lock(); err != nil {
				lockErrors <- err
				return
			}
			close(lockHeld)
		}}
		client := &http.Client{Transport: transport}
		gate := &ClientGate{}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := gate.Post(ctx, client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
			done <- err
		}()
		select {
		case <-lockHeld:
			defer func() { _ = lock.Unlock() }()
		case err := <-lockErrors:
			require.NoError(t, err)
		}
		synctest.Wait()
		require.Error(t, <-done)
		assert.True(t, gate.ReadyEvent("endpoint-a", EventAgentActive, "", ""))
		status, err := gate.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
		require.NoError(t, err)
		assert.Equal(t, posthog.StatusSkipped, status)
		assert.Equal(t, int32(1), transport.requests.Load())
	})
}

func TestClientGateQueuesOverlappingAgentCalls(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	var requests atomic.Int32
	arrived, release := make(chan struct{}), make(chan struct{})
	releaseResponse := sync.OnceFunc(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(arrived)
			<-release
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"status":"queued"}`)
	}))
	defer func() { releaseResponse(); srv.Close() }()

	firstGate := &ClientGate{}
	secondGate := &ClientGate{}
	firstDone := make(chan error, 1)
	go func() {
		_, err := firstGate.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAgentActive, nil, false)
		firstDone <- err
	}()
	<-arrived
	status, err := secondGate.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAgentActive, nil, false)
	require.NoError(t, err)
	assert.Equal(t, posthog.StatusSkipped, status)
	assert.Equal(t, int32(1), requests.Load())

	releaseResponse()
	require.NoError(t, <-firstDone)
	assert.Equal(t, int32(2), requests.Load())
	status, err = secondGate.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAgentActive, nil, false)
	require.NoError(t, err)
	assert.Equal(t, posthog.StatusQueued, status)
	assert.Equal(t, int32(3), requests.Load())
}

func TestClientGateAgentAttemptSurvivesProcessExit(t *testing.T) {
	const childEnv = "ROBOREV_TEST_AGENT_ATTEMPT_CHILD"
	const startedEnv = "ROBOREV_TEST_AGENT_ATTEMPT_STARTED"
	const exitEnv = "ROBOREV_TEST_AGENT_ATTEMPT_EXIT"
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	if os.Getenv(childEnv) == "1" {
		gate := &ClientGate{now: clock}
		gate.Load()
		client := &http.Client{Transport: clientGateRoundTripper(func(req *http.Request) (*http.Response, error) {
			if err := os.WriteFile(os.Getenv(startedEnv), []byte("started"), 0o600); err != nil {
				os.Exit(2)
			}
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}
		go gate.Post(context.Background(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(os.Getenv(exitEnv)); err == nil {
				os.Exit(0)
			}
			time.Sleep(time.Millisecond)
		}
		os.Exit(3)
	}

	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	startedPath := filepath.Join(os.Getenv("ROBOREV_DATA_DIR"), "request-started")
	exitPath := filepath.Join(os.Getenv("ROBOREV_DATA_DIR"), "exit")
	cmd := exec.Command(os.Args[0], "-test.run=^TestClientGateAgentAttemptSurvivesProcessExit$")
	cmd.Env = append(os.Environ(), childEnv+"=1", startedEnv+"="+startedPath, exitEnv+"="+exitPath)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(startedPath)
		return err == nil
	}, 3*time.Second, time.Millisecond)
	require.NoError(t, os.WriteFile(exitPath, []byte("exit"), 0o600))
	require.NoError(t, cmd.Wait())

	var requests atomic.Int32
	client := &http.Client{Transport: clientGateRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"status":"queued"}`)),
			Request:    req,
		}, nil
	})}
	gate := &ClientGate{now: clock}
	gate.Load()
	assert.True(t, gate.ReadyEvent("endpoint-a", EventAgentActive, "", ""))
	status, err := gate.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
	require.NoError(t, err)
	assert.Equal(t, posthog.StatusSkipped, status)
	assert.Zero(t, requests.Load())

	now = now.Add(NotificationTimeout + backoffDelay(1))
	status, err = gate.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
	require.NoError(t, err)
	assert.Equal(t, posthog.StatusQueued, status)
	assert.Equal(t, int32(3), requests.Load())
}

func TestClientGateKeepsSuccessfulReceiptInMemoryWhenLockStaysHeld(t *testing.T) {
	for _, receipt := range []string{"queued", "disabled"} {
		t.Run(receipt, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				lock := flock.New(filepath.Join(os.Getenv("ROBOREV_DATA_DIR"), "telemetry-client.json.lock"))
				transport := &lockedReceiptTransport{
					lock:     lock,
					receipt:  receipt,
					lockHeld: make(chan struct{}),
					lockErr:  make(chan error, 1),
				}
				client := &http.Client{Transport: transport}
				gate := &ClientGate{}
				done := make(chan struct {
					status posthog.Status
					err    error
				}, 1)
				go func() {
					status, err := gate.Post(
						t.Context(), client, "endpoint-a", "http://example.com",
						EventAppOpened, map[string]any{PropertySurface: SurfaceCLI}, true,
					)
					done <- struct {
						status posthog.Status
						err    error
					}{status, err}
				}()
				select {
				case <-transport.lockHeld:
					defer func() { _ = lock.Unlock() }()
				case err := <-transport.lockErr:
					require.NoError(t, err)
				}
				synctest.Wait()
				result := <-done
				require.NoError(t, result.err)
				assert.Equal(t, posthog.Status(receipt), result.status)
				assert.Equal(t, receipt == "queued", gate.AwaitProbe(t.Context(), "endpoint-a", EventAppOpened, SurfaceCLI))
				assert.False(t, gate.ReadyEvent("endpoint-a", EventAppOpened, SurfaceCLI, ""))

				status, err := gate.Post(
					t.Context(), client, "endpoint-a", "http://example.com",
					EventAppOpened, map[string]any{PropertySurface: SurfaceCLI}, true,
				)
				require.NoError(t, err)
				assert.Equal(t, posthog.StatusSkipped, status)
				assert.Equal(t, int32(1), transport.requests.Load())
			})
		})
	}
}

type timeoutGateTransport struct {
	requests atomic.Int32
	onStart  func()
}

type clientGateRoundTripper func(*http.Request) (*http.Response, error)

func (f clientGateRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (t *timeoutGateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.requests.Add(1)
	if t.onStart != nil {
		t.onStart()
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

type lockedReceiptTransport struct {
	lock     *flock.Flock
	receipt  string
	lockHeld chan struct{}
	lockErr  chan error
	requests atomic.Int32
}

func (t *lockedReceiptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.requests.Add(1)
	if err := t.lock.Lock(); err != nil {
		t.lockErr <- err
		return nil, err
	}
	close(t.lockHeld)
	body := `{"status":"` + t.receipt + `"}`
	return &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

type failedGateTransport struct{ requests atomic.Int32 }

func (f *failedGateTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.requests.Add(1)
	return nil, errors.New("unavailable")
}

func TestClientGateBackoffAndMemoryFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "memory"}[fallback], func(t *testing.T) {
			dir := t.TempDir()
			if fallback {
				dir += "/file"
				require.NoError(t, os.WriteFile(dir, nil, 0o600))
			}
			t.Setenv("ROBOREV_DATA_DIR", dir)
			synctest.Test(t, func(t *testing.T) {
				gate := &ClientGate{}
				transport := &failedGateTransport{}
				client := &http.Client{Transport: transport}
				send := func() {
					_, _ = gate.Post(context.Background(), client, "endpoint-a", "http://example.com", EventScreenViewed, map[string]any{PropertySurface: SurfaceTUI, PropertyScreen: "queue"}, true)
				}
				for _, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 16 * time.Second} {
					send()
					count := transport.requests.Load()
					for range 100 {
						send()
					}
					assert.Equal(t, count, transport.requests.Load())
					time.Sleep(delay)
				}
				send()
				assert.Equal(t, int32(6), transport.requests.Load())
			})
		})
	}
}

func TestClientGateClaimsBeforeReceiptAndKeepsVisits(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	var requests atomic.Int32
	arrived, release := make(chan struct{}), make(chan struct{})
	releaseResponse := sync.OnceFunc(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(arrived)
			<-release
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	}))
	defer func() { releaseResponse(); srv.Close() }()
	gate := &ClientGate{}
	props := map[string]any{PropertySurface: SurfaceCLI}
	done := make(chan struct{})
	go func() {
		_, _ = gate.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAppOpened, props, true)
		close(done)
	}()
	<-arrived
	restarted := &ClientGate{}
	status, err := restarted.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAppOpened, props, true)
	require.NoError(t, err)
	assert.Equal(t, posthog.StatusSkipped, status)
	assert.Equal(t, int32(1), requests.Load())
	releaseResponse()
	<-done
	for range 3 {
		_, err = restarted.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAgentActive, nil, false)
		require.NoError(t, err)
		_, err = restarted.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventSessionEnded, props, false)
		require.NoError(t, err)
	}
	assert.Equal(t, int32(7), requests.Load())
}

func TestClientGateRetriesAgentCallWithSameID(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	var calls []string
	client := &http.Client{Transport: clientGateRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.URL.Query().Get("call"))
		code, body := http.StatusAccepted, `{"status":"queued"}`
		if len(calls) == 1 {
			code, body = http.StatusInternalServerError, ""
		}
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	gate := &ClientGate{now: func() time.Time { return now }}
	_, err := gate.Post(t.Context(), client, "endpoint-a", "http://example.com/api/telemetry/agent-call", EventAgentActive, nil, false)
	require.Error(t, err)
	now = now.Add(backoffDelay(1))
	status, err := gate.Post(t.Context(), client, "endpoint-a", "http://example.com/api/telemetry/agent-call", EventAgentActive, nil, false)
	require.NoError(t, err)
	assert.Equal(t, posthog.StatusQueued, status)
	// The failed call is resent under its ID; the next queued call gets its own.
	require.Len(t, calls, 3)
	assert.NotEmpty(t, calls[0])
	assert.Equal(t, calls[0], calls[1])
	assert.NotEqual(t, calls[1], calls[2])
}

func TestClientGateAwaitProbeWaitsForAnotherSender(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)
	// The other sender's request is its sixth attempt, so this process may not send one itself.
	receipt := `{"version":1,"day":"` + time.Now().UTC().Format(time.DateOnly) + `","endpoints":{"endpoint-a":{"events":{"app_opened:cli:":{"attempts":5}}}}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry-client.json"), []byte(receipt), 0o600))
	arrived, release := make(chan struct{}), make(chan struct{})
	releaseResponse := sync.OnceFunc(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(arrived)
		<-release
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	}))
	defer func() { releaseResponse(); srv.Close() }()
	props := map[string]any{PropertySurface: SurfaceCLI}
	sender := &ClientGate{}
	sent := make(chan struct{})
	go func() {
		_, _ = sender.Post(t.Context(), srv.Client(), "endpoint-a", srv.URL, EventAppOpened, props, true)
		close(sent)
	}()
	<-arrived

	waiter := &ClientGate{}
	waiter.Load()
	require.False(t, waiter.ReadyEvent("endpoint-a", EventAppOpened, SurfaceCLI, ""))
	ready := make(chan bool, 1)
	go func() { ready <- waiter.AwaitProbe(t.Context(), "endpoint-a", EventAppOpened, SurfaceCLI) }()
	releaseResponse()
	<-sent
	assert.True(t, <-ready)
}

func TestClientGateAwaitProbeStopsAtExpiredLease(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	receipt := `{"version":1,"day":"2026-01-02","endpoints":{"endpoint-a":{"events":{"app_opened:cli:":{"attempts":1,"in_flight":true,"lease_until":"2026-01-02T00:00:01Z"}}}}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry-client.json"), []byte(receipt), 0o600))
	gate := &ClientGate{now: func() time.Time { return now }}
	gate.Load()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	// A sender that exited never settles its reservation; the wait ends with its lease.
	now = now.Add(2 * time.Second)
	assert.False(t, gate.AwaitProbe(ctx, "endpoint-a", EventAppOpened, SurfaceCLI))
	assert.NoError(t, ctx.Err())
}

func TestClientGateSettlesAgentCallReservedAfterMidnight(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		var nowMu sync.Mutex
		now := time.Date(2026, 1, 2, 23, 59, 59, 0, time.UTC)
		clock := func() time.Time {
			nowMu.Lock()
			defer nowMu.Unlock()
			return now
		}
		var requests atomic.Int32
		client := &http.Client{Transport: clientGateRoundTripper(func(req *http.Request) (*http.Response, error) {
			requests.Add(1)
			return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Request: req}, nil
		})}
		lock := flock.New(filepath.Join(os.Getenv("ROBOREV_DATA_DIR"), "telemetry-client.json.lock"))
		require.NoError(t, lock.Lock())
		gate := &ClientGate{now: clock}
		done := make(chan error, 1)
		go func() {
			_, err := gate.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
			done <- err
		}()
		// The call waits on the receipt lock across UTC midnight.
		synctest.Wait()
		nowMu.Lock()
		now = now.Add(time.Second)
		nowMu.Unlock()
		require.NoError(t, lock.Unlock())
		require.NoError(t, <-done)
		status, err := gate.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
		require.NoError(t, err)
		assert.Equal(t, posthog.StatusQueued, status)
		assert.Equal(t, int32(2), requests.Load())
	})
}

func TestClientGateKeepsQueuedCallWhenSettlementStalls(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		lock := flock.New(filepath.Join(os.Getenv("ROBOREV_DATA_DIR"), "telemetry-client.json.lock"))
		var mu sync.Mutex
		var calls []string
		arrived, release := make(chan struct{}), make(chan struct{})
		client := &http.Client{Transport: clientGateRoundTripper(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			calls = append(calls, req.URL.Query().Get("call"))
			first := len(calls) == 1
			mu.Unlock()
			if first {
				close(arrived)
				<-release
				// Another process holds the receipt lock while this response settles.
				if err := lock.Lock(); err != nil {
					return nil, err
				}
			}
			return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Request: req}, nil
		})}
		sender, other := &ClientGate{}, &ClientGate{}
		done := make(chan error, 1)
		go func() {
			_, err := sender.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
			done <- err
		}()
		<-arrived
		// The other process queues its call behind the request in flight.
		status, err := other.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
		require.NoError(t, err)
		assert.Equal(t, posthog.StatusSkipped, status)
		close(release)
		synctest.Wait()
		require.NoError(t, <-done)
		require.NoError(t, lock.Unlock())

		// Two more calls after the stalled settlement's lease and backoff.
		time.Sleep(NotificationTimeout + backoffDelay(1))
		for range 2 {
			_, err = sender.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
			require.NoError(t, err)
			time.Sleep(backoffDelay(2))
		}
		mu.Lock()
		defer mu.Unlock()
		// Four calls were made; each reached the daemon under its own ID, and a resent call repeats its ID.
		ids := map[string]bool{}
		for _, id := range calls {
			ids[id] = true
		}
		assert.Len(t, ids, 4)
	})
}

func TestClientGateCanceledLockWaitKeepsStorage(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		lock := flock.New(filepath.Join(os.Getenv("ROBOREV_DATA_DIR"), "telemetry-client.json.lock"))
		require.NoError(t, lock.Lock())
		var requests atomic.Int32
		client := &http.Client{Transport: clientGateRoundTripper(func(req *http.Request) (*http.Response, error) {
			requests.Add(1)
			return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Request: req}, nil
		})}
		props := map[string]any{PropertySurface: SurfaceTUI}
		gate := &ClientGate{}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan posthog.Status, 1)
		go func() {
			status, _ := gate.Post(ctx, client, "endpoint-a", "http://example.com", EventAppOpened, props, true)
			done <- status
		}()
		synctest.Wait()
		cancel()
		assert.Equal(t, posthog.StatusSkipped, <-done)
		assert.Zero(t, requests.Load())
		require.NoError(t, lock.Unlock())

		_, err := gate.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAppOpened, props, true)
		require.NoError(t, err)
		restarted := &ClientGate{}
		restarted.Load()
		assert.True(t, restarted.AwaitProbe(t.Context(), "endpoint-a", EventAppOpened, SurfaceTUI))
	})
}

func TestClientGateKeepsAgentBackoffWhenWritesFail(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory whose mode blocks file creation")
	}
	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "telemetry-client.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"day":"2026-01-02","endpoints":{}}`), 0o600))
	require.NoError(t, os.WriteFile(path+".lock", nil, 0o600))
	// The receipt stays readable, but no replacement file can be created.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	transport := &failedGateTransport{}
	client := &http.Client{Transport: transport}
	gate := &ClientGate{now: func() time.Time { return now }}
	gate.Load()
	for range 20 {
		for range 3 {
			_, _ = gate.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
		}
		now = now.Add(time.Minute)
	}
	// One request per backoff window, and no more than six attempts for the day.
	assert.Equal(t, int32(6), transport.requests.Load())
}

func TestClientGateQueuesAgentCallsDuringBackoff(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	var calls []string
	client := &http.Client{Transport: clientGateRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.URL.Query().Get("call"))
		if len(calls) == 1 {
			return nil, errors.New("daemon unavailable")
		}
		return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Request: req}, nil
	})}
	gate := &ClientGate{now: func() time.Time { return now }}
	post := func() {
		// Callers check readiness before scheduling a call, as the CLI and MCP do.
		if gate.ReadyEvent("endpoint-a", EventAgentActive, "", "") {
			_, _ = gate.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
		}
	}
	post()
	post() // during backoff: queued, not sent
	assert.Len(t, calls, 1)
	now = now.Add(backoffDelay(1))
	post()
	// The failed call is resent, then the call made during backoff, then this one.
	require.Len(t, calls, 4)
	assert.Equal(t, calls[0], calls[1])
	assert.Len(t, map[string]bool{calls[1]: true, calls[2]: true, calls[3]: true}, 3)
}

func TestClientGateSendsQueuedCallOnlyAfterItsIDIsSaved(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory whose mode blocks file creation")
	}
	dir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dir)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	var nowMu sync.Mutex
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	var mu sync.Mutex
	var calls []string
	arrived, release := make(chan struct{}), make(chan struct{})
	client := &http.Client{Transport: clientGateRoundTripper(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		id := req.URL.Query().Get("call")
		if id != "" {
			calls = append(calls, id)
		}
		first := len(calls) == 1 && id != ""
		mu.Unlock()
		if first {
			close(arrived)
			<-release
		}
		return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Request: req}, nil
	})}
	sender, queued, other := &ClientGate{now: clock}, &ClientGate{now: clock}, &ClientGate{now: clock}
	done := make(chan error, 1)
	go func() {
		_, err := sender.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
		done <- err
	}()
	<-arrived
	_, err := queued.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
	require.NoError(t, err)
	// The settlement that would name the queued call's ID cannot be saved.
	require.NoError(t, os.Chmod(dir, 0o500))
	close(release)
	require.NoError(t, <-done)
	require.NoError(t, os.Chmod(dir, 0o700))
	// Another process changes the shared file before the queued call settles.
	_, err = other.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAppOpened, map[string]any{PropertySurface: SurfaceCLI}, true)
	require.NoError(t, err)

	nowMu.Lock()
	now = now.Add(NotificationTimeout + backoffDelay(1))
	nowMu.Unlock()
	_, err = other.Post(t.Context(), client, "endpoint-a", "http://example.com", EventAgentActive, nil, false)
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	// Three agent calls were made; the daemon counts each distinct ID once.
	ids := map[string]bool{}
	for _, id := range calls {
		ids[id] = true
	}
	assert.Len(t, ids, 3)
}
