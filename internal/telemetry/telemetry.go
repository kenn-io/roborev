package telemetry

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"

	kittelemetry "go.kenn.io/kit/telemetry"

	"go.kenn.io/roborev/internal/storage"
)

const (
	EnabledEnv           = "ROBOREV_TELEMETRY_ENABLED"
	GenericEnabledEnv    = kittelemetry.GenericTelemetryEnabledEnv
	installIDMetadataKey = "telemetry.install_id"
	postHogAPIKey        = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf"

	EventDaemonStarted = "daemon_started"
	EventDaemonActive  = "daemon_active"
)

var ErrUnsupportedEvent = kittelemetry.ErrUnsupportedTelemetryEvent

type Client = kittelemetry.PostHogClient

type Reporter = kittelemetry.PostHogReporter

type Options struct {
	Database *storage.DB
	Version  string
}

func EnabledFromEnv() bool {
	return kittelemetry.PostHogTelemetryEnabledFromEnv("ROBOREV")
}

func NewReporter(opts Options) (*Reporter, error) {
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

	return kittelemetry.NewPostHogReporter(kittelemetry.PostHogOptions{
		APIKey:      postHogAPIKey,
		Application: "roborev",
		EnvPrefix:   "ROBOREV",
		DistinctID:  distinctID,
		Version:     opts.Version,
		Source:      "daemon",
	}, allowedEventOptions()...)
}

func DisabledReporter() *Reporter {
	return kittelemetry.DisabledPostHogReporter()
}

func NewReporterOrDisabled(opts Options) *Reporter {
	reporter, err := NewReporter(opts)
	if err != nil {
		log.Printf("Warning: telemetry disabled: %v", err)
		return DisabledReporter()
	}
	return reporter
}

func allowedEventOptions() []kittelemetry.PostHogOption {
	daemonProperties := []kittelemetry.AllowedTelemetryProperty{
		kittelemetry.AllowTelemetryProperty("repo_count", kittelemetry.AllowTelemetryNumber),
		kittelemetry.AllowTelemetryProperty("review_count", kittelemetry.AllowTelemetryNumber),
		kittelemetry.AllowTelemetryProperty("sync_enabled", kittelemetry.AllowTelemetryBool),
		kittelemetry.AllowTelemetryProperty("ci_enabled", kittelemetry.AllowTelemetryBool),
		kittelemetry.AllowTelemetryProperty("auto_design_enabled", kittelemetry.AllowTelemetryBool),
	}

	return []kittelemetry.PostHogOption{
		kittelemetry.WithAllowedEvent(EventDaemonStarted, daemonProperties...),
		kittelemetry.WithAllowedEvent(EventDaemonActive, daemonProperties...),
	}
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
