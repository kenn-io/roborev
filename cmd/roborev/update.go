package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/skills"
	"go.kenn.io/roborev/internal/update"
	"go.kenn.io/roborev/internal/version"
)

// waitForDaemonExit polls until the daemon with previousPID no longer
// appears in runtime files and the process is gone, or the timeout expires.
// Returns (exited, newPID) where
// newPID > 0 means an external manager already restarted the daemon
// with a different PID.
func waitForDaemonExit(
	previousPID int, timeout time.Duration,
) (exited bool, newPID int) {
	deadline := time.Now().Add(timeout)
	for {
		info, err := getAnyRunningDaemon()
		if err != nil {
			if previousPIDExited(previousPID) {
				// A manager-restarted daemon may exist but not be
				// health-responsive yet. Detect the replacement PID
				// from runtime files to avoid duplicate manual starts.
				if handoffPID := replacementRuntimePID(previousPID); handoffPID > 0 {
					return true, handoffPID
				}
				return true, 0
			}
		} else if info.PID != previousPID {
			// A new daemon PID can appear before the previous daemon has
			// fully exited. Treat this as a successful handoff only after
			// the previous PID disappears from runtime files.
			if previousPIDExited(previousPID) {
				return true, info.PID
			}
		}
		if time.Now().After(deadline) {
			return false, 0
		}
		time.Sleep(updateRestartPollInterval)
	}
}

// waitForNewDaemonReady polls until any daemon becomes responsive or the
// timeout expires.
func waitForNewDaemonReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := getAnyRunningDaemon(); err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(updateRestartPollInterval)
	}
}

// runtimePID returns the PID from the first daemon runtime file on
// disk, or 0 if none exist. Used as a fallback when the daemon is not
// responding to health probes.
func runtimePID() int {
	runtimes, err := listAllRuntimes()
	if err != nil || len(runtimes) == 0 {
		return 0
	}
	for _, info := range runtimes {
		if info != nil && info.PID > 0 {
			return info.PID
		}
	}
	return 0
}

// runtimeHasPID returns true when a runtime file for pid exists.
// On read/list errors, it conservatively returns true so callers continue
// waiting rather than treating the daemon as fully exited.
//
// Dead runtime entries are treated as stale and ignored (best-effort
// cleanup) so they don't block shutdown/restart handoff.
func runtimeHasPID(pid int) bool {
	if pid <= 0 {
		return false
	}
	runtimes, err := listAllRuntimes()
	if err != nil {
		return true
	}
	for _, info := range runtimes {
		if info == nil || info.PID != pid {
			continue
		}
		if isPIDAliveForUpdate(pid) {
			return true
		}
		// Best-effort stale runtime cleanup.
		if info.SourcePath != "" {
			_ = os.Remove(info.SourcePath)
		}
		return false
	}
	return false
}

// previousPIDExited returns true when previousPID no longer appears in
// runtime files and the process no longer exists.
func previousPIDExited(previousPID int) bool {
	if previousPID <= 0 {
		return true
	}
	if runtimeHasPID(previousPID) {
		return false
	}
	return !isPIDAliveForUpdate(previousPID)
}

// replacementRuntimePID returns a live daemon PID from runtime files
// that differs from previousPID, or 0 if none are found.
func replacementRuntimePID(previousPID int) int {
	pids, err := runtimePIDSet()
	if err != nil {
		return 0
	}
	best := 0
	for pid := range pids {
		if pid <= 0 || pid == previousPID {
			continue
		}
		if !isPIDAliveForUpdate(pid) {
			continue
		}
		if best == 0 || pid < best {
			best = pid
		}
	}
	return best
}

func pidInSet(pids map[int]struct{}, pid int) bool {
	if len(pids) == 0 || pid <= 0 {
		return false
	}
	_, ok := pids[pid]
	return ok
}

// runtimePIDSet returns all runtime PIDs currently on disk.
func runtimePIDSet() (map[int]struct{}, error) {
	runtimes, err := listAllRuntimes()
	if err != nil {
		return nil, err
	}
	pids := make(map[int]struct{}, len(runtimes))
	for _, info := range runtimes {
		if info != nil && info.PID > 0 {
			pids[info.PID] = struct{}{}
		}
	}
	return pids, nil
}

// initialPIDsExited returns true when none of the initial runtime PIDs
// are still represented by a runtime file or a live process, excluding
// allowPID (typically the manager-restarted PID).
func initialPIDsExited(initialPIDs map[int]struct{}, allowPID int) bool {
	if len(initialPIDs) == 0 {
		return true
	}
	currentPIDs, err := runtimePIDSet()
	if err != nil {
		return false
	}
	for pid := range initialPIDs {
		if pid == allowPID {
			continue
		}
		if _, exists := currentPIDs[pid]; exists || isPIDAliveForUpdate(pid) {
			return false
		}
	}
	return true
}

func restartDaemonAfterUpdate(binDir string, noRestart bool) {
	// Check for a responsive daemon first; fall back to runtime
	// files so we don't silently skip when the daemon is running
	// but temporarily unresponsive.
	runningInfo, err := getAnyRunningDaemon()
	if err != nil && runtimePID() == 0 {
		return
	}

	if noRestart {
		fmt.Println("Skipping daemon restart (--no-restart)")
		return
	}

	fmt.Print("Restarting daemon... ")

	previousPID := 0
	if runningInfo != nil {
		previousPID = runningInfo.PID
	} else {
		previousPID = runtimePID()
	}

	initialRuntimePIDs, initialPIDsErr := runtimePIDSet()
	if initialPIDsErr != nil {
		initialRuntimePIDs = make(map[int]struct{})
	}
	if previousPID > 0 {
		initialRuntimePIDs[previousPID] = struct{}{}
	}

	stopErr := stopDaemonForUpdate()
	stopFailed := stopErr != nil && !errors.Is(stopErr, ErrDaemonNotRunning)
	if stopFailed {
		fmt.Printf("warning: failed to stop daemon: %v\n", stopErr)
	}

	// Wait for the old daemon to exit. If an external service
	// manager (launchd/systemd) restarts it, we detect the new
	// PID and skip manual start.
	exited, newPID := waitForDaemonExit(
		previousPID, updateRestartWaitTimeout,
	)
	if newPID > 0 {
		// If stop reported failure, require stronger evidence that
		// all pre-update daemon PIDs are gone before accepting
		// manager restart as success.
		if !stopFailed || (initialPIDsErr == nil && !pidInSet(initialRuntimePIDs, newPID) && initialPIDsExited(initialRuntimePIDs, newPID)) {
			// Runtime-file handoff can race before the replacement daemon
			// is actually serving; only accept success once responsive.
			if waitForNewDaemonReady(updateRestartWaitTimeout) {
				fmt.Println("OK")
				return
			}
			// A replacement PID already exists; avoid manually starting
			// another daemon instance while handoff is still warming up.
			fmt.Println(
				"warning: daemon handoff detected but replacement is not ready;" +
					" restart it manually",
			)
			return
		}
		// Treat as unresolved and continue to kill fallback.
		exited = false
	}
	if !exited {
		// Forcefully kill orphaned/stuck daemon processes.
		killAllDaemonsForUpdate()
		exitedAfterKill, newPIDAfterKill := waitForDaemonExit(
			previousPID, updateRestartWaitTimeout,
		)
		if newPIDAfterKill > 0 {
			// Apply the same stop-failure safety gate here to avoid
			// accepting a manager restart while other pre-update
			// daemon runtimes are still present.
			if stopFailed && (initialPIDsErr != nil || pidInSet(initialRuntimePIDs, newPIDAfterKill) || !initialPIDsExited(initialRuntimePIDs, newPIDAfterKill)) {
				fmt.Println(
					"warning: daemon restart detected but older daemon runtimes" +
						" remain; restart it manually",
				)
				return
			}
			// Do not report success until the handoff daemon is responsive.
			if waitForNewDaemonReady(updateRestartWaitTimeout) {
				fmt.Println("OK")
				return
			}
			// A replacement PID already exists; avoid manually starting
			// another daemon instance while handoff is still warming up.
			fmt.Println(
				"warning: daemon handoff detected but replacement is not ready;" +
					" restart it manually",
			)
			return
		}
		if !exitedAfterKill {
			fmt.Printf(
				"warning: daemon pid %d is still running;"+
					" restart it manually\n", previousPID,
			)
			return
		}
	}

	// stopDaemonForUpdate reported failure; do not manually start a new daemon
	// unless daemon runtime state is successfully verified first.
	if stopFailed {
		if initialPIDsErr != nil {
			// Initial snapshot failed; require a successful resnapshot with
			// no remaining daemon runtimes before we attempt manual start.
			currentPIDs, err := runtimePIDSet()
			if err != nil {
				fmt.Println(
					"warning: failed to verify daemon runtimes after stop;" +
						" restart it manually",
				)
				return
			}
			if len(currentPIDs) > 0 {
				fmt.Println(
					"warning: older daemon runtimes still present after stop;" +
						" restart it manually",
				)
				return
			}
		} else if !initialPIDsExited(initialRuntimePIDs, 0) {
			fmt.Println(
				"warning: older daemon runtimes still present after stop;" +
					" restart it manually",
			)
			return
		}
	}

	if err := startUpdatedDaemon(binDir); err != nil {
		fmt.Printf("warning: failed to start daemon: %v\n", err)
		return
	}

	if waitForNewDaemonReady(updateRestartWaitTimeout) {
		fmt.Println("OK")
		return
	}

	fmt.Println(
		"warning: daemon did not become ready after restart;" +
			" restart it manually",
	)
}

func updatedRoborevBinary(binDir string) string {
	newBinary := filepath.Join(binDir, "roborev")
	if runtime.GOOS == "windows" {
		newBinary += ".exe"
	}
	return newBinary
}

type repairHookRunner func(opts repairHookOptions) error

func repairHooksAfterUpdate(binDir string, noRestart bool, run repairHookRunner) {
	if noRestart {
		fmt.Println("Skipping git hook update (--no-restart)")
		return
	}

	newBinary := updatedRoborevBinary(binDir)
	if run == nil {
		run = func(opts repairHookOptions) error {
			cmd := exec.Command(
				newBinary,
				"install-hook",
				"repair",
				"--registered",
			)
			output, err := cmd.CombinedOutput()
			if err != nil {
				if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
					return fmt.Errorf("%w: %s", err, trimmed)
				}
				return err
			}
			return nil
		}
	}

	fmt.Print("Updating git hooks... ")
	if err := run(repairHookOptions{registered: true, binary: newBinary}); err != nil {
		fmt.Printf("warning: %v\n", err)
		return
	}
	fmt.Println("OK")
}

func updateCmd() *cobra.Command {
	var checkOnly bool
	var yes bool
	var force bool
	var noRestart bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update roborev to the latest version",
		Long: `Check for and install roborev updates.

Shows exactly what will be downloaded and where it will be installed.
Requires confirmation before making changes (use --yes to skip).

Dev builds are not replaced by default. Use --force to install the latest
official release over a dev build.

Use --no-restart when daemon lifecycle is managed externally (for example,
launchd or systemd).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("Checking for updates...")

			// kit already wraps check errors with "check for updates:".
			info, err := update.CheckForUpdate(true) // Force check, ignore cache
			if err != nil {
				return err
			}

			if info == nil {
				fmt.Printf("Already running latest version (%s)\n", version.Version)
				return nil
			}

			fmt.Printf("\n  Current version: %s\n", info.CurrentVersion)
			fmt.Printf("  Latest version:  %s\n", info.LatestVersion)
			if info.IsDevBuild {
				fmt.Println("\nYou're running a dev build. Latest official release available.")
			} else {
				fmt.Println("\nUpdate available!")
			}
			fmt.Println("\nDownload:")
			fmt.Printf("  URL:  %s\n", info.DownloadURL)
			fmt.Printf("  Size: %s\n", update.FormatSize(info.Size))
			if info.Checksum != "" {
				fmt.Printf("  SHA256: %s\n", info.Checksum)
			}

			// Show install location
			currentExe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("find executable: %w", err)
			}
			currentExe, _ = filepath.EvalSymlinks(currentExe)
			binDir := filepath.Dir(currentExe)

			fmt.Println("\nInstall location:")
			fmt.Printf("  %s\n", binDir)

			if checkOnly {
				if info.IsDevBuild {
					fmt.Println("\nUse --force to install the latest official release.")
				}
				return nil
			}

			// Dev builds require --force to update
			if info.IsDevBuild && !force {
				fmt.Println("\nUse --force to install the latest official release.")
				return nil
			}

			// Confirm
			if !yes {
				fmt.Print("\nProceed with update? [y/N] ")
				var response string
				_, _ = fmt.Scanln(&response)
				if strings.ToLower(response) != "y" && strings.ToLower(response) != "yes" {
					fmt.Println("Update cancelled")
					return nil
				}
			}

			fmt.Println()

			// Progress display
			var lastPercent int
			progressFn := func(downloaded, total int64) {
				if total > 0 {
					percent := int(downloaded * 100 / total)
					if percent != lastPercent {
						fmt.Printf("\rDownloading... %d%% (%s / %s)",
							percent, update.FormatSize(downloaded), update.FormatSize(total))
						lastPercent = percent
					}
				}
			}

			// Perform update
			if err := update.PerformUpdate(info, progressFn); err != nil {
				return fmt.Errorf("update failed: %w", err)
			}

			fmt.Printf("\nUpdated to %s\n", info.LatestVersion)

			// Clean up old roborevd binary if it exists (consolidated into roborev)
			oldDaemonPath := filepath.Join(binDir, "roborevd")
			if runtime.GOOS == "windows" {
				oldDaemonPath += ".exe"
			}
			if _, err := os.Stat(oldDaemonPath); err == nil {
				fmt.Print("Removing old roborevd binary... ")
				if err := os.Remove(oldDaemonPath); err != nil {
					fmt.Printf("warning: %v\n", err)
				} else {
					fmt.Println("OK")
				}
			}

			restartDaemonAfterUpdate(binDir, noRestart)
			repairHooksAfterUpdate(binDir, noRestart, nil)

			// Update skills using the NEW binary (current process has old embedded skills)
			// Use "skills update" to only update agents that already have skills installed
			if skills.IsInstalled(skills.AgentClaude) || skills.IsInstalled(skills.AgentCodex) {
				fmt.Print("Updating skills... ")
				newBinary := updatedRoborevBinary(binDir)
				skillsCmd := exec.Command(newBinary, "skills", "update")
				if output, err := skillsCmd.CombinedOutput(); err != nil {
					fmt.Printf("warning: %v\n", err)
				} else {
					// Parse output to show what changed
					lines := strings.SplitSeq(strings.TrimSpace(string(output)), "\n")
					found := false
					for line := range lines {
						if strings.Contains(line, "updated") || strings.Contains(line, "installed") {
							fmt.Println(line)
							found = true
						}
					}
					if !found {
						fmt.Println("OK")
					}
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&checkOnly, "check", false, "only check for updates, don't install")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "replace dev build with latest official release")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "skip daemon restart after update (for launchd/systemd-managed daemons)")

	return cmd
}
