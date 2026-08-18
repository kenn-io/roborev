package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/skills"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/update"
	"go.kenn.io/roborev/internal/version"
)

var (
	checkForUpdateForCommand        = update.CheckForUpdate
	performUpdateForCommand         = update.PerformUpdateWithReporter
	prepareUpdateDaemonForCommand   = prepareUpdateDaemon
	restartUpdatedDaemonForCommand  = restartAndVerifyUpdatedDaemon
	repairHooksForUpdateCommand     = repairHooksAfterUpdateResult
	updateSkillsForUpdateCommand    = updateSkillsAfterUpdateResult
	installedSkillsForUpdateCommand = installedSkillsNeedUpdate
	waitLegacyDaemonExitForCommand  = waitForLegacyDaemonExit
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
		// Treat the handoff as unresolved.
		exited = false
	}
	if !exited {
		fmt.Printf(
			"warning: daemon pid %d is still running;"+
				" restart it manually\n", previousPID,
		)
		return
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

	if err := repairHooksAfterUpdateResult(binDir, run); err != nil {
		fmt.Printf("Updating git hooks... warning: %v\n", err)
		return
	}
	fmt.Println("Updating git hooks... OK")
}

func repairHooksAfterUpdateResult(binDir string, run repairHookRunner) error {
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

	if err := run(repairHookOptions{registered: true, binary: newBinary}); err != nil {
		return err
	}
	return nil
}

func installedSkillsNeedUpdate() bool {
	return slices.ContainsFunc([]skills.Agent{
		skills.AgentClaude,
		skills.AgentCodex,
		skills.AgentDroid,
		skills.AgentGrok,
	}, skills.IsInstalled)
}

func updateSkillsAfterUpdateResult(binDir string) error {
	if !installedSkillsNeedUpdate() {
		return nil
	}
	newBinary := updatedRoborevBinary(binDir)
	cmd := exec.Command(newBinary, "skills", "update")
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if detail := strings.TrimSpace(string(output)); detail != "" {
		return fmt.Errorf("%w: %s", err, detail)
	}
	return err
}

func parseRunningReviewPolicy(raw string) (runningReviewPolicy, error) {
	policy := runningReviewPolicy(strings.ToLower(strings.TrimSpace(raw)))
	switch policy {
	case policyWait, policyInterrupt, policyAbort, "":
		return policy, nil
	default:
		return "", fmt.Errorf(
			"invalid --running value %q: use wait, interrupt, or abort", raw,
		)
	}
}

func chooseRunningReviewPolicy(
	in io.Reader, out io.Writer, running int,
) (runningReviewPolicy, bool, error) {
	fmt.Fprintf(out, "%d reviews are currently running.\n\n", running)
	fmt.Fprintln(out, "  [w] Wait for them to finish, then update")
	fmt.Fprintln(out, "  [u] Update now; interrupt and restart them automatically")
	fmt.Fprintln(out, "  [a] Abort")
	fmt.Fprint(out, "\nChoice [a]: ")
	scanner := bufio.NewScanner(in)
	answer := ""
	if scanner.Scan() {
		answer = scanner.Text()
	} else if err := scanner.Err(); err != nil {
		return "", false, err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "w", "wait":
		return policyWait, true, nil
	case "u", "update", "interrupt":
		return policyInterrupt, true, nil
	default:
		return "", false, nil
	}
}

type commandUpdateReporter struct {
	out           io.Writer
	wroteProgress bool
	lastPercent   int
}

func (r *commandUpdateReporter) Stepf(string, ...any) {}

func (r *commandUpdateReporter) Progress(downloaded, total int64) {
	if total <= 0 {
		return
	}
	percent := int(downloaded * 100 / total)
	if r.wroteProgress && percent == r.lastPercent {
		return
	}
	r.wroteProgress = true
	r.lastPercent = percent
	fmt.Fprintf(r.out, "\r%-13s%d%% (%s)", "Downloading", percent, update.FormatSize(total))
}

func (r *commandUpdateReporter) Finish(total int64, success bool) {
	if r.wroteProgress {
		fmt.Fprintln(r.out)
		return
	}
	if success {
		printUpdatePhase(r.out, "Downloading", fmt.Sprintf("100%% (%s)", update.FormatSize(total)))
	}
}

func printUpdateSummary(out io.Writer, info *update.UpdateInfo, installPath string) {
	fmt.Fprintln(out, "Update available")
	fmt.Fprintf(out, "  Version  %s -> %s\n", info.CurrentVersion, info.LatestVersion)
	fmt.Fprintf(out, "  Package  %s (%s)\n", info.AssetName, update.FormatSize(info.Size))
	fmt.Fprintf(out, "  Install  %s\n", installPath)
	if verbose {
		fmt.Fprintf(out, "  URL      %s\n", info.DownloadURL)
		if info.Checksum != "" {
			fmt.Fprintf(out, "  SHA256   %s\n", info.Checksum)
		}
	}
}

func printUpdatePhase(out io.Writer, phase, result string) {
	fmt.Fprintf(out, "%-13s%s\n", phase, result)
}

func runControlledUpdate(
	ctx context.Context,
	in io.Reader,
	out io.Writer,
	info *update.UpdateInfo,
	binDir string,
	policy runningReviewPolicy,
	yes bool,
	noRestart bool,
) error {
	var runningDaemon *daemon.RuntimeInfo
	var session *updateDaemonSession
	confirmed := yes

	if !noRestart {
		var err error
		runningDaemon, err = discoverDaemonForUpdate()
		if err != nil {
			return err
		}
	}

	if runningDaemon != nil {
		status, err := fetchDaemonStatus(ctx, runningDaemon.Endpoint())
		if err != nil {
			return err
		}
		if policy == "" && !yes && status.RunningJobs > 0 {
			var selected bool
			policy, selected, err = chooseRunningReviewPolicy(
				in, out, int(status.RunningJobs),
			)
			if err != nil {
				return err
			}
			if !selected {
				fmt.Fprintln(out, "Update cancelled")
				return nil
			}
			confirmed = true
		}
		if policy == "" {
			policy = policyWait
		}
	}

	if !confirmed {
		accepted, err := confirmUpdate(in, out)
		if err != nil {
			return err
		}
		if !accepted {
			fmt.Fprintln(out, "Update cancelled")
			return nil
		}
	}
	fmt.Fprintln(out)

	operationCtx, cancelOperation := context.WithCancel(ctx)
	defer cancelOperation()
	heartbeatFailure := make(chan error, 1)
	var heartbeatMonitorDone chan struct{}
	stopHeartbeat := func() {
		if session != nil {
			session.stopHeartbeat()
		}
		if heartbeatMonitorDone != nil {
			<-heartbeatMonitorDone
			heartbeatMonitorDone = nil
		}
	}
	defer stopHeartbeat()
	defer func() {
		if session != nil && session.Prepared && !session.ShutdownOwned {
			releaseCtx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx), 2*time.Second,
			)
			defer cancel()
			_, _ = session.release(releaseCtx)
		}
	}()
	prepareDaemon := func(info *daemon.RuntimeInfo) error {
		prepared, err := prepareUpdateDaemonForCommand(
			operationCtx,
			info.Endpoint(),
			storage.GenerateUUID(),
			policy,
			out,
		)
		if err != nil {
			if errors.Is(err, errUpdateReviewsRunning) && policy == policyAbort {
				return fmt.Errorf("update aborted because reviews are running: %w", err)
			}
			return err
		}
		session = prepared
		heartbeat := session.startHeartbeat(operationCtx)
		heartbeatMonitorDone = make(chan struct{})
		go func() {
			defer close(heartbeatMonitorDone)
			if heartbeatErr, ok := <-heartbeat; ok && heartbeatErr != nil {
				heartbeatFailure <- heartbeatErr
				cancelOperation()
			}
		}()
		if err := waitForPreparedDrain(operationCtx, session, out); err != nil {
			return preferHeartbeatFailure(err, heartbeatFailure)
		}
		return nil
	}
	if runningDaemon == nil && !noRestart {
		appeared, err := discoverDaemonForUpdate()
		if err != nil {
			return err
		}
		if appeared != nil {
			runningDaemon = appeared
			if policy == "" {
				policy = policyWait
			}
		}
	}
	if runningDaemon != nil {
		if err := prepareDaemon(runningDaemon); err != nil {
			return err
		}
	}

	if err := operationCtx.Err(); err != nil {
		return preferHeartbeatFailure(err, heartbeatFailure)
	}
	reporter := &commandUpdateReporter{out: out}
	installErr := performUpdateForCommand(operationCtx, info, reporter)
	reporter.Finish(info.Size, installErr == nil)
	if installErr != nil {
		return fmt.Errorf("update failed: %w", preferHeartbeatFailure(installErr, heartbeatFailure))
	}
	printUpdatePhase(out, "Installing", "done")
	if session != nil {
		session.Installed = true
	}

	if err := operationCtx.Err(); err != nil {
		return installedUpdateInterruption(session, preferHeartbeatFailure(err, heartbeatFailure))
	}

	if session == nil && !noRestart {
		appeared, err := discoverDaemonForUpdate()
		if err != nil {
			return installedUpdateInterruption(session, err)
		}
		if appeared != nil {
			runningDaemon = appeared
			if policy == "" {
				policy = policyWait
			}
			if err := prepareDaemon(runningDaemon); err != nil {
				return installedUpdateInterruption(session, err)
			}
			session.Installed = true
		}
	}

	removeLegacyDaemonBinary(binDir)
	if noRestart {
		printUpdatePhase(out, "Daemon", "skipped (--no-restart)")
		printUpdatePhase(out, "Git hooks", "skipped (--no-restart)")
		printUpdatePhase(out, "Skills", "skipped (--no-restart)")
		fmt.Fprintf(out, "\nUpdated roborev to %s\n", info.LatestVersion)
		return nil
	}
	if session == nil {
		printUpdatePhase(out, "Daemon", "not running")
	} else {
		stopHeartbeat()
		if err := operationCtx.Err(); err != nil {
			return installedUpdateInterruption(session, preferHeartbeatFailure(err, heartbeatFailure))
		}
		if err := session.shutdown(operationCtx); err != nil {
			return installedUpdateInterruption(session, err)
		}
		session.ShutdownOwned = true
		if session.Legacy {
			if err := waitLegacyDaemonExitForCommand(
				operationCtx, runningDaemon.PID,
			); err != nil {
				return installedUpdateInterruption(session, err)
			}
		}
		if err := restartUpdatedDaemonForCommand(
			operationCtx, binDir, info.LatestVersion, runningDaemon,
		); err != nil {
			if errors.Is(err, context.Canceled) || operationCtx.Err() != nil {
				return installedUpdateInterruption(session, err)
			}
			return fmt.Errorf(
				"restart updated daemon: %w; run roborev daemon restart", err,
			)
		}
		printUpdatePhase(out, "Daemon", "restarted ("+info.LatestVersion+")")
	}

	if err := repairHooksForUpdateCommand(binDir, nil); err != nil {
		printUpdatePhase(out, "Git hooks", "warning: "+err.Error())
	} else {
		printUpdatePhase(out, "Git hooks", "done")
	}
	if err := operationCtx.Err(); err != nil {
		return installedUpdateInterruption(session, err)
	}
	if !installedSkillsForUpdateCommand() {
		printUpdatePhase(out, "Skills", "not installed")
	} else if err := updateSkillsForUpdateCommand(binDir); err != nil {
		printUpdatePhase(out, "Skills", "warning: "+err.Error())
	} else {
		printUpdatePhase(out, "Skills", "done")
	}
	if err := operationCtx.Err(); err != nil {
		return installedUpdateInterruption(session, err)
	}
	fmt.Fprintf(out, "\nUpdated roborev to %s\n", info.LatestVersion)
	return nil
}

func discoverDaemonForUpdate() (*daemon.RuntimeInfo, error) {
	info, probeErr := getAnyRunningDaemon()
	if probeErr == nil {
		return info, nil
	}
	runtimes, listErr := listAllRuntimes()
	if listErr != nil {
		return nil, fmt.Errorf("inspect daemon runtimes: %w", listErr)
	}
	for _, runtimeInfo := range runtimes {
		if runtimeInfo == nil || runtimeInfo.PID <= 0 {
			continue
		}
		if isPIDAliveForUpdate(runtimeInfo.PID) {
			return nil, fmt.Errorf("running daemon is not responsive: %w", probeErr)
		}
		if runtimeInfo.SourcePath != "" {
			_ = os.Remove(runtimeInfo.SourcePath)
		}
	}
	return nil, nil
}

func confirmUpdate(in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprint(out, "\nProceed with update? [y/N] ")
	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		return false, scanner.Err()
	}
	response := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return response == "y" || response == "yes", nil
}

func preferHeartbeatFailure(err error, heartbeatFailure <-chan error) error {
	select {
	case heartbeatErr := <-heartbeatFailure:
		if heartbeatErr != nil {
			return heartbeatErr
		}
	default:
	}
	return err
}

func installedUpdateInterruption(session *updateDaemonSession, cause error) error {
	if session != nil && session.ShutdownOwned {
		return fmt.Errorf(
			"binary installed; daemon is finishing shutdown — run roborev daemon restart: %w",
			cause,
		)
	}
	return fmt.Errorf(
		"binary installed; daemon still running old version — run roborev daemon restart: %w",
		cause,
	)
}

func removeLegacyDaemonBinary(binDir string) {
	oldDaemonPath := filepath.Join(binDir, "roborevd")
	if runtime.GOOS == "windows" {
		oldDaemonPath += ".exe"
	}
	if _, err := os.Stat(oldDaemonPath); err == nil {
		_ = os.Remove(oldDaemonPath)
	}
}

func updateCmd() *cobra.Command {
	var checkOnly bool
	var yes bool
	var force bool
	var noRestart bool
	var runningPolicy string

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
			policy, err := parseRunningReviewPolicy(runningPolicy)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			in := cmd.InOrStdin()

			// kit already wraps check errors with "check for updates:".
			info, err := checkForUpdateForCommand(true) // Force check, ignore cache
			if err != nil {
				return err
			}

			if info == nil {
				fmt.Fprintf(out, "Already running latest version (%s)\n", version.Version)
				return nil
			}

			// Show install location
			currentExe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("find executable: %w", err)
			}
			currentExe, _ = filepath.EvalSymlinks(currentExe)
			binDir := filepath.Dir(currentExe)
			printUpdateSummary(out, info, binDir)

			if checkOnly {
				if info.IsDevBuild {
					fmt.Fprintln(out, "\nUse --force to install the latest official release.")
				}
				return nil
			}

			// Dev builds require --force to update
			if info.IsDevBuild && !force {
				fmt.Fprintln(out, "\nUse --force to install the latest official release.")
				return nil
			}

			ctx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stopSignals()
			return runControlledUpdate(
				ctx, in, out, info, binDir, policy,
				yes, noRestart,
			)
		},
	}

	cmd.Flags().BoolVar(&checkOnly, "check", false, "only check for updates, don't install")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "replace dev build with latest official release")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false, "skip daemon restart after update (for launchd/systemd-managed daemons)")
	cmd.Flags().StringVar(&runningPolicy, "running", "", "when reviews are running: wait, interrupt, or abort")

	return cmd
}
