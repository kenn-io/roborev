package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/config"
	gitpkg "go.kenn.io/roborev/internal/git"
)

// HookRunner listens for broadcaster events and runs configured hooks.
type HookRunner struct {
	cfgGetter   ConfigGetter
	broadcaster Broadcaster
	logger      *log.Logger
	subID       int
	stopCh      chan struct{}
	idleCh      chan chan struct{}
	wg          sync.WaitGroup
}

// NewHookRunner creates a new HookRunner that subscribes to events from the broadcaster.
func NewHookRunner(cfgGetter ConfigGetter, broadcaster Broadcaster, logger *log.Logger) *HookRunner {
	if logger == nil {
		logger = log.Default()
	}
	subID, eventCh := broadcaster.Subscribe("")

	hr := &HookRunner{
		cfgGetter:   cfgGetter,
		broadcaster: broadcaster,
		logger:      logger,
		subID:       subID,
		stopCh:      make(chan struct{}),
		idleCh:      make(chan chan struct{}),
	}

	go hr.listen(eventCh)

	return hr
}

// listen processes events from the broadcaster and fires matching hooks.
func (hr *HookRunner) listen(eventCh <-chan Event) {
	for {
		// Prioritize processing events
		select {
		case <-hr.stopCh:
			return
		case event, ok := <-eventCh:
			if !ok {
				return
			}
			hr.handleEvent(event)
			continue
		default:
		}

		select {
		case <-hr.stopCh:
			return
		case req := <-hr.idleCh:
			// Drain any currently queued events before acknowledging idle
		drainLoop:
			for {
				select {
				case <-hr.stopCh:
					close(req)
					return
				case event, ok := <-eventCh:
					if !ok {
						close(req)
						return
					}
					hr.handleEvent(event)
				default:
					break drainLoop
				}
			}
			// Wait for in-flight hooks here (inside the listener) so
			// no new wg.Add(1) can race with wg.Wait().
			hr.wg.Wait()
			close(req)
		case event, ok := <-eventCh:
			if !ok {
				return
			}
			hr.handleEvent(event)
		}
	}
}

// WaitUntilIdle blocks until the currently queued events are drained and
// all in-flight hooks have finished. It is a point-in-time barrier: events
// arriving after the drain starts are handled on the next listener iteration.
func (hr *HookRunner) WaitUntilIdle() {
	ch := make(chan struct{})
	select {
	case hr.idleCh <- ch:
	case <-hr.stopCh:
		return
	}
	select {
	case <-ch:
	case <-hr.stopCh:
	}
}

// Stop shuts down the hook runner, waits for in-flight hooks to finish,
// and unsubscribes from the broadcaster. Unsubscribe runs before Wait to
// prevent the broadcaster from blocking on a full channel after the
// event loop exits.
func (hr *HookRunner) Stop() {
	close(hr.stopCh)
	hr.broadcaster.Unsubscribe(hr.subID)
	hr.wg.Wait()
}

// handleEvent checks all configured hooks against the event and fires matches.
func (hr *HookRunner) handleEvent(event Event) {
	// Only handle review events
	if !strings.HasPrefix(event.Type, "review.") {
		return
	}

	cfg := hr.cfgGetter.Config()
	if cfg == nil {
		return
	}

	// Resolve one effective repo path: prefer the worktree if it still
	// exists and belongs to the same repository. Used for both config
	// loading (.roborev.toml) and as the hook working directory.
	effectiveRepo := event.Repo
	if event.WorktreePath != "" && event.Repo != "" {
		if gitpkg.ValidateWorktreeForRepo(event.WorktreePath, event.Repo) {
			effectiveRepo = event.WorktreePath
		}
	}

	// Collect hooks: copy global slice to avoid aliasing, then append repo-specific.
	hooks := append([]config.HookConfig{}, cfg.Hooks...)
	if effectiveRepo != "" {
		if repoCfg, err := config.LoadRepoConfig(effectiveRepo); err == nil && repoCfg != nil {
			hooks = append(hooks, repoCfg.Hooks...)
		}
	}

	fired := 0
	for _, hook := range hooks {
		if !matchEvent(hook.Event, event.Type) {
			continue
		}

		if hook.Type == "webhook" {
			if hook.URL == "" {
				continue
			}

			fired++
			hr.wg.Add(1)
			go hr.postWebhook(hook.URL, event)
			continue
		}

		cmd := resolveCommand(hook, event)
		if cmd == "" {
			continue
		}

		fired++
		// Run async so hooks don't block workers
		hr.wg.Add(1)
		go hr.runHook(cmd, effectiveRepo)
	}

	if fired > 0 {
		hr.logger.Printf("Hooks: fired %d hook(s) for %s (job %d)", fired, event.Type, event.JobID)
	}
}

// matchEvent checks if an event type matches a hook's event pattern.
// Supports exact match and "review.*" wildcard.
func matchEvent(pattern, eventType string) bool {
	if pattern == eventType {
		return true
	}
	// Support wildcard like "review.*"
	if before, ok := strings.CutSuffix(pattern, ".*"); ok {
		prefix := before
		return strings.HasPrefix(eventType, prefix+".")
	}
	return false
}

// resolveCommand builds the shell command for a hook, handling built-in types
// and template variable interpolation.
func resolveCommand(hook config.HookConfig, event Event) string {
	if hook.Type == "beads" {
		return beadsCommand(event)
	}
	if hook.Type == "lint" {
		return lintCommand(event)
	}
	return interpolate(hook.Command, event)
}

// beadsCommand generates a bd create command for the beads built-in hook.
func beadsCommand(event Event) string {
	repoName := event.RepoName
	if repoName == "" {
		repoName = filepath.Base(event.Repo)
	}

	shortSHA := gitpkg.ShortSHA(event.SHA)

	switch event.Type {
	case "review.failed":
		title := fmt.Sprintf("Review failed for %s (%s): run roborev show %d", repoName, shortSHA, event.JobID)
		return fmt.Sprintf("bd create %s -p 1", shellEscape(title))
	case "review.completed":
		if event.Verdict == "F" {
			title := fmt.Sprintf("Review findings for %s (%s): roborev show %d / one-shot fix with roborev fix %d", repoName, shortSHA, event.JobID, event.JobID)
			return fmt.Sprintf("bd create %s -p 2", shellEscape(title))
		}
		return "" // No issue for passing reviews
	default:
		return ""
	}
}

// lintCommand generates a roborev lint command for the lint built-in hook.
// Runs semgrep on changed files and logs findings.
func lintCommand(event Event) string {
	if event.Repo == "" {
		return ""
	}
	switch event.Type {
	case "review.completed":
		repo := shellEscape(event.Repo)
		return fmt.Sprintf("cd %s && roborev lint --diff --quiet 2>&1", repo)
	default:
		return ""
	}
}

// interpolate replaces {var} template variables in a command string.
// Values are shell-escaped to prevent injection via event fields.
func interpolate(cmd string, event Event) string {
	if cmd == "" {
		return ""
	}

	r := strings.NewReplacer(
		"{job_id}", fmt.Sprintf("%d", event.JobID),
		"{repo}", shellEscape(event.Repo),
		"{repo_name}", shellEscape(event.RepoName),
		"{sha}", shellEscape(event.SHA),
		"{agent}", shellEscape(event.Agent),
		"{verdict}", shellEscape(event.Verdict),
		"{findings}", shellEscape(event.Findings),
		"{error}", shellEscape(event.Error),
	)
	return r.Replace(cmd)
}

// shellEscape quotes a value for safe interpolation into a shell command.
// Wraps in single quotes on all platforms, with embedded single quotes escaped.
// On Windows (PowerShell), a doubled single-quote escapes a literal one. On Unix, uses the quote-break-quote idiom.
func shellEscape(s string) string {
	if runtime.GOOS == "windows" {
		// PowerShell single-quoted strings: only escape is '' for literal '.
		if s == "" {
			return "''"
		}
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// runHook executes a shell command in the given working directory.
// Errors are logged but never propagated.
func (hr *HookRunner) runHook(command, workDir string) {
	defer hr.wg.Done()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// Use PowerShell for reliable path handling and command execution.
		// -NoProfile avoids loading user profiles that could slow or alter execution.
		// -Command takes the rest as a PowerShell script string.
		cmd = exec.Command("powershell", "-NoProfile", "-Command", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	if workDir != "" {
		cmd.Dir = workDir
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		hr.logger.Printf("Hook error (cmd=%q dir=%q): %v\n%s", command, workDir, err, output)
		return
	}
	if len(output) > 0 {
		hr.logger.Printf("Hook output (cmd=%q): %s", command, output)
	}
}

func (hr *HookRunner) postWebhook(webhookURL string, event Event) {
	defer hr.wg.Done()
	safeURL := redactWebhookURL(webhookURL)

	payload, err := json.Marshal(event)
	if err != nil {
		hr.logger.Printf("Webhook error (url=%q): marshal event: %v", safeURL, err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(payload))
	if err != nil {
		hr.logger.Printf("Webhook error (url=%q): build request: %v", safeURL, redactURLError(err))
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		hr.logger.Printf("Webhook error (url=%q): %v", safeURL, redactURLError(err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if len(body) > 0 {
			hr.logger.Printf("Webhook error (url=%q): status %s: %s", safeURL, resp.Status, strings.TrimSpace(string(body)))
			return
		}
		hr.logger.Printf("Webhook error (url=%q): status %s", safeURL, resp.Status)
	}
}

func redactWebhookURL(raw string) string {
	parsed, err := neturl.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "<invalid webhook url>"
	}

	redacted := &neturl.URL{
		Scheme: parsed.Scheme,
		Host:   parsed.Host,
	}

	if p := parsed.EscapedPath(); p != "" && p != "/" {
		redacted.Path = "/..."
	}

	return redacted.String()
}

// redactURLError unwraps *url.Error to return only its inner
// error, preventing Go's HTTP client from leaking the raw URL
// (including secret path segments) in log output.
func redactURLError(err error) error {
	var ue *neturl.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
