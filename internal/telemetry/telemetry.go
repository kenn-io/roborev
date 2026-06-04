package telemetry

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/posthog/posthog-go"

	"go.kenn.io/roborev/internal/storage"
)

const (
	EnabledEnv           = "ROBOREV_TELEMETRY_ENABLED"
	GenericEnabledEnv    = "TELEMETRY_ENABLED"
	installIDMetadataKey = "telemetry.install_id"
	postHogAPIKey        = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf"
	postHogEndpoint      = "https://us.i.posthog.com"

	EventDaemonStarted = "daemon_started"
	EventDaemonActive  = "daemon_active"
)

var ErrUnsupportedEvent = errors.New("unsupported telemetry event")

type propertyFilter func(any) (any, bool)

var allowedEvents = map[string]map[string]propertyFilter{
	EventDaemonStarted: {
		"repo_count":          safeTelemetryNumber,
		"review_count":        safeTelemetryNumber,
		"sync_enabled":        safeTelemetryBool,
		"ci_enabled":          safeTelemetryBool,
		"auto_design_enabled": safeTelemetryBool,
	},
	EventDaemonActive: {
		"repo_count":          safeTelemetryNumber,
		"review_count":        safeTelemetryNumber,
		"sync_enabled":        safeTelemetryBool,
		"ci_enabled":          safeTelemetryBool,
		"auto_design_enabled": safeTelemetryBool,
	},
}

type Client interface {
	Capture(event string, properties map[string]any) error
	Close() error
	Enabled() bool
}

type Reporter struct {
	client     enqueueCloser
	distinctID string
	version    string
	enabled    bool
}

type enqueueCloser interface {
	Enqueue(posthog.Message) error
	Close() error
}

type Options struct {
	Database *storage.DB
	Version  string
}

func EnabledFromEnv() bool {
	if strings.TrimSpace(os.Getenv(EnabledEnv)) == "0" {
		return false
	}
	return strings.TrimSpace(os.Getenv(GenericEnabledEnv)) != "0"
}

func EventAllowed(event string) bool {
	_, ok := allowedEvents[strings.TrimSpace(event)]
	return ok
}

func SanitizeProperties(event string, properties map[string]any) (map[string]any, error) {
	allowedProperties, ok := allowedEvents[strings.TrimSpace(event)]
	if !ok {
		return nil, ErrUnsupportedEvent
	}

	safeProperties := map[string]any{}
	for key, value := range properties {
		key = strings.TrimSpace(key)
		filter, ok := allowedProperties[key]
		if !ok {
			continue
		}
		if safeValue, ok := filter(value); ok {
			safeProperties[key] = safeValue
		}
	}
	safeProperties["$process_person_profile"] = false
	safeProperties["$geoip_disable"] = true
	safeProperties["application"] = "roborev"
	safeProperties["source"] = "daemon"
	safeProperties["goos"] = runtime.GOOS
	safeProperties["goarch"] = runtime.GOARCH
	return safeProperties, nil
}

func NewReporter(opts Options) (*Reporter, error) {
	if runningInTestProcess() {
		return DisabledReporter(), nil
	}
	if !EnabledFromEnv() {
		return DisabledReporter(), nil
	}
	if opts.Database == nil {
		return nil, errors.New("telemetry database is required")
	}

	distinctID, err := loadOrCreateInstallID(opts.Database)
	if err != nil {
		return nil, err
	}

	disableGeoIP := true
	client, err := posthog.NewWithConfig(postHogAPIKey, posthog.Config{
		Endpoint:     postHogEndpoint,
		DisableGeoIP: &disableGeoIP,
		DefaultEventProperties: posthog.Properties{
			"application":             "roborev",
			"source":                  "daemon",
			"version":                 opts.Version,
			"goos":                    runtime.GOOS,
			"goarch":                  runtime.GOARCH,
			"$process_person_profile": false,
			"$geoip_disable":          true,
		},
	})
	if err != nil {
		return nil, err
	}

	return &Reporter{
		client:     client,
		distinctID: distinctID,
		version:    opts.Version,
		enabled:    true,
	}, nil
}

func DisabledReporter() *Reporter {
	return &Reporter{}
}

func NewReporterOrDisabled(opts Options) *Reporter {
	reporter, err := NewReporter(opts)
	if err != nil {
		log.Printf("Warning: telemetry disabled: %v", err)
		return DisabledReporter()
	}
	return reporter
}

func (r *Reporter) Enabled() bool {
	return r != nil && r.enabled && r.client != nil
}

func (r *Reporter) Capture(event string, properties map[string]any) error {
	if !r.Enabled() {
		return nil
	}

	capture, err := r.captureMessage(event, properties, time.Now().UTC())
	if err != nil {
		return err
	}
	if runningInTestProcess() {
		return nil
	}

	return r.client.Enqueue(capture)
}

func (r *Reporter) captureMessage(event string, properties map[string]any, timestamp time.Time) (posthog.Capture, error) {
	event = strings.TrimSpace(event)
	if event == "" {
		return posthog.Capture{}, errors.New("telemetry event is required")
	}

	safeProperties, err := SanitizeProperties(event, properties)
	if err != nil {
		return posthog.Capture{}, err
	}

	safeProperties["version"] = r.version

	props := posthog.Properties{}
	maps.Copy(props, safeProperties)

	return posthog.Capture{
		DistinctId: r.distinctID,
		Event:      event,
		Timestamp:  timestamp,
		Properties: props,
	}, nil
}

func (r *Reporter) Close() error {
	if !r.Enabled() {
		return nil
	}
	return r.client.Close()
}

func safeTelemetryNumber(value any) (any, bool) {
	switch v := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return v, true
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, false
		}
		return v, true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		return v, true
	default:
		return nil, false
	}
}

func safeTelemetryBool(value any) (any, bool) {
	v, ok := value.(bool)
	return v, ok
}

func loadOrCreateInstallID(database *storage.DB) (string, error) {
	return database.GetOrCreateSyncStateValue(installIDMetadataKey, randomInstallID)
}

func randomInstallID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate telemetry install id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func runningInTestProcess() bool {
	return testing.Testing()
}
