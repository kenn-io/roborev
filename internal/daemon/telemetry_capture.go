package daemon

import (
	"bytes"
	"io"
	"net/http"

	"go.kenn.io/roborev/internal/telemetry"
)

const (
	telemetryEventsPath = "/api/telemetry/events"
	// Same cap the daemon's typed JSON POST routes get by default.
	telemetryEventMaxBodyBytes = 1 << 20
)

func (s *Server) registerTelemetryCaptureRoute(mux *http.ServeMux) {
	mux.HandleFunc(http.MethodPost+" "+telemetryEventsPath, s.handleTelemetryEvent)
}

// handleTelemetryEvent reads the reporter per request because SetTelemetry runs after route registration.
func (s *Server) handleTelemetryEvent(w http.ResponseWriter, r *http.Request) {
	// Read the whole body first: kit decodes one JSON value and would never see trailing bytes past the cap.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, telemetryEventMaxBodyBytes))
	if err != nil {
		http.Error(w, "telemetry request too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	reporter, _ := s.telemetry.(*telemetry.Reporter)
	telemetry.NewCaptureHandler(reporter).ServeHTTP(w, r)
}
