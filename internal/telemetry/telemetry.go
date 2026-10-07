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
	// NotificationTimeout shares the CLI's existing one-second bound with MCP database work.
	NotificationTimeout  = time.Second
	EnabledEnv           = "ROBOREV_TELEMETRY_ENABLED"
	GenericEnabledEnv    = kittelemetry.GenericTelemetryEnabledEnv
	installIDMetadataKey = "telemetry.install_id"
	installedAtKey       = "telemetry.installed_at"
	postHogAPIKey        = "phc_AzHd9YvuHR7M5poKzC6eW654d3SgKyBdoQPuwkWhimUf"

	EventDaemonStarted      = "daemon_started"
	EventDaemonActive       = "daemon_active"
	EventAppOpened          = "app_opened"
	EventSessionEnded       = "session_ended"
	EventScreenViewed       = "screen_viewed"
	EventAgentActive        = "agent_active"
	EventAgentCallCount     = "agent_call_count"
	PropertyCallCountBucket = "call_count_bucket"

	PropertyScreen = "screen"
	// PropertySurface names the interface that sent a usage event.
	PropertySurface        = "surface"
	PropertyDurationBucket = "duration_bucket"
	DurationUnder1m        = "under_1m"
	Duration1To5m          = "1_to_5m"
	Duration5To30m         = "5_to_30m"
	DurationOver30m        = "over_30m"
	SurfaceWeb             = "web"
	SurfaceTUI             = "tui"
	SurfaceCLI             = "cli"
)

var ErrUnsupportedEvent = kittelemetry.ErrUnsupportedTelemetryEvent

type Client = kittelemetry.PostHogClient

type Reporter = kittelemetry.PostHogReporter

type Options struct {
	Database *storage.DB
	Version  string
	Endpoint string
}

func EnabledFromEnv() bool {
	return kittelemetry.PostHogTelemetryEnabledFromEnv("ROBOREV")
}

func NewReporter(opts Options) (*Reporter, error) {
	if !EnabledFromEnv() {
		// An opted-out kit reporter keeps the allowlist, so the capture route can still reject unknown events.
		return kittelemetry.NewPostHogReporter(kittelemetry.PostHogOptions{EnvPrefix: "ROBOREV"}, allowedEventOptions()...)
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
		Endpoint:    opts.Endpoint,
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
		kittelemetry.WithAllowedEvent(EventAgentActive,
			kittelemetry.AllowTelemetryProperty(PropertyCallCountBucket, kittelemetry.AllowTelemetryStringValues("1-10"))),
		kittelemetry.WithAllowedEvent(EventAgentCallCount,
			kittelemetry.AllowTelemetryProperty(PropertyCallCountBucket, kittelemetry.AllowTelemetryStringValues("11-100", "over-100"))),
		kittelemetry.WithAllowedEvent(EventDaemonStarted, daemonProperties...),
		kittelemetry.WithAllowedEvent(EventDaemonActive, daemonProperties...),
		kittelemetry.WithAllowedEvent(EventScreenViewed,
			kittelemetry.AllowTelemetryProperty(PropertyScreen, kittelemetry.AllowTelemetryStringValues(
				"reviews", "analytics", "queue", "review", "prompt", "filter", "comment", "commit-msg", "help", "log", "tasks", "worktree-confirm", "patch", "column-options", "release-notes", "rerun-agent")),
			kittelemetry.AllowTelemetryProperty(PropertySurface, kittelemetry.AllowTelemetryStringValues(SurfaceWeb, SurfaceTUI))),
		kittelemetry.WithAllowedEvent(EventAppOpened,
			kittelemetry.AllowTelemetryProperty(PropertySurface, kittelemetry.AllowTelemetryStringValues(SurfaceWeb, SurfaceTUI, SurfaceCLI))),
		kittelemetry.WithAllowedEvent(EventSessionEnded,
			kittelemetry.AllowTelemetryProperty(PropertySurface, kittelemetry.AllowTelemetryStringValues(SurfaceWeb, SurfaceTUI)),
			kittelemetry.AllowTelemetryProperty(PropertyDurationBucket, kittelemetry.AllowTelemetryStringValues(DurationUnder1m, Duration1To5m, Duration5To30m, DurationOver30m))),
	}
}

// DurationBucket groups elapsed time without sending an exact duration.
func DurationBucket(elapsed time.Duration) string {
	switch {
	case elapsed < time.Minute:
		return DurationUnder1m
	case elapsed < 5*time.Minute:
		return Duration1To5m
	case elapsed <= 30*time.Minute:
		return Duration5To30m
	default:
		return DurationOver30m
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
