package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v7"
	"github.com/spf13/cobra"
	"go.kenn.io/kit/secretref"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/telemetry"
	"go.kenn.io/roborev/internal/version"
)

var (
	daemonEnsure   = ensureDaemon
	daemonStop     = stopDaemon
	daemonDiscover = uiRuntimeInfo
)

var (
	openDaemonSearchIndex  = searchindex.Open
	closeDaemonSearchIndex = func(index *searchindex.Index) error { return index.Close() }
)

type daemonSearch struct {
	path       string
	index      *searchindex.Index
	service    *searchindex.Service
	reconciler *searchindex.Reconciler
	closeOnce  sync.Once
	closeErr   error
}

func newDaemonSearch(
	ctx context.Context, db *storage.DB, dbPath string, cfg *config.Config,
) (_ *daemonSearch, err error) {
	path := searchindex.PathFor(dbPath)
	index, err := openDaemonSearchIndex(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open search sidecar: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, closeDaemonSearchIndex(index))
		}
	}()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate daemon config: %w", err)
	}

	var embedder searchindex.Embedder
	reconcilerConfig := searchindex.ReconcilerConfig{}
	if embeddings := cfg.Search.Embeddings; embeddings != nil && embeddings.Enabled() {
		// Config loading already rejected a malformed key, so an error here
		// means the configured source has no key. Search then stays
		// lexical-only and reports why; an endpoint with no key configured is
		// called without authentication.
		credential, credentialErr := embeddings.ResolveAPIKey()
		client, err := searchindex.NewEmbeddings(*embeddings, credential.Value, searchdoc.RecipeVersion)
		if err != nil {
			return nil, err
		}
		reconcilerConfig.CredentialSource = embeddingKeySource(embeddings.APIKey)
		if credentialErr != nil {
			reconcilerConfig.CredentialReason = "no embedding API key (" +
				strings.TrimPrefix(credentialErr.Error(), "embed api_key: secretref: ") + ")"
		} else {
			embedder = client
		}
	}

	reconciler := searchindex.NewReconciler(db, index, embedder, reconcilerConfig)
	service := searchindex.NewService(db, index, embedder, reconciler)
	return &daemonSearch{
		path: path, index: index, service: service, reconciler: reconciler,
	}, nil
}

// embeddingKeySource names where the embedding key comes from, without the
// key itself: "inline", "env:NAME", "file:PATH", or "" when none is set.
func embeddingKeySource(ref secretref.Ref) string {
	switch {
	case ref.Env != "":
		return "env:" + strings.TrimSpace(ref.Env)
	case ref.File != "":
		return "file:" + strings.TrimSpace(ref.File)
	case ref.Value != "":
		return "inline"
	default:
		return ""
	}
}

func (s *daemonSearch) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = closeDaemonSearchIndex(s.index)
	})
	return s.closeErr
}

type daemonLifecycle interface {
	Start(context.Context) error
	Stop() error
}

type searchCloser interface {
	Close() error
}

func runDaemonWithSearch(
	ctx context.Context, server daemonLifecycle, search searchCloser,
) error {
	startErr := server.Start(ctx)
	stopErr := server.Stop()
	closeErr := search.Close()
	return errors.Join(startErr, stopErr, closeErr)
}

func daemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the roborev daemon",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "Start the daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := daemonEnsure(); err != nil {
				return err
			}
			writeDaemonLifecycleResult("Daemon started")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := daemonStop(); errors.Is(err, ErrDaemonNotRunning) {
				fmt.Println("Daemon was not running")
				return nil
			} else if err != nil {
				return err
			}
			fmt.Println("Daemon stopped")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			wasRunning := true
			if err := daemonStop(); errors.Is(err, ErrDaemonNotRunning) {
				wasRunning = false
			} else if err != nil {
				return err
			}
			if err := daemonEnsure(); err != nil {
				return err
			}
			if wasRunning {
				writeDaemonLifecycleResult("Daemon restarted")
			} else {
				writeDaemonLifecycleResult("Daemon started (was not running)")
			}
			return nil
		},
	})

	cmd.AddCommand(statusCmd())
	cmd.AddCommand(daemonRunCmd())

	return cmd
}

func writeDaemonLifecycleResult(message string) {
	fmt.Println(message)
	fmt.Printf("Web UI: %s\n", displayWebUI(discoverWebUI(daemonDiscover)))
}

// webUIStatus describes the daemon's browser UI: either a reachable URL, or
// the daemon-published reason the listener is not running.
type webUIStatus struct {
	url            string
	disabledReason string
}

func discoverWebUI(discover func() (*daemon.RuntimeInfo, error)) webUIStatus {
	runtimeInfo, err := discover()
	if err != nil || runtimeInfo == nil {
		return webUIStatus{}
	}
	if runtimeInfo.WebOrigin == "" {
		return webUIStatus{disabledReason: runtimeInfo.WebDisabledReason}
	}
	webURL, err := browserRootURL(runtimeInfo.WebOrigin, runtimeInfo.WebBasePath)
	if err != nil {
		return webUIStatus{}
	}
	return webUIStatus{url: webURL}
}

func displayWebUI(status webUIStatus) string {
	if status.url != "" {
		return status.url
	}
	switch status.disabledReason {
	case daemon.WebDisabledReasonMissingAssets:
		return "disabled (this build has no embedded web assets; reinstall from an official release)"
	case daemon.WebDisabledReasonConfig:
		return "disabled ([web] enabled = false)"
	}
	return "unavailable"
}

// daemonRunCmd runs the daemon in the foreground (used by "daemon start" internally)
func daemonRunCmd() *cobra.Command {
	var (
		dbPath       string
		configPath   string
		addr         string
		workers      int
		webDevOrigin string
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the daemon in foreground",
		Long:  "Run the daemon in the foreground. Usually invoked by 'daemon start' in the background.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Defense-in-depth: clear git repo-context env vars that hooks may set.
			// The spawn sites (startDaemon, upgrade) filter these out, but
			// clear them here too in case the daemon is started manually.
			for _, e := range os.Environ() {
				if isGitRepoEnvKey(e) {
					key, _, _ := strings.Cut(e, "=")
					os.Unsetenv(key)
				}
			}

			log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
			log.Printf("Starting roborev daemon (version %s)...", version.Version)

			// Silently clean up old roborevd binary if it exists (consolidated into roborev)
			if exePath, err := os.Executable(); err == nil {
				oldDaemonPath := filepath.Join(filepath.Dir(exePath), "roborevd")
				if runtime.GOOS == "windows" {
					oldDaemonPath += ".exe"
				}
				os.Remove(oldDaemonPath) // Ignore errors silently
			}

			// Load configuration from specified path
			cfg, err := config.LoadGlobalFrom(configPath)
			if err != nil {
				return fmt.Errorf("failed to load config from %s: %w", configPath, err)
			}

			// Fail fast on invalid auto-design heuristic config. An
			// unchecked typo in trigger_paths/skip_paths or in one of
			// the message-pattern regexes would otherwise surface only
			// at dispatch time as a "heuristic error" skipped row,
			// silently suppressing every automatic design review.
			if cfg.AutoDesignReview.Enabled || cfg.AutoDesignReview.HookEnabled {
				globalHeuristics := config.ResolveGlobalAutoDesignHeuristics(cfg)
				if err := globalHeuristics.Validate(); err != nil {
					return fmt.Errorf("invalid [auto_design_review] config in %s: %w", configPath, err)
				}
			}

			// Apply flag overrides
			if addr != "" {
				cfg.ServerAddr = addr
			}
			if workers > 0 {
				cfg.MaxWorkers = workers
			}

			// Open database
			db, err := storage.Open(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer db.Close()
			log.Printf("Database: %s", dbPath)

			search, err := newDaemonSearch(cmd.Context(), db, dbPath, cfg)
			if err != nil {
				return err
			}
			log.Printf("Search database: %s", search.path)

			telemetryReporter := telemetry.NewReporterOrDisabled(telemetry.Options{
				Database: db,
				Version:  version.Version,
			})
			defer func() {
				if err := telemetryReporter.Close(); err != nil {
					log.Printf("Warning: close telemetry: %v", err)
				}
			}()

			// Start sync worker if enabled
			var syncWorker *storage.SyncWorker
			if cfg.Sync.Enabled {
				// Validate sync config
				warnings := cfg.Sync.Validate()
				for _, w := range warnings {
					log.Printf("Sync warning: %s", w)
				}

				// Backfill machine IDs on existing rows
				if err := db.BackfillSourceMachineID(); err != nil {
					log.Printf("Warning: failed to backfill source_machine_id: %v", err)
				}

				// Backfill repo identities from git remotes
				if count, err := db.BackfillRepoIdentities(); err != nil {
					log.Printf("Warning: failed to backfill repo identities: %v", err)
				} else if count > 0 {
					log.Printf("Backfilled %d repo identities from git remotes", count)
				}

				syncWorker = storage.NewSyncWorker(db, cfg.Sync)
				syncWorker.SetAfterPullWrite(search.reconciler.Wake)
				search.reconciler.ShareVectors(syncWorker)
				syncWorker.SetAfterReviewPush(search.reconciler.ReviewsPushed)
				if err := syncWorker.Start(); err != nil {
					log.Printf("Warning: failed to start sync worker: %v", err)
				} else {
					log.Printf("Sync worker started (interval: %s)", cfg.Sync.Interval)
				}
			}

			// Create context for config watcher
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create and start server
			var serverOptions []daemon.ServerOption
			if webDevOrigin != "" {
				serverOptions = append(serverOptions, daemon.WithWebDevelopmentOrigin(webDevOrigin))
			}
			serverOptions = append(serverOptions, daemon.WithSearch(search.service, search.reconciler))
			server := daemon.NewServer(db, cfg, configPath, serverOptions...)
			server.SetTelemetry(telemetryReporter)
			if syncWorker != nil {
				server.SetSyncWorker(syncWorker)
			}

			// Start CI poller if enabled
			var ciPoller *daemon.CIPoller
			if cfg.CI.Enabled {
				ciPoller = daemon.NewCIPoller(db, server.ConfigWatcher(), server.Broadcaster())
				server.SetCIPoller(ciPoller) // wire callbacks before Start to avoid race
				if err := ciPoller.Start(); err != nil {
					log.Printf("Warning: failed to start CI poller: %v", err)
				} else {
					interval := cfg.CI.PollInterval
					if interval == "" {
						interval = "5m"
					}
					log.Printf("CI poller started (interval: %s, repos: %v)", interval, cfg.CI.Repos)
				}
			}

			// Handle shutdown signals
			sigCh, stopSignals := setupSignalHandler()
			defer stopSignals()

			go func() {
				select {
				case sig := <-sigCh:
					log.Printf("Received signal %v, shutting down...", sig)
				case <-server.ShutdownRequested():
					log.Printf("Shutdown requested via API, shutting down...")
				case <-cmd.Context().Done():
					log.Printf("Context cancelled, shutting down...")
				}

				cancel() // Cancel context to stop config watcher
				stopDaemonWithRetry(server.Stop, time.Second)
				// Note: Don't call os.Exit here - let server.Start() return naturally
				// after Stop() is called. This allows proper cleanup and testability.
			}()

			// Start blocks until HTTP serving stops. Join Stop before returning so
			// the process cannot exit while workers are still finalizing.
			return runDaemonWithSearch(ctx, server, search)
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", storage.DefaultDBPath(), "path to sqlite database")
	cmd.Flags().StringVar(&configPath, "config", config.GlobalConfigPath(), "path to config file")
	cmd.Flags().StringVar(&addr, "addr", "", "server address (overrides config)")
	cmd.Flags().IntVar(&workers, "workers", 0, "number of workers (overrides config)")
	cmd.Flags().StringVar(&webDevOrigin, "web-dev-origin", "", "exact loopback origin for web development")
	if err := cmd.Flags().MarkHidden("web-dev-origin"); err != nil {
		panic(err)
	}

	return cmd
}

func stopDaemonWithRetry(stop func() error, retryDelay time.Duration) {
	_, _ = backoff.Retry(context.Background(), func() (struct{}, error) {
		return struct{}{}, stop()
	}, backoff.WithBackOff(backoff.NewConstantBackOff(retryDelay)),
		backoff.WithMaxTries(0), backoff.WithMaxElapsedTime(0),
		backoff.WithNotify(func(err error, _ time.Duration) {
			log.Printf("Prepare daemon shutdown failed; retrying: %v", err)
		}))
}
