package daemon

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/telemetry"
)

// sizeHiddenReader hides the body's concrete type so the request carries no known length.
type sizeHiddenReader struct{ io.Reader }

func paddedTelemetryBody(t *testing.T, size int) []byte {
	t.Helper()
	prefix := `{"event":"app_opened","properties":{"pad":"`
	suffix := `"}}`
	padding := size - len(prefix) - len(suffix)
	require.Positive(t, padding)
	return []byte(prefix + strings.Repeat("x", padding) + suffix)
}

func trailingPaddedTelemetryBody(size int) []byte {
	event := `{"event":"app_opened"}`
	return []byte(event + strings.Repeat(" ", size-len(event)))
}

func serveTelemetryCapture(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func newTelemetryCaptureRequest(method string, body []byte) *http.Request {
	request := httptest.NewRequest(method, TelemetryEventsPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestTelemetryCaptureRouteOnCoreMux(t *testing.T) {
	server, _, _ := newTestServer(t)
	reporter, err := telemetry.NewReporter(telemetry.Options{})
	require.NoError(t, err)
	server.SetTelemetry(reporter)
	handler := server.httpServer.Handler

	unknownLength := newTelemetryCaptureRequest(http.MethodPost, nil)
	unknownLength.Body = io.NopCloser(sizeHiddenReader{bytes.NewReader(trailingPaddedTelemetryBody(65537))})
	unknownLength.ContentLength = -1

	tests := []struct {
		name       string
		request    *http.Request
		wantStatus int
		wantBody   string
	}{
		{
			name:       "allowed event",
			request:    newTelemetryCaptureRequest(http.MethodPost, []byte(`{"event":"app_opened"}`)),
			wantStatus: http.StatusAccepted,
			wantBody:   `{"status":"disabled"}`,
		},
		{
			name:       "tui body with surface",
			request:    newTelemetryCaptureRequest(http.MethodPost, []byte(`{"event":"app_opened","properties":{"surface":"tui"}}`)),
			wantStatus: http.StatusAccepted,
			wantBody:   `{"status":"disabled"}`,
		},
		{
			name:       "unknown event",
			request:    newTelemetryCaptureRequest(http.MethodPost, []byte(`{"event":"search_run"}`)),
			wantStatus: http.StatusBadRequest,
			wantBody:   telemetry.ErrUnsupportedEvent.Error(),
		},
		{
			name:       "wrong case",
			request:    newTelemetryCaptureRequest(http.MethodPost, []byte(`{"event":"App_Opened"}`)),
			wantStatus: http.StatusBadRequest,
			wantBody:   telemetry.ErrUnsupportedEvent.Error(),
		},
		{
			name:       "body at the 65536 byte cap",
			request:    newTelemetryCaptureRequest(http.MethodPost, paddedTelemetryBody(t, 65536)),
			wantStatus: http.StatusAccepted,
			wantBody:   `{"status":"disabled"}`,
		},
		{
			name:       "body one byte over the cap",
			request:    newTelemetryCaptureRequest(http.MethodPost, paddedTelemetryBody(t, 65537)),
			wantStatus: http.StatusRequestEntityTooLarge,
			wantBody:   "too large",
		},
		{
			name:       "valid event with trailing padding over the cap",
			request:    newTelemetryCaptureRequest(http.MethodPost, trailingPaddedTelemetryBody(65537)),
			wantStatus: http.StatusRequestEntityTooLarge,
			wantBody:   "too large",
		},
		{
			name:       "trailing padding over the cap with unknown length",
			request:    unknownLength,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantBody:   "too large",
		},
		{
			name:       "get",
			request:    newTelemetryCaptureRequest(http.MethodGet, nil),
			wantStatus: http.StatusMethodNotAllowed,
			wantBody:   "Method Not Allowed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := serveTelemetryCapture(handler, tt.request)
			assert.Equal(t, tt.wantStatus, recorder.Code)
			assert.Contains(t, recorder.Body.String(), tt.wantBody)
		})
	}
}

func TestTelemetryCaptureRouteWithoutReporterAdmitsNothing(t *testing.T) {
	assert := assert.New(t)

	server, _, _ := newTestServer(t)
	handler := server.httpServer.Handler

	recorder := serveTelemetryCapture(handler, newTelemetryCaptureRequest(http.MethodPost, []byte(`{"event":"app_opened"}`)))
	assert.Equal(http.StatusBadRequest, recorder.Code)

	fake := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(fake)
	recorder = serveTelemetryCapture(handler, newTelemetryCaptureRequest(http.MethodPost, []byte(`{"event":"app_opened"}`)))
	assert.Equal(http.StatusBadRequest, recorder.Code)
	assert.Empty(fake.events)
}

func TestBrowserHandlerTelemetryCaptureRoute(t *testing.T) {
	server, _, _ := newTestServer(t)
	reporter, err := telemetry.NewReporter(telemetry.Options{})
	require.NoError(t, err)
	server.SetTelemetry(reporter)
	handler, sessions := newBrowserHandlerFixtureWithCore(t, testBrowserAuthToken, server.httpServer.Handler)
	credentials, err := sessions.Login(testBrowserAuthToken)
	require.NoError(t, err)

	appOpened := map[string]string{"event": "app_opened"}
	withSession := func(request *http.Request) *http.Request {
		request.AddCookie(sessions.Cookie(credentials.Ambient))
		request.Header.Set(WebSessionHeader, credentials.Tab)
		return request
	}
	withCredentials := func(request *http.Request) *http.Request {
		withSession(request).Header.Set(WebCSRFHeader, credentials.CSRF)
		return request
	}
	foreignOrigin := withCredentials(browserRequest(http.MethodPost, TelemetryEventsPath, appOpened))
	foreignOrigin.Header.Set("Origin", "http://evil.example")

	tests := []struct {
		name       string
		request    *http.Request
		wantStatus int
		wantBody   string
	}{
		{
			name:       "no session",
			request:    browserRequest(http.MethodPost, TelemetryEventsPath, appOpened),
			wantStatus: http.StatusUnauthorized,
			wantBody:   "web_session_required",
		},
		{
			name:       "session without csrf",
			request:    withSession(browserRequest(http.MethodPost, TelemetryEventsPath, appOpened)),
			wantStatus: http.StatusForbidden,
			wantBody:   "csrf_invalid",
		},
		{
			name:       "foreign origin",
			request:    foreignOrigin,
			wantStatus: http.StatusForbidden,
			wantBody:   "invalid_origin",
		},
		{
			name:       "full credentials",
			request:    withCredentials(browserRequest(http.MethodPost, TelemetryEventsPath, appOpened)),
			wantStatus: http.StatusAccepted,
			wantBody:   `{"status":"disabled"}`,
		},
		{
			name:       "unknown event",
			request:    withCredentials(browserRequest(http.MethodPost, TelemetryEventsPath, map[string]string{"event": "search_run"})),
			wantStatus: http.StatusBadRequest,
			wantBody:   telemetry.ErrUnsupportedEvent.Error(),
		},
		{
			name:       "get is not listed",
			request:    withCredentials(browserRequest(http.MethodGet, TelemetryEventsPath, nil)),
			wantStatus: http.StatusNotFound,
			wantBody:   "404 page not found",
		},
		{
			name:       "parent path is not listed",
			request:    withCredentials(browserRequest(http.MethodPost, "/api/telemetry", appOpened)),
			wantStatus: http.StatusNotFound,
			wantBody:   "404 page not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := serveTelemetryCapture(handler, tt.request)
			assert.Equal(t, tt.wantStatus, recorder.Code)
			assert.Contains(t, recorder.Body.String(), tt.wantBody)
		})
	}
}
