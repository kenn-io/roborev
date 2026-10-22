package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/telemetry"
)

func TestAgentActivityBucketsRestartAndUTC(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		name := "sequential"
		if concurrent {
			name = "concurrent"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			server, db, _ := newTestServer(t)
			db.SetMaxOpenConns(1)
			_, err := db.Exec(`PRAGMA synchronous = OFF`)
			require.NoError(t, err)
			client := &fakeTelemetryClient{enabled: true}
			server.SetTelemetry(client)
			now := time.Date(2026, 1, 2, 18, 59, 0, 0, time.FixedZone("offset", -5*60*60))
			server.agentActivityNow = func() time.Time { return now }
			var wg sync.WaitGroup
			for call := 1; call <= 101; call++ {
				if concurrent {
					wg.Go(func() { server.recordAgentCall(t.Context()) })
					continue
				}
				server.recordAgentCall(t.Context())
				if call == 1 || call == 2 || call == 10 {
					assert.Len(client.events, 1, "call %d", call)
				}
				if call == 11 || call == 100 {
					assert.Len(client.events, 2, "call %d", call)
				}
				if call == 10 {
					// A new server reads the same durable state after restart.
					server = &Server{db: db, telemetry: client, agentActivityGate: make(chan struct{}, 1), agentActivityNow: server.agentActivityNow}
				}
			}
			wg.Wait()
			assert.Equal([]string{telemetry.EventAgentActive, telemetry.EventAgentCallCount, telemetry.EventAgentCallCount}, client.events)
			assert.Equal([]map[string]any{
				{telemetry.PropertyCallCountBucket: "1-10"},
				{telemetry.PropertyCallCountBucket: "11-100"},
				{telemetry.PropertyCallCountBucket: "over-100"},
			}, client.properties)
			if !concurrent {
				now = now.Add(time.Minute)
				server.recordAgentCall(t.Context())
				assert.Len(client.events, 4)
				assert.Equal(telemetry.EventAgentActive, client.events[3])
			}
		})
	}
}

type failingAgentTelemetry struct{ fakeTelemetryClient }

func (f *failingAgentTelemetry) Capture(event string, props map[string]any) error {
	_ = f.fakeTelemetryClient.Capture(event, props)
	return errors.New("delivery failed")
}

func TestAgentActivityRoutesRejectBrowserCapture(t *testing.T) {
	server, _, _ := newTestServer(t)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)
	for _, tc := range []struct {
		contentType string
		origin      string
		status      int
	}{
		{"", "", http.StatusUnsupportedMediaType},
		{"text/plain", "", http.StatusUnsupportedMediaType},
		{"application/x-www-form-urlencoded", "", http.StatusUnsupportedMediaType},
		{"application/json", "https://example.com", http.StatusForbidden},
		{"application/json", "null", http.StatusForbidden},
		{"application/json; charset=utf-8", "", http.StatusAccepted},
	} {
		req := httptest.NewRequest(http.MethodPost, TelemetryAgentCallPath, nil)
		req.Header.Set("Content-Type", tc.contentType)
		req.Header.Set("Origin", tc.origin)
		recorder := serveTelemetryCapture(server.httpServer.Handler, req)
		assert.Equal(t, tc.status, recorder.Code)
		if tc.status == http.StatusAccepted {
			repeated := serveTelemetryCapture(server.httpServer.Handler, req.Clone(t.Context()))
			assert.Equal(t, tc.status, repeated.Code)
			assert.Equal(t, []string{telemetry.EventAgentActive}, client.events)
		} else {
			assert.Empty(t, client.events)
		}
	}
}

func TestAgentActivityRouteReceipts(t *testing.T) {
	server, _, _ := newTestServer(t)
	client := &fakeTelemetryClient{}
	server.SetTelemetry(client)
	t.Run("opted out", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, TelemetryAgentCallPath, nil)
		req.Header.Set("Content-Type", "application/json")
		recorder := serveTelemetryCapture(server.httpServer.Handler, req)
		assert.Equal(t, http.StatusAccepted, recorder.Code)
		assert.JSONEq(t, `{"status":"disabled"}`, recorder.Body.String())
		assert.Empty(t, client.events)
	})
	client.enabled = true
	for _, status := range []string{"queued", "skipped"} {
		t.Run(status, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, TelemetryAgentCallPath, nil)
			req.Header.Set("Content-Type", "application/json")
			recorder := serveTelemetryCapture(server.httpServer.Handler, req)
			assert.Equal(t, http.StatusAccepted, recorder.Code)
			assert.JSONEq(t, `{"status":"`+status+`"}`, recorder.Body.String())
			assert.Equal(t, []string{telemetry.EventAgentActive}, client.events)
		})
	}

	for _, failure := range []string{"storage", "capture"} {
		t.Run(failure, func(t *testing.T) {
			server, db, _ := newTestServer(t)
			server.SetTelemetry(&failingAgentTelemetry{fakeTelemetryClient{enabled: true}})
			if failure == "storage" {
				require.NoError(t, db.Close())
			}
			req := httptest.NewRequest(http.MethodPost, TelemetryAgentCallPath, nil)
			req.Header.Set("Content-Type", "application/json")
			recorder := serveTelemetryCapture(server.httpServer.Handler, req)
			assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		})
	}
}

func TestAgentActivityCanceledWhileWaiting(t *testing.T) {
	server, _, _ := newTestServer(t)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)
	synctest.Test(t, func(t *testing.T) {
		server.agentActivityGate = make(chan struct{}, 1)
		server.agentActivityGate <- struct{}{}
		go server.recordAgentCall(t.Context())
		synctest.Wait()
		time.Sleep(telemetry.NotificationTimeout)
		synctest.Wait()
		<-server.agentActivityGate
		assert.Empty(t, client.events)
		server.recordAgentCall(t.Context())
		assert.Equal(t, []string{telemetry.EventAgentActive}, client.events)
	})
}

func TestAgentActivityRouteCountsAfterClientDisconnect(t *testing.T) {
	server, _, _ := newTestServer(t)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)
	// CLI clients stop waiting shortly after exit, so the call arrives with the connection already closed.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, TelemetryAgentCallPath, nil)
	req.Header.Set("Content-Type", "application/json")
	assert.Equal(t, http.StatusAccepted, serveTelemetryCapture(server.httpServer.Handler, req).Code)
	assert.Equal(t, []string{telemetry.EventAgentActive}, client.events)
}
