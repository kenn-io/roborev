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

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/telemetry"
	"go.kenn.io/roborev/internal/version"
)

func daemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the roborev daemon",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "Start the daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureDaemon(); err != nil {
				return err
			}
			fmt.Println("Daemon started")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := stopDaemon(); errors.Is(err, ErrDaemonNotRunning) {
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
			if err := stopDaemon(); errors.Is(err, ErrDaemonNotRunning) {
				wasRunning = false
			} else if err != nil {
				return err
			}
			if err := ensureDaemon(); err != nil {
				return err
			}
			if wasRunning {
				fmt.Println("Daemon restarted")
			} else {
				fmt.Println("Daemon started (was not running)")
			}
			return nil
		},
	})

	cmd.AddCommand(daemonRunCmd())

	return cmd
}

// daemonRunCmd runs the daemon in the foreground (used by "daemon start" internally)
func daemonRunCmd() *cobra.Command {
	var (
		dbPath     string
		configPath string
		addr       string
		workers    int
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
				log.Printf("Warning: failed to load config from %s: %v", configPath, err)
				cfg = config.DefaultConfig()
			}

			// Fail fast on invalid auto-design heuristic config. An
			// unchecked typo in trigger_paths/skip_paths or in one of
			// the message-pattern regexes would otherwise surface only
			// at dispatch time as a "heuristic error" skipped row,
			// silently suppressing every automatic design review.
			if cfg.AutoDesignReview.Enabled {
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
			server := daemon.NewServer(db, cfg, configPath)
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
				case <-cmd.Context().Done():
					log.Printf("Context cancelled, shutting down...")
				}

				cancel() // Cancel context to stop config watcher
				if ciPoller != nil {
					ciPoller.Stop()
				}
				if syncWorker != nil {
					// Final push before shutdown to ensure local changes are synced
					if err := syncWorker.FinalPush(); err != nil {
						log.Printf("Final sync push error: %v", err)
					}
					syncWorker.Stop()
				}
				if err := server.Stop(); err != nil {
					log.Printf("Shutdown error: %v", err)
				}
				// Note: Don't call os.Exit here - let server.Start() return naturally
				// after Stop() is called. This allows proper cleanup and testability.
			}()

			// Start server (blocks until shutdown)
			return server.Start(ctx)
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", storage.DefaultDBPath(), "path to sqlite database")
	cmd.Flags().StringVar(&configPath, "config", config.GlobalConfigPath(), "path to config file")
	cmd.Flags().StringVar(&addr, "addr", "", "server address (overrides config)")
	cmd.Flags().IntVar(&workers, "workers", 0, "number of workers (overrides config)")

	return cmd
}
