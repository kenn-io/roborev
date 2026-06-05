package agenthook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	roborevdaemon "go.kenn.io/roborev/internal/daemon"
	roborevgit "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
)

var agentHookGit = gitcmd.New()

func LoadState() (*StateStore, error) {
	path := StatePath()
	s := &StateStore{
		path:     path,
		sessions: map[string]SessionState{},
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open agent hook state: %w", err)
	}
	defer file.Close()

	var snap Snapshot
	if err := json.NewDecoder(file).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode agent hook state: %w", err)
	}
	if snap.Sessions != nil {
		s.sessions = snap.Sessions
	}
	return s, nil
}

func StatePath() string {
	return filepath.Join(config.DataDir(), "agent-hook", "state.json")
}

func (s *StateStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create agent hook state dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "state.*.json.tmp")
	if err != nil {
		return fmt.Errorf("create agent hook state temp: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(Snapshot{Sessions: s.sessions}); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode agent hook state: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod agent hook state temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close agent hook state temp: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace agent hook state: %w", err)
	}
	ok = true
	return nil
}

func (s *StateStore) Record(req Request) (Response, error) {
	switch req.Event.HookEventName {
	case "PreToolUse":
		return s.recordPreToolUse(req)
	case "", "Stop":
		return s.recordStop(req)
	case "PostToolUse":
		return s.recordPostToolUse(req)
	default:
		return Response{SessionID: req.Event.SessionID, Skipped: true}, nil
	}
}

func (s *StateStore) recordStop(req Request) (Response, error) {
	repoRoot, head, ok := currentGitHead(req.Event.CWD)
	if !ok {
		return Response{
			SessionID:             req.Event.SessionID,
			Threshold:             req.Threshold,
			FailedReviewThreshold: req.FailedReviewThreshold,
			Skipped:               true,
		}, nil
	}
	branch := currentGitBranch(repoRoot)
	failedReviewCount, haveFailedReviewCount := countOpenFailedReviews(
		context.Background(), mainRepoRoot(repoRoot), branch, head, req.RoborevServerAddr,
	)

	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.sessions[req.Event.SessionID]
	if req.Event.StopHookActive {
		return Response{
			SessionID:             req.Event.SessionID,
			Count:                 st.Count,
			Threshold:             req.Threshold,
			FailedReviewCount:     st.FailedReviewCount,
			FailedReviewThreshold: req.FailedReviewThreshold,
			ReminderPromptCount:   st.ReminderPromptCount,
			Skipped:               true,
		}, nil
	}

	now := time.Now().UTC()
	st.Count++
	st.StopCountSincePrompt++
	st.LastTurnID = req.Event.TurnID
	st.LastCWD = req.Event.CWD
	st.LastSeenAt = now

	actionableReviews := hasActionableFailedReviews(failedReviewCount, haveFailedReviewCount)
	stopTriggered := thresholdReady(st.StopCountSincePrompt, req.Threshold) && actionableReviews
	if stopTriggered {
		st.TriggeredAt = now
	}
	failedReviewTriggered := applyFailedReviewTrigger(req, &st, repoRoot, branch, failedReviewCount, haveFailedReviewCount, now)
	promptTriggered := stopTriggered || failedReviewTriggered
	if promptTriggered {
		st.ReminderPromptCount++
		resetPromptCounters(&st, repoHeadKey(repoRoot, branch))
	}
	s.sessions[req.Event.SessionID] = st
	if err := s.saveLocked(); err != nil {
		return Response{}, err
	}

	resp := Response{
		SessionID:             req.Event.SessionID,
		Count:                 st.Count,
		Threshold:             req.Threshold,
		FailedReviewCount:     st.FailedReviewCount,
		FailedReviewThreshold: req.FailedReviewThreshold,
		ReminderPromptCount:   st.ReminderPromptCount,
		Triggered:             promptTriggered,
	}
	switch {
	case failedReviewTriggered:
		resp.TriggeredBy = "failed_reviews"
		resp.Reason = buildFailedReviewReason(req, st)
	case stopTriggered:
		resp.TriggeredBy = "stop"
		resp.Reason = buildStopReason(req, st)
	}
	return resp, nil
}

func (s *StateStore) recordPreToolUse(req Request) (Response, error) {
	if req.Event.ToolName != "" && req.Event.ToolName != "Bash" {
		return Response{
			SessionID:             req.Event.SessionID,
			CommitThreshold:       req.CommitThreshold,
			FailedReviewThreshold: req.FailedReviewThreshold,
			Skipped:               true,
		}, nil
	}
	if !IsCommitProducingCommand(req.Event.Command()) {
		return Response{
			SessionID:             req.Event.SessionID,
			CommitThreshold:       req.CommitThreshold,
			FailedReviewThreshold: req.FailedReviewThreshold,
			Skipped:               true,
		}, nil
	}

	repoRoot, head, ok := currentGitHead(commandGitDir(req.Event.CWD, req.Event.Command()))
	if !ok {
		return Response{
			SessionID:             req.Event.SessionID,
			CommitThreshold:       req.CommitThreshold,
			FailedReviewThreshold: req.FailedReviewThreshold,
			Skipped:               true,
		}, nil
	}
	branch := currentGitBranch(repoRoot)

	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.sessions[req.Event.SessionID]
	if st.RepoHeads == nil {
		st.RepoHeads = map[string]string{}
	}
	st.RepoHeads[repoHeadKey(repoRoot, branch)] = head
	st.LastCWD = req.Event.CWD
	st.LastSeenAt = time.Now().UTC()
	s.sessions[req.Event.SessionID] = st
	if err := s.saveLocked(); err != nil {
		return Response{}, err
	}

	return Response{
		SessionID:             req.Event.SessionID,
		CommitThreshold:       req.CommitThreshold,
		FailedReviewThreshold: req.FailedReviewThreshold,
	}, nil
}

func (s *StateStore) recordPostToolUse(req Request) (Response, error) {
	if req.Event.ToolName != "" && req.Event.ToolName != "Bash" {
		return Response{
			SessionID:             req.Event.SessionID,
			CommitThreshold:       req.CommitThreshold,
			FailedReviewThreshold: req.FailedReviewThreshold,
			Skipped:               true,
		}, nil
	}

	command := req.Event.Command()
	commitCommand := IsCommitProducingCommand(command)
	// Only commit commands move HEAD, so only they need the effective working
	// directory resolved from -C options; every other command tracks the cwd repo.
	gitDir := req.Event.CWD
	if commitCommand {
		gitDir = commandGitDir(req.Event.CWD, command)
	}

	repoRoot, head, ok := currentGitHead(gitDir)
	if !ok {
		return Response{
			SessionID:             req.Event.SessionID,
			CommitThreshold:       req.CommitThreshold,
			FailedReviewThreshold: req.FailedReviewThreshold,
			Skipped:               true,
		}, nil
	}

	branch := currentGitBranch(repoRoot)
	failedReviewCount, haveFailedReviewCount := countOpenFailedReviews(
		context.Background(), mainRepoRoot(repoRoot), branch, head, req.RoborevServerAddr,
	)

	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.sessions[req.Event.SessionID]
	if st.RepoHeads == nil {
		st.RepoHeads = map[string]string{}
	}
	headKey := repoHeadKey(repoRoot, branch)
	previousHead := st.RepoHeads[headKey]
	// Count commits only against a HEAD baseline recorded earlier in the
	// session; the first observation merely establishes that baseline below.
	// Counting on the first observation would misfire when a failed commit
	// command leaves an unrelated older commit as the latest reflog entry.
	increment := 0
	if commitCommand && previousHead != "" && previousHead != head {
		increment = countNewCommits(repoRoot, previousHead, head)
	}

	st.RepoHeads[headKey] = head
	st.LastCWD = req.Event.CWD
	now := time.Now().UTC()
	st.LastSeenAt = now
	if increment > 0 {
		st.CommitCount += increment
		// CommitCountsSincePrompt is keyed by repo/branch so a deferred reminder
		// for one checkout is never advanced, consumed, or reset by another.
		if st.CommitCountsSincePrompt == nil {
			st.CommitCountsSincePrompt = map[string]int{}
		}
		st.CommitCountsSincePrompt[headKey] += increment
		st.LastCommitRepo = repoRoot
		st.LastCommitHead = head
	}

	actionableReviews := hasActionableFailedReviews(failedReviewCount, haveFailedReviewCount)
	// The commit reminder fires once this checkout's threshold is met and
	// actionable failed reviews exist; it does not require a commit in this exact
	// event, because reviews are produced asynchronously and the failures for the
	// commit that crossed the threshold usually only land on a later tool call.
	// The count is keyed by repo/branch, so a deferred reminder for one checkout
	// is not consumed or reset by activity in another. thresholdReady implies a
	// real commit was counted for this checkout since its last prompt, and
	// triggering resets that checkout's count.
	commitTriggered := thresholdReady(st.CommitCountsSincePrompt[headKey], req.CommitThreshold) && actionableReviews
	// Capture this checkout's count before resetPromptCounters clears it, so the
	// reminder text reports the triggering repo's commits, not session-wide totals.
	triggeringCommitCount := st.CommitCountsSincePrompt[headKey]
	if commitTriggered {
		st.CommitTriggeredAt = now
	}
	failedReviewTriggered := applyFailedReviewTrigger(req, &st, repoRoot, branch, failedReviewCount, haveFailedReviewCount, now)
	promptTriggered := commitTriggered || failedReviewTriggered
	if promptTriggered {
		st.ReminderPromptCount++
		resetPromptCounters(&st, headKey)
	}
	s.sessions[req.Event.SessionID] = st
	if err := s.saveLocked(); err != nil {
		return Response{}, err
	}

	resp := Response{
		SessionID:             req.Event.SessionID,
		Count:                 st.Count,
		Threshold:             req.Threshold,
		CommitCount:           st.CommitCount,
		CommitThreshold:       req.CommitThreshold,
		FailedReviewCount:     st.FailedReviewCount,
		FailedReviewThreshold: req.FailedReviewThreshold,
		ReminderPromptCount:   st.ReminderPromptCount,
		Triggered:             promptTriggered,
	}
	switch {
	case failedReviewTriggered:
		resp.TriggeredBy = "failed_reviews"
		resp.Reason = buildFailedReviewReason(req, st)
	case commitTriggered:
		resp.TriggeredBy = "commit"
		resp.Reason = buildCommitReason(req, triggeringCommitCount, repoRoot)
	}
	return resp, nil
}

func hasActionableFailedReviews(count int, ok bool) bool {
	return ok && count > 0
}

func thresholdReady(countSincePrompt, threshold int) bool {
	return threshold > 0 && countSincePrompt >= threshold
}

// resetPromptCounters restarts the per-prompt counters after a reminder fires.
// StopCountSincePrompt is session-wide, but the commit count is cleared only for
// key (the checkout being prompted) so a prompt in one repo/branch cannot
// discard a deferred commit reminder owed to another.
func resetPromptCounters(st *SessionState, key string) {
	st.StopCountSincePrompt = 0
	delete(st.CommitCountsSincePrompt, key)
}

func repoHeadKey(repoRoot, branch string) string {
	if branch == "" {
		return repoRoot
	}
	return repoRoot + "\x00" + branch
}

func applyFailedReviewTrigger(
	req Request, st *SessionState, repoRoot, branch string, count int, ok bool, now time.Time,
) bool {
	if !ok || req.FailedReviewThreshold <= 0 {
		return false
	}
	st.FailedReviewCount = count
	st.LastFailedReviewRepo = repoRoot
	st.LastFailedReviewBranch = branch
	// failedReviewCount is scoped to the current repo/branch, so dedup the prompt
	// per repo/branch. A single session-wide counter would let a prompt in one
	// repo/branch suppress prompts in another with an equal or lower count.
	key := repoHeadKey(repoRoot, branch)
	if count < req.FailedReviewThreshold {
		delete(st.FailedReviewTriggeredCounts, key)
		return false
	}
	if !thresholdReady(count-st.FailedReviewTriggeredCounts[key], req.FailedReviewThreshold) {
		return false
	}
	if st.FailedReviewTriggeredCounts == nil {
		st.FailedReviewTriggeredCounts = map[string]int{}
	}
	st.FailedReviewTriggeredCounts[key] = count
	st.FailedReviewTriggeredAt = now
	return true
}

func buildStopReason(req Request, st SessionState) string {
	return buildPromptReason(req, fmt.Sprintf("%s reached.", countPhrase(st.Count, "Stop hook", "Stop hooks")))
}

// buildCommitReason describes the commit reminder for the checkout that triggered
// it. count and repo come from the triggering repo/branch (CommitCountsSincePrompt
// before it is reset), not the session-wide totals, so a deferred reminder for one
// repo reports that repo and its count rather than whichever repo committed most
// recently.
func buildCommitReason(req Request, count int, repo string) string {
	detail := fmt.Sprintf("%s reached", countPhrase(count, "commit", "commits"))
	if repoName := quotedLabel(repoDisplayName(repo)); repoName != "" {
		detail += " in " + repoName
	}
	return buildPromptReason(req, detail+".")
}

func buildFailedReviewReason(req Request, st SessionState) string {
	detail := countPhrase(st.FailedReviewCount, "open failed roborev review", "open failed roborev reviews")
	if branch := quotedLabel(st.LastFailedReviewBranch); branch != "" {
		detail += " on " + branch
	} else if repoName := quotedLabel(repoDisplayName(st.LastFailedReviewRepo)); repoName != "" {
		detail += " in " + repoName
	}
	return buildPromptReason(req, detail+".")
}

// sanitizeLabel makes an untrusted git branch or repo (directory) name safe to
// embed in agent-facing hook text. Both are attacker-influenced, so it drops
// control characters and double quotes that could inject new instruction lines
// or break out of delimiting, collapses whitespace, and caps the length so a
// hostile name cannot flood or steer the active agent.
func sanitizeLabel(raw string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == '"' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, raw)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	const maxRunes = 64
	if runes := []rune(cleaned); len(runes) > maxRunes {
		cleaned = strings.TrimSpace(string(runes[:maxRunes]))
	}
	return cleaned
}

// quotedLabel returns raw sanitized and wrapped in quotes so it renders as a
// clearly delimited data token, or "" when nothing usable remains.
func quotedLabel(raw string) string {
	clean := sanitizeLabel(raw)
	if clean == "" {
		return ""
	}
	return fmt.Sprintf("%q", clean)
}

func buildPromptReason(req Request, detail string) string {
	instruction := strings.TrimSpace(req.Instruction)
	if instruction == "" {
		instruction = DefaultInstruction
	}
	if strings.TrimSpace(detail) == "" {
		return instruction
	}
	return instruction + " " + detail
}

func countPhrase(count int, singular, plural string) string {
	if count == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	return fmt.Sprintf("%d %s", count, plural)
}

func repoDisplayName(repoPath string) string {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return ""
	}
	base := filepath.Base(filepath.Clean(repoPath))
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

func currentGitHead(cwd string) (string, string, bool) {
	if cwd == "" {
		return "", "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	root, err := gitrepo.Root(ctx, cwd)
	if err != nil || strings.TrimSpace(root) == "" {
		return "", "", false
	}
	root = strings.TrimSpace(root)
	head, err := gitrepo.Resolve(ctx, root, "HEAD")
	if err != nil || head == "" {
		return "", "", false
	}
	return root, head, true
}

func currentGitBranch(repoRoot string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return gitrepo.CurrentBranch(ctx, repoRoot)
}

// mainRepoRoot resolves the main repository root for daemon API queries,
// following linked worktrees to the path the daemon stores jobs under. The
// daemon canonicalizes jobs to the main root on enqueue but the /api/jobs
// filter matches the path as sent, so a worktree session that queried its own
// checkout root would miss failed reviews recorded for the main repo. The
// checkout root still drives branch and HEAD detection; only the repo filter
// needs the main root. Falls back to worktreeRoot when resolution fails (for
// example a plain checkout, where the two roots are identical).
func mainRepoRoot(worktreeRoot string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if root, err := gitrepo.MainRoot(ctx, worktreeRoot); err == nil {
		if trimmed := strings.TrimSpace(root); trimmed != "" {
			return trimmed
		}
	}
	return worktreeRoot
}

func countNewCommits(repoRoot, oldHead, newHead string) int {
	out, err := gitOutput(repoRoot, "rev-list", "--count", oldHead+".."+newHead)
	if err != nil {
		return 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || n <= 0 {
		return 1
	}
	return n
}

func gitOutput(cwd string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := agentHookGit.Output(ctx, cwd, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func IsCommitProducingCommand(command string) bool {
	_, ok := commitInvocationChdirs(shellFields(command))
	return ok
}

// commitInvocationChdirs scans fields for the first git invocation whose
// subcommand is commit, cherry-pick or revert, returning that invocation's -C
// path arguments (in order) and whether such an invocation exists. It performs no
// filesystem access, keeping IsCommitProducingCommand a pure predicate; commandGitDir
// resolves the paths only for the invocation that produces a commit. Keying both
// off the same invocation aligns them in a chained Bash command:
// `git status && git -C sub commit` yields sub's paths, while
// `git -C sub status && git commit` yields none.
func commitInvocationChdirs(fields []string) ([]string, bool) {
	for i := range fields {
		if !isGitToken(fields[i]) {
			continue
		}
		chdirs, sub := gitInvocation(fields, i)
		if sub < len(fields) && isCommitSubcommand(cleanShellToken(fields[sub])) {
			return chdirs, true
		}
	}
	return nil, false
}

// gitInvocation walks the global options of the git invocation whose git token is
// fields[start], collecting its -C path arguments in order, and returns those
// paths together with the index of the subcommand token (the first non-option
// token), or len(fields) when the invocation has none.
func gitInvocation(fields []string, start int) ([]string, int) {
	var chdirs []string
	j := start + 1
	for j < len(fields) {
		token := cleanShellToken(fields[j])
		switch {
		case token == "-C":
			if j+1 >= len(fields) {
				return chdirs, len(fields)
			}
			chdirs = append(chdirs, cleanShellToken(fields[j+1]))
			j += 2
		case token == "-c" || token == "--git-dir" || token == "--work-tree":
			j += 2 // option takes a separate argument we do not use
		case strings.HasPrefix(token, "--git-dir=") || strings.HasPrefix(token, "--work-tree="):
			j++
		case strings.HasPrefix(token, "-"):
			j++
		default:
			return chdirs, j // first non-option token is the subcommand
		}
	}
	return chdirs, j
}

// commandGitDir returns the working directory the commit-producing git invocation
// in command operates on, honoring that invocation's -C options applied
// cumulatively and relative to cwd, the way git does. In a chained Bash command it
// resolves the same invocation whose subcommand is commit/cherry-pick/revert, not
// merely the first git token. A -C path is used only when it resolves to an
// existing directory: shell expansions such as $(...) or ${VAR}, which the hook
// cannot evaluate, and paths that do not exist fall back to cwd. This keeps repo
// and HEAD tracking pointed at the repository a commit actually lands in - for
// example `git -C ./submodule commit` from a superproject - rather than cwd.
//
// Security: cwd and command arrive in the local agent hook payload, so this path
// is influenced only by the same user the daemon already runs as, and it feeds a
// read-only os.Stat plus read-only `git` reads in directories that user controls -
// never a write or a privileged read. There is no trust boundary to cross, and
// pinning the result under a base directory would defeat the cross-repo/submodule
// resolution above, so the static-analysis path-injection flag on the cwd -> path
// flow is a false positive.
func commandGitDir(cwd, command string) string {
	chdirs, ok := commitInvocationChdirs(shellFields(command))
	if !ok {
		return cwd
	}
	return resolveChdirs(cwd, chdirs)
}

// resolveChdirs folds -C path arguments into a working directory, each resolved
// against the directory established by the previous one, as git applies them.
func resolveChdirs(cwd string, chdirs []string) string {
	dir := cwd
	for _, path := range chdirs {
		dir = existingDir(dir, path)
	}
	return dir
}

// existingDir resolves path against base (absolute paths are used as-is) and
// returns it only when it names an existing directory; otherwise it returns base.
func existingDir(base, path string) string {
	if path == "" {
		return base
	}
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(base, resolved)
	}
	if info, err := os.Stat(resolved); err == nil && info.IsDir() {
		return resolved
	}
	return base
}

func shellFields(command string) []string {
	var fields []string
	var b strings.Builder
	var quote rune
	escaped := false
	expansionDepth := 0
	inToken := false
	pendingExpansion := false
	for _, r := range command {
		if escaped {
			b.WriteRune(r)
			inToken = true
			escaped = false
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				inToken = true
				continue
			}
			if quote != '\'' && r == '\\' {
				escaped = true
				inToken = true
				continue
			}
			b.WriteRune(r)
			inToken = true
			continue
		}
		if pendingExpansion && (r == '(' || r == '{') {
			b.WriteRune(r)
			expansionDepth++
			inToken = true
			pendingExpansion = false
			continue
		}
		pendingExpansion = false
		if expansionDepth > 0 {
			if r == '$' {
				pendingExpansion = true
			}
			if r == ')' || r == '}' {
				expansionDepth--
			}
			b.WriteRune(r)
			inToken = true
			continue
		}
		switch r {
		case '\\':
			escaped = true
			inToken = true
		case '$':
			b.WriteRune(r)
			pendingExpansion = true
			inToken = true
		case '\'', '"', '`':
			quote = r
			inToken = true
		case ' ', '\t', '\r', '\n', ';', '&', '|', '[', ']', '<', '>':
			if inToken {
				fields = append(fields, b.String())
				b.Reset()
				inToken = false
			}
		default:
			b.WriteRune(r)
			inToken = true
		}
	}
	if escaped {
		b.WriteRune('\\')
	}
	if inToken {
		fields = append(fields, b.String())
	}
	return fields
}

func isGitToken(token string) bool {
	token = cleanShellToken(token)
	return token == "git" || strings.HasSuffix(token, "/git")
}

func isCommitSubcommand(token string) bool {
	switch token {
	case "commit", "cherry-pick", "revert":
		return true
	default:
		return false
	}
}

func cleanShellToken(token string) string {
	return strings.Trim(token, " \t\r\n'\"`;$&|(){}[]<>")
}

type jobsResponse struct {
	Jobs []storage.ReviewJob `json:"jobs"`
}

// countsAsFailedReview reports whether job is a review whose F verdict should
// drive the failed-review reminder. Review (single/range/dirty), synthesis, and
// compact jobs produce meaningful P/F verdicts; task, insights, fix, and classify
// jobs do not. A fix job in particular stores a verdict parsed from its own output
// (see storage.DB.CompleteFixJob), so counting it would make the hook keep
// prompting $roborev-fix for a job that is not a failing review. The empty
// job_type is counted for legacy jobs recorded before job_type existed.
func countsAsFailedReview(job storage.ReviewJob) bool {
	switch job.JobType {
	case storage.JobTypeReview, storage.JobTypeRange, storage.JobTypeDirty,
		storage.JobTypeCompact, storage.JobTypeSynthesis, "":
		return true
	default:
		return false
	}
}

func countOpenFailedReviews(ctx context.Context, repoRoot, branch, head, configuredAddr string) (int, bool) {
	if repoRoot == "" {
		return 0, false
	}
	ep, ok := roborevEndpoint(configuredAddr)
	if !ok {
		return 0, false
	}
	client := ep.HTTPClient(2 * time.Second)
	values := url.Values{}
	values.Set("repo", repoRoot)
	if branch != "" {
		values.Set("branch", branch)
		values.Set("branch_include_empty", "true")
	}
	values.Set("status", "done")
	values.Set("closed", "false")
	values.Set("limit", "10000")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.BaseURL()+"/api/jobs?"+values.Encode(), nil)
	if err != nil {
		return 0, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var out jobsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, false
	}
	count := 0
	for _, job := range out.Jobs {
		if job.Status != "" && job.Status != storage.JobStatusDone {
			continue
		}
		if job.Closed != nil && *job.Closed {
			continue
		}
		if !countsAsFailedReview(job) {
			continue
		}
		if !failedReviewCountsForHead(repoRoot, branch, head, job) {
			continue
		}
		if job.Verdict != nil && strings.EqualFold(*job.Verdict, "F") {
			count++
		}
	}
	return count, true
}

// failedReviewCountsForHead reports whether an open failed review returned by
// the jobs query counts toward the current checkout. branch_include_empty makes
// branchful queries also return branchless jobs, so the reachability gate used
// for detached HEAD must apply to those too - otherwise a stale or unrelated
// detached review would prompt $roborev-fix on a branch it does not belong to.
//
//   - A job carrying a branch belongs to the queried branch (the daemon already
//     scoped the query to it).
//   - On detached HEAD, only branchless reviews reachable from HEAD are ours.
//   - On a branch, a branchless review counts unless it pins a concrete ref that
//     is unreachable from HEAD; reviews with no ref (repo-level or dirty) still
//     count, matching the long-standing reminder behavior.
func failedReviewCountsForHead(repoRoot, branch, head string, job storage.ReviewJob) bool {
	if strings.TrimSpace(job.Branch) != "" {
		return branch != ""
	}
	if branch == "" {
		return head != "" && detachedReviewMatches(repoRoot, head, job)
	}
	ref := strings.TrimSpace(job.GitRef)
	if ref == "" || ref == "dirty" || head == "" {
		return true
	}
	return detachedReviewMatches(repoRoot, head, job)
}

func detachedReviewMatches(repoRoot, head string, job storage.ReviewJob) bool {
	if strings.TrimSpace(job.Branch) != "" {
		return false
	}
	ref := strings.TrimSpace(job.GitRef)
	if ref == "" || ref == "dirty" {
		return false
	}
	if ref == head {
		return true
	}
	if _, end, ok := roborevgit.ParseRange(ref); ok {
		return refReachableFromHead(repoRoot, strings.TrimSpace(end), head)
	}
	return refReachableFromHead(repoRoot, ref, head)
}

func refReachableFromHead(repoRoot, ref, head string) bool {
	if ref == "" || head == "" {
		return false
	}
	if ref == head {
		return true
	}
	ok, err := roborevgit.IsAncestor(repoRoot, ref, head)
	return err == nil && ok
}

func roborevEndpoint(configuredAddr string) (roborevdaemon.DaemonEndpoint, bool) {
	if configuredAddr != "" {
		ep, err := roborevdaemon.ParseEndpoint(configuredAddr)
		return ep, err == nil
	}
	info, err := roborevdaemon.GetAnyRunningDaemon()
	if err != nil {
		return roborevdaemon.DaemonEndpoint{}, false
	}
	return info.Endpoint(), true
}
