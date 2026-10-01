package telemetry

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	kittelemetry "go.kenn.io/kit/telemetry"

	"go.kenn.io/roborev/internal/storage"
)

const (
	EnabledEnv           = "ROBOREV_TELEMETRY_ENABLED"
	GenericEnabledEnv    = kittelemetry.GenericTelemetryEnabledEnv
	installIDMetadataKey = "telemetry.install_id"
	installedAtKey       = "telemetry.installed_at"
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

	distinctID, installedAt, err := loadOrCreateInstall(opts.Database)
	if err != nil {
		return nil, err
	}

	return kittelemetry.NewPostHogReporter(kittelemetry.PostHogOptions{
		APIKey:      postHogAPIKey,
		Application: "roborev",
		EnvPrefix:   "ROBOREV",
		DistinctID:  distinctID,
		InstalledAt: installedAt,
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

// loadOrCreateInstall returns the install ID and when it was created. An ID
// created before roborev recorded that time returns a zero time, so its
// events carry no install age.
func loadOrCreateInstall(database *storage.DB) (string, time.Time, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	id, err := database.GetOrCreateSyncStateValueWith(installIDMetadataKey, randomInstallID,
		map[string]string{installedAtKey: now})
	if err != nil {
		return "", time.Time{}, err
	}

	stored, err := database.GetSyncState(installedAtKey)
	if err != nil {
		return "", time.Time{}, err
	}
	if strings.TrimSpace(stored) == "" {
		return id, time.Time{}, nil
	}
	installedAt, err := time.Parse(time.RFC3339Nano, stored)
	if err != nil {
		log.Printf("Warning: telemetry install time unreadable, sending events without an install age: %v", err)
		return id, time.Time{}, nil
	}
	return id, installedAt, nil
}

func randomInstallID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate telemetry install id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
