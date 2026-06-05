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
		resetPromptCounters(&st)
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

	repoRoot, head, ok := currentGitHead(req.Event.CWD)
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

	repoRoot, head, ok := currentGitHead(req.Event.CWD)
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
	command := req.Event.Command()
	commitCommand := IsCommitProducingCommand(command)

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
		st.CommitCountSincePrompt += increment
		st.LastCommitRepo = repoRoot
		st.LastCommitHead = head
	}

	actionableReviews := hasActionableFailedReviews(failedReviewCount, haveFailedReviewCount)
	commitTriggered := thresholdReady(st.CommitCountSincePrompt, req.CommitThreshold) && increment > 0 && actionableReviews
	if commitTriggered {
		st.CommitTriggeredAt = now
	}
	failedReviewTriggered := applyFailedReviewTrigger(req, &st, repoRoot, branch, failedReviewCount, haveFailedReviewCount, now)
	promptTriggered := commitTriggered || failedReviewTriggered
	if promptTriggered {
		st.ReminderPromptCount++
		resetPromptCounters(&st)
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
		resp.Reason = buildCommitReason(req, st)
	}
	return resp, nil
}

func hasActionableFailedReviews(count int, ok bool) bool {
	return ok && count > 0
}

func thresholdReady(countSincePrompt, threshold int) bool {
	return threshold > 0 && countSincePrompt >= threshold
}

func resetPromptCounters(st *SessionState) {
	st.StopCountSincePrompt = 0
	st.CommitCountSincePrompt = 0
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
	if count < req.FailedReviewThreshold {
		st.FailedReviewTriggeredCount = 0
		return false
	}
	if !thresholdReady(count-st.FailedReviewTriggeredCount, req.FailedReviewThreshold) {
		return false
	}
	st.FailedReviewTriggeredCount = count
	st.FailedReviewTriggeredAt = now
	return true
}

func buildStopReason(req Request, st SessionState) string {
	return buildPromptReason(req, fmt.Sprintf("%s reached.", countPhrase(st.Count, "Stop hook", "Stop hooks")))
}

func buildCommitReason(req Request, st SessionState) string {
	detail := fmt.Sprintf("%s reached", countPhrase(st.CommitCount, "commit", "commits"))
	if repoName := repoDisplayName(st.LastCommitRepo); repoName != "" {
		detail += " in " + repoName
	}
	return buildPromptReason(req, detail+".")
}

func buildFailedReviewReason(req Request, st SessionState) string {
	detail := countPhrase(st.FailedReviewCount, "open failed roborev review", "open failed roborev reviews")
	if branch := strings.TrimSpace(st.LastFailedReviewBranch); branch != "" {
		detail += " on " + branch
	} else if repoName := repoDisplayName(st.LastFailedReviewRepo); repoName != "" {
		detail += " in " + repoName
	}
	return buildPromptReason(req, detail+".")
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
	fields := shellFields(command)
	for i := range len(fields) {
		if !isGitToken(fields[i]) {
			continue
		}
		j := i + 1
		for j < len(fields) {
			token := cleanShellToken(fields[j])
			if token == "-c" {
				j += 2
				continue
			}
			if token == "-C" || token == "--git-dir" || token == "--work-tree" {
				j += 2
				continue
			}
			if strings.HasPrefix(token, "--git-dir=") || strings.HasPrefix(token, "--work-tree=") {
				j++
				continue
			}
			if strings.HasPrefix(token, "-") {
				j++
				continue
			}
			break
		}
		if j < len(fields) && isCommitSubcommand(cleanShellToken(fields[j])) {
			return true
		}
	}
	return false
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
