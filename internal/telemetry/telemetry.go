package telemetry

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/roborev/internal/storage"
)

const (
	// NotificationTimeout shares the CLI's existing one-second bound with MCP database work.
	NotificationTimeout  = time.Second
	EnabledEnv           = "ROBOREV_TELEMETRY_ENABLED"
	GenericEnabledEnv    = posthog.GenericEnabledEnv
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

var ErrUnsupportedEvent = posthog.ErrUnsupportedEvent

type Client = posthog.Client

type Reporter = posthog.Reporter

type Options struct {
	Database *storage.DB
	Version  string
	Endpoint string
}

func EnabledFromEnv() bool {
	return posthog.EnabledFromEnv("ROBOREV")
}

func NewReporter(opts Options) (*Reporter, error) {
	if !EnabledFromEnv() {
		// An opted-out kit reporter keeps the allowlist, so the capture route can still reject unknown events.
		return posthog.NewReporter(posthog.Options{EnvPrefix: "ROBOREV"}, allowedEventOptions()...)
	}
	if opts.Database == nil {
		return nil, errors.New("telemetry database is required")
	}

	distinctID, installedAt, err := loadOrCreateInstall(opts.Database)
	if err != nil {
		return nil, err
	}

	return posthog.NewReporter(posthog.Options{
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
	return posthog.DisabledReporter()
}

func NewReporterOrDisabled(opts Options) *Reporter {
	reporter, err := NewReporter(opts)
	if err != nil {
		log.Printf("Warning: telemetry disabled: %v", err)
		return DisabledReporter()
	}
	return reporter
}

func allowedEventOptions() []posthog.Option {
	daemonProperties := []posthog.AllowedProperty{
		posthog.AllowProperty("repo_count", posthog.AllowNumber),
		posthog.AllowProperty("review_count", posthog.AllowNumber),
		posthog.AllowProperty("sync_enabled", posthog.AllowBool),
		posthog.AllowProperty("ci_enabled", posthog.AllowBool),
		posthog.AllowProperty("auto_design_enabled", posthog.AllowBool),
	}

	return []posthog.Option{
		posthog.WithAllowedEvent(EventAgentActive,
			posthog.AllowProperty(PropertyCallCountBucket, posthog.AllowStringValues("1-10"))),
		posthog.WithAllowedEvent(EventAgentCallCount,
			posthog.AllowProperty(PropertyCallCountBucket, posthog.AllowStringValues("11-100", "over-100"))),
		posthog.WithAllowedEvent(EventDaemonStarted, daemonProperties...),
		posthog.WithAllowedEvent(EventDaemonActive, daemonProperties...),
		posthog.WithAllowedEvent(EventScreenViewed,
			posthog.AllowProperty(PropertyScreen, posthog.AllowStringValues(
				"reviews", "analytics", "queue", "review", "prompt", "filter", "comment", "commit-msg", "help", "log", "tasks", "worktree-confirm", "patch", "column-options", "release-notes", "rerun-agent")),
			posthog.AllowProperty(PropertySurface, posthog.AllowStringValues(SurfaceWeb, SurfaceTUI))),
		posthog.WithAllowedEvent(EventAppOpened,
			posthog.AllowProperty(PropertySurface, posthog.AllowStringValues(SurfaceWeb, SurfaceTUI, SurfaceCLI))),
		posthog.WithAllowedEvent(EventSessionEnded,
			posthog.AllowProperty(PropertySurface, posthog.AllowStringValues(SurfaceWeb, SurfaceTUI)),
			posthog.AllowProperty(PropertyDurationBucket, posthog.AllowStringValues(DurationUnder1m, Duration1To5m, Duration5To30m, DurationOver30m))),
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
