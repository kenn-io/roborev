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
	case "", "Stop":
		return s.recordStop(req)
	case "PostToolUse":
		return s.recordPostToolUse(req)
	default:
		return Response{SessionID: req.Event.SessionID, Skipped: true}, nil
	}
}

func (s *StateStore) recordStop(req Request) (Response, error) {
	repoRoot, _, ok := currentGitHead(req.Event.CWD)
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
		context.Background(), mainRepoRoot(repoRoot), branch, req.RoborevServerAddr,
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
		context.Background(), mainRepoRoot(repoRoot), branch, req.RoborevServerAddr,
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
	increment := 0
	if commitCommand {
		switch {
		case previousHead != "" && previousHead != head:
			increment = countNewCommits(repoRoot, previousHead, head)
		case previousHead == "" && latestReflogLooksLikeCommit(repoRoot):
			increment = 1
		}
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
	instruction := strings.TrimSpace(req.Instruction)
	if instruction == "" {
		instruction = DefaultInstruction
	}
	var b strings.Builder
	b.WriteString(instruction)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "roborev agent-hook counted %d Stop hooks for session %s.", st.Count, req.Event.SessionID)
	if req.Event.CWD != "" {
		fmt.Fprintf(&b, " Current working directory: %s.", req.Event.CWD)
	}
	return b.String()
}

func buildCommitReason(req Request, st SessionState) string {
	instruction := strings.TrimSpace(req.Instruction)
	if instruction == "" {
		instruction = DefaultInstruction
	}
	var b strings.Builder
	b.WriteString(instruction)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "roborev agent-hook counted %d commits for session %s.", st.CommitCount, req.Event.SessionID)
	if st.LastCommitRepo != "" {
		fmt.Fprintf(&b, " Last commit repository: %s.", st.LastCommitRepo)
	}
	if req.Event.CWD != "" {
		fmt.Fprintf(&b, " Current working directory: %s.", req.Event.CWD)
	}
	return b.String()
}

func buildFailedReviewReason(req Request, st SessionState) string {
	instruction := strings.TrimSpace(req.Instruction)
	if instruction == "" {
		instruction = DefaultInstruction
	}
	var b strings.Builder
	b.WriteString(instruction)
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "roborev agent-hook counted %d non-closed failed roborev reviews for session %s.", st.FailedReviewCount, req.Event.SessionID)
	if st.LastFailedReviewRepo != "" {
		fmt.Fprintf(&b, " Review repository: %s.", st.LastFailedReviewRepo)
	}
	if st.LastFailedReviewBranch != "" {
		fmt.Fprintf(&b, " Review branch: %s.", st.LastFailedReviewBranch)
	}
	if req.Event.CWD != "" {
		fmt.Fprintf(&b, " Current working directory: %s.", req.Event.CWD)
	}
	return b.String()
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

func latestReflogLooksLikeCommit(repoRoot string) bool {
	out, err := gitOutput(repoRoot, "reflog", "-1", "--format=%gs")
	if err != nil {
		return false
	}
	action := strings.ToLower(strings.TrimSpace(out))
	return strings.HasPrefix(action, "commit") ||
		strings.HasPrefix(action, "cherry-pick") ||
		strings.HasPrefix(action, "revert")
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
	fields := strings.Fields(command)
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

func countOpenFailedReviews(ctx context.Context, repoRoot, branch, configuredAddr string) (int, bool) {
	if repoRoot == "" || branch == "" {
		return 0, false
	}
	ep, ok := roborevEndpoint(configuredAddr)
	if !ok {
		return 0, false
	}
	client := ep.HTTPClient(2 * time.Second)
	values := url.Values{}
	values.Set("repo", repoRoot)
	values.Set("branch", branch)
	values.Set("branch_include_empty", "true")
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
		if job.Verdict != nil && strings.EqualFold(*job.Verdict, "F") {
			count++
		}
	}
	return count, true
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
