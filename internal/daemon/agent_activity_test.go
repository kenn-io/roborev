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

func TestAgentActivityOptOutAndDeliveryFailure(t *testing.T) {
	server, _, _ := newTestServer(t)
	client := &failingAgentTelemetry{}
	server.SetTelemetry(client)
	server.recordAgentCall(context.Background())
	client.enabled = true
	for range 2 {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, TelemetryAgentCallPath, nil)
		req.Header.Set("Content-Type", "application/json")
		server.httpServer.Handler.ServeHTTP(w, req)
		assert.Equal(t, http.StatusAccepted, w.Code)
	}
	assert.Equal(t, []string{telemetry.EventAgentActive}, client.events)
}

func TestAgentActivityRoutesRejectBrowserCapture(t *testing.T) {
	server, _, _ := newTestServer(t)
	reporter, err := telemetry.NewReporter(telemetry.Options{})
	require.NoError(t, err)
	server.SetTelemetry(reporter)
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
		assert.Equal(t, tc.status, serveTelemetryCapture(server.httpServer.Handler, req).Code)
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
