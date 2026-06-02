package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	daemonclient "go.kenn.io/roborev/internal/daemon_client"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
	"go.kenn.io/roborev/internal/update"
)

func (m model) tick() tea.Cmd {
	return tea.Tick(m.tickInterval(), func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) displayTick() tea.Cmd {
	return tea.Tick(displayTickInterval, func(time.Time) tea.Msg {
		return displayTickMsg{}
	})
}

// tickInterval returns the polling interval. Now that SSE handles real-time
// updates, polling is only a fallback for missed events or disconnections.
func (m model) tickInterval() time.Duration {
	return tickIntervalFallback
}

type jobsPageResult struct {
	Jobs    []storage.ReviewJob `json:"jobs"`
	HasMore bool                `json:"has_more"`
	Stats   storage.JobStats    `json:"stats"`
}

type repoListResult struct {
	Repos []struct {
		Name     string `json:"name"`
		RootPath string `json:"root_path"`
		Identity string `json:"identity"`
		Count    int    `json:"count"`
	} `json:"repos"`
	TotalCount int `json:"total_count"`
}

type branchListResult struct {
	Branches []struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	} `json:"branches"`
	TotalCount     int `json:"total_count"`
	NullsRemaining int `json:"nulls_remaining"`
}

func (m model) loadJobsPage(params neturl.Values) (*jobsPageResult, error) {
	apiParams := listJobsParams(params)
	resp, err := m.api.ListJobsWithResponse(m.apiContext(), &apiParams)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, apiStatusError(resp.StatusCode(), resp.Status(), resp.Body)
	}
	var result jobsPageResult
	if err := decodeAPIBody(resp.Body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (m model) loadRepoList(branchFilter string) (*repoListResult, bool, error) {
	branchFiltered := branchFilter != "" && branchFilter != branchNone
	var params daemonclient.ListReposParams
	if branchFiltered {
		params.Branch = &branchFilter
	}
	resp, err := m.api.ListReposWithResponse(m.apiContext(), &params)
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, false, apiStatusError(resp.StatusCode(), resp.Status(), resp.Body)
	}
	var result repoListResult
	if err := decodeAPIBody(resp.Body, &result); err != nil {
		return nil, false, err
	}
	return &result, branchFiltered, nil
}

func (m model) loadBranchList(rootPaths []string) (*branchListResult, error) {
	var params daemonclient.ListBranchesParams
	if len(rootPaths) > 0 {
		repos := append([]string(nil), rootPaths...)
		params.Repo = &repos
	}
	resp, err := m.api.ListBranchesWithResponse(m.apiContext(), &params)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, apiStatusError(resp.StatusCode(), resp.Status(), resp.Body)
	}
	var result branchListResult
	if err := decodeAPIBody(resp.Body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func listJobsParams(values neturl.Values) daemonclient.ListJobsParams {
	params := daemonclient.ListJobsParams{}
	setStringParam := func(key string, dst **string) {
		if value := values.Get(key); value != "" {
			*dst = &value
		}
	}
	setIntParam := func(key string, dst **int64) {
		if value := values.Get(key); value != "" {
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				*dst = &parsed
			}
		}
	}

	setIntParam("id", &params.Id)
	setStringParam("status", &params.Status)
	setStringParam("repo", &params.Repo)
	setStringParam("git_ref", &params.GitRef)
	setStringParam("branch", &params.Branch)
	if value := values.Get("branch_include_empty"); value != "" {
		typed := daemonclient.ListJobsParamsBranchIncludeEmpty(value)
		params.BranchIncludeEmpty = &typed
	}
	if value := values.Get("closed"); value != "" {
		typed := daemonclient.ListJobsParamsClosed(value)
		params.Closed = &typed
	}
	setStringParam("job_type", &params.JobType)
	setStringParam("exclude_job_type", &params.ExcludeJobType)
	if value := values.Get("hide_classify_jobs"); value != "" {
		typed := daemonclient.ListJobsParamsHideClassifyJobs(value)
		params.HideClassifyJobs = &typed
	}
	setStringParam("repo_prefix", &params.RepoPrefix)
	setIntParam("limit", &params.Limit)
	setIntParam("offset", &params.Offset)
	setIntParam("before", &params.Before)
	return params
}

func (m model) fetchJobs() tea.Cmd {
	// Fetch enough to fill the visible area plus a buffer for smooth scrolling.
	// Use minimum of 100 only before first WindowSizeMsg (when height is default 24)
	visibleRows := m.queueVisibleRows() + queuePrefetchBuffer
	if !m.heightDetected {
		visibleRows = max(100, visibleRows)
	}
	currentJobCount := len(m.jobs)
	seq := m.fetchSeq

	return func() tea.Msg {
		// Build URL with server-side filters where possible, falling back to
		// limit=0 (no pagination) only when client-side filtering is required.
		params := neturl.Values{}

		// Repo filter: single repo can use API filter; multiple repos need client-side
		needsAllJobs := false
		if len(m.activeRepoFilter) == 1 {
			params.Set("repo", m.activeRepoFilter[0])
		} else if len(m.activeRepoFilter) > 1 {
			needsAllJobs = true // Multiple repos (shared display name) - filter client-side
		}

		// Branch filter: use server-side for real branch names.
		// branchNone is a client-side sentinel for empty/NULL branches and can't be
		// sent to the server, so it falls through to client-side filtering.
		if m.activeBranchFilter != "" && m.activeBranchFilter != branchNone {
			params.Set("branch", m.activeBranchFilter)
		} else if m.activeBranchFilter == branchNone {
			needsAllJobs = true
		}

		// Closed filter: use server-side to avoid fetching all jobs.
		// Skip for client-side filtered views (needsAllJobs) so we get
		// all jobs for accurate client-side metrics counting.
		if m.hideClosed && !needsAllJobs {
			params.Set("closed", "false")
		}

		// Exclude fix jobs — they belong in the Tasks view, not the queue
		params.Set("exclude_job_type", "fix")

		// Hide auto-design-router byproducts (classify rows + skipped design
		// rows) unless the user opted in via show_classify_jobs. Resolved at
		// fetch time so single-repo filters honor that repo's override.
		if !m.shouldShowClassifyJobs() {
			params.Set("hide_classify_jobs", "true")
		}

		// Set limit: use pagination unless we need client-side filtering (multi-repo)
		if needsAllJobs {
			params.Set("limit", "0")
		} else {
			limit := max(currentJobCount,
				// Maintain paginated view on refresh
				visibleRows)
			params.Set("limit", fmt.Sprintf("%d", limit))
		}

		result, err := m.loadJobsPage(params)
		if err != nil {
			return jobsErrMsg{
				err: fmt.Errorf("fetch jobs: %w", err),
				seq: seq,
			}
		}
		return jobsMsg{jobs: result.Jobs, hasMore: result.HasMore, append: false, seq: seq, stats: result.Stats}
	}
}

func (m model) fetchMoreJobs() tea.Cmd {
	seq := m.fetchSeq
	return func() tea.Msg {
		// Only fetch more when not doing client-side filtering that loads all jobs
		if len(m.activeRepoFilter) > 1 || m.activeBranchFilter == branchNone {
			return nil // Multi-repo or "(none)" branch filter loads everything
		}
		offset := len(m.jobs)
		params := neturl.Values{}
		params.Set("limit", "50")
		params.Set("offset", fmt.Sprintf("%d", offset))
		if len(m.activeRepoFilter) == 1 {
			params.Set("repo", m.activeRepoFilter[0])
		}
		if m.activeBranchFilter != "" && m.activeBranchFilter != branchNone {
			params.Set("branch", m.activeBranchFilter)
		}
		if m.hideClosed {
			params.Set("closed", "false")
		}
		params.Set("exclude_job_type", "fix")
		if !m.shouldShowClassifyJobs() {
			params.Set("hide_classify_jobs", "true")
		}
		result, err := m.loadJobsPage(params)
		if err != nil {
			return paginationErrMsg{
				err: fmt.Errorf("fetch more jobs: %w", err),
				seq: seq,
			}
		}
		return jobsMsg{jobs: result.Jobs, hasMore: result.HasMore, append: true, seq: seq}
	}
}

func (m model) fetchStatus() tea.Cmd {
	gen := m.fetchGen
	return func() tea.Msg {
		resp, err := m.api.GetStatusWithResponse(m.apiContext())
		if err != nil {
			return statusErrMsg{err: err, gen: gen}
		}
		if resp.StatusCode() != http.StatusOK {
			return statusErrMsg{
				err: apiStatusError(resp.StatusCode(), resp.Status(), resp.Body),
				gen: gen,
			}
		}
		var status storage.DaemonStatus
		if err := decodeAPIBody(resp.Body, &status); err != nil {
			return statusErrMsg{err: err, gen: gen}
		}
		return statusMsg{status: status, gen: gen}
	}
}

// startFetchStatus dispatches fetchStatus if no status fetch is already
// in flight, and sets the loadingStatus flag. Returns nil if skipped.
func (m *model) startFetchStatus() tea.Cmd {
	if m.loadingStatus {
		return nil
	}
	m.loadingStatus = true
	return m.fetchStatus()
}

// requestFetchStatus is like startFetchStatus but for paths triggered by
// daemon state changes (SSE events). If a fetch is already in flight, it
// marks the current data as stale so handleStatusMsg will dispatch a
// follow-up fetch when the in-flight one returns.
func (m *model) requestFetchStatus() tea.Cmd {
	if m.loadingStatus {
		m.statusStale = true
		return nil
	}
	m.loadingStatus = true
	return m.fetchStatus()
}

func (m model) checkForUpdate() tea.Cmd {
	return func() tea.Msg {
		info, err := update.CheckForUpdate(false) // Use cache
		if err != nil || info == nil {
			return updateCheckMsg{} // No update or error
		}
		return updateCheckMsg{version: info.LatestVersion, isDevBuild: info.IsDevBuild}
	}
}

// tryReconnect attempts to find a running daemon at a new address.
// This is called after consecutive connection failures to handle daemon restarts.
func (m model) tryReconnect() tea.Cmd {
	return func() tea.Msg {
		info, err := daemon.GetAnyRunningDaemon()
		if err != nil {
			return reconnectMsg{err: err}
		}
		return reconnectMsg{endpoint: info.Endpoint(), version: info.Version}
	}
}

// fetchRepoNames fetches the unfiltered repo list and builds a
// display-name-to-root-paths mapping for control socket resolution.
func (m model) fetchRepoNames() tea.Cmd {
	return func() tea.Msg {
		result, _, err := m.loadRepoList("")
		if err != nil {
			return repoNamesMsg{} // non-fatal; map stays nil
		}

		names := make(map[string][]string)
		identities := make(map[string][]string)
		for _, r := range result.Repos {
			displayName := config.GetDisplayName(r.RootPath)
			if displayName == "" {
				displayName = r.Name
			}
			names[displayName] = append(names[displayName], r.RootPath)
			if r.Identity != "" {
				identities[r.Identity] = append(identities[r.Identity], r.RootPath)
			}
		}
		return repoNamesMsg{names: names, identities: identities}
	}
}

func (m model) fetchRepos() tea.Cmd {
	activeBranchFilter := m.activeBranchFilter // Constrain repos by active branch filter

	return func() tea.Msg {
		reposResult, filtered, err := m.loadRepoList(activeBranchFilter)
		if err != nil {
			return errMsg(fmt.Errorf("fetch repos: %w", err))
		}

		// Aggregate repos by display name
		displayNameMap := make(map[string]*repoFilterItem)
		identities := make(map[string][]string)
		var displayNameOrder []string // Preserve order for stable display
		for _, r := range reposResult.Repos {
			displayName := config.GetDisplayName(r.RootPath)
			if displayName == "" {
				displayName = r.Name
			}
			if r.Identity != "" {
				identities[r.Identity] = append(identities[r.Identity], r.RootPath)
			}
			if item, ok := displayNameMap[displayName]; ok {
				item.rootPaths = append(item.rootPaths, r.RootPath)
				item.count += r.Count
			} else {
				displayNameMap[displayName] = &repoFilterItem{
					name:      displayName,
					rootPaths: []string{r.RootPath},
					count:     r.Count,
				}
				displayNameOrder = append(displayNameOrder, displayName)
			}
		}
		repos := make([]repoFilterItem, len(displayNameOrder))
		for i, name := range displayNameOrder {
			repos[i] = *displayNameMap[name]
		}
		return reposMsg{repos: repos, identities: identities, branchFiltered: filtered}
	}
}

// fetchBranchesForRepo fetches branches for a specific repo in the tree filter.
// Returns repoBranchesMsg with the branch data (or err set on failure).
// When expand is true, the handler sets expanded=true on the tree node.
// searchSeq is the search generation at dispatch time; the error handler
// uses it to avoid marking fetchFailed for stale search sessions.
func (m model) fetchBranchesForRepo(
	rootPaths []string, repoIdx int, expand bool, searchSeq int,
) tea.Cmd {
	errMsg := func(err error) repoBranchesMsg {
		return repoBranchesMsg{
			repoIdx:      repoIdx,
			rootPaths:    rootPaths,
			err:          err,
			expandOnLoad: expand,
			searchSeq:    searchSeq,
		}
	}

	return func() tea.Msg {
		branchResult, err := m.loadBranchList(rootPaths)
		if err != nil {
			return errMsg(fmt.Errorf("fetch branches for repo: %w", err))
		}

		branches := make([]branchFilterItem, len(branchResult.Branches))
		for i, b := range branchResult.Branches {
			branches[i] = branchFilterItem{
				name:  b.Name,
				count: b.Count,
			}
		}

		return repoBranchesMsg{
			repoIdx:      repoIdx,
			rootPaths:    rootPaths,
			branches:     branches,
			expandOnLoad: expand,
			searchSeq:    searchSeq,
		}
	}
}

func (m model) backfillBranches() tea.Cmd {
	// Capture values for use in goroutine
	machineID := m.status.MachineID

	return func() tea.Msg {
		var backfillCount int

		checkResult, err := m.loadBranchList(nil)
		if err != nil {
			return errMsg(fmt.Errorf("check branches for backfill: %w", err))
		}

		// If there are NULL branches, fetch all jobs to backfill
		if checkResult.NullsRemaining > 0 {
			jobsResult, err := m.loadJobsPage(nil)
			if err != nil {
				return errMsg(fmt.Errorf("fetch jobs for backfill: %w", err))
			}

			// Find jobs that need backfill
			type backfillJob struct {
				id     int64
				branch string
			}
			var toBackfill []backfillJob

			for _, job := range jobsResult.Jobs {
				if job.Branch != "" {
					continue // Already has branch
				}
				// Mark task jobs (run, analyze, custom) or dirty jobs with no-branch sentinel
				if job.IsTaskJob() || job.IsDirtyJob() {
					toBackfill = append(toBackfill, backfillJob{id: job.ID, branch: branchNone})
					continue
				}
				// Mark remote jobs with no-branch sentinel (can't look up)
				if job.RepoPath == "" || (machineID != "" && job.SourceMachineID != "" && job.SourceMachineID != machineID) {
					toBackfill = append(toBackfill, backfillJob{id: job.ID, branch: branchNone})
					continue
				}

				sha := job.GitRef
				if idx := strings.Index(sha, ".."); idx != -1 {
					sha = sha[idx+2:]
				}
				branch := git.GetBranchName(job.RepoPath, sha)
				if branch == "" {
					branch = branchNone // Mark as attempted but not found
				}
				toBackfill = append(toBackfill, backfillJob{id: job.ID, branch: branch})
			}

			for _, bf := range toBackfill {
				resp, err := m.api.UpdateJobBranchWithResponse(m.apiContext(), daemonclient.UpdateJobBranchRequest{
					JobId:  bf.id,
					Branch: bf.branch,
				})
				if err == nil && resp.StatusCode() == http.StatusOK {
					var updateResult struct {
						Updated bool `json:"updated"`
					}
					if decodeAPIBody(resp.Body, &updateResult) == nil && updateResult.Updated {
						backfillCount++
					}
				}
			}
		}

		return branchesMsg{backfillCount: backfillCount}
	}
}

// loadReview fetches a review from the server by job ID.
// Used by fetchReview, fetchReviewForPrompt, and fetchReviewAndCopy.
func (m model) loadReview(jobID int64) (*storage.Review, error) {
	resp, err := m.api.GetReviewWithResponse(
		m.apiContext(),
		&daemonclient.GetReviewParams{JobId: &jobID},
	)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != http.StatusOK {
		err := apiStatusError(resp.StatusCode(), resp.Status(), resp.Body)
		if errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("no review found")
		}
		return nil, fmt.Errorf("fetch review: %w", err)
	}
	var review storage.Review
	if err := decodeAPIBody(resp.Body, &review); err != nil {
		return nil, fmt.Errorf("fetch review: %w", err)
	}
	return &review, nil
}

// loadResponses fetches responses for a job, merging legacy SHA-based
// responses via storage.MergeResponses.
func (m model) loadResponses(jobID int64, review *storage.Review) []storage.Response {
	var responses []storage.Response

	// Fetch responses by job ID
	var jobResult struct {
		Responses []storage.Response `json:"responses"`
	}
	if err := m.loadComments(
		&daemonclient.ListCommentsParams{JobId: &jobID},
		&jobResult,
	); err == nil {
		responses = jobResult.Responses
	}

	// Also fetch legacy commit-based responses and merge.
	// Prefer commit_id (unambiguous), fall back to SHA for legacy jobs.
	var legacyParams *daemonclient.ListCommentsParams
	if review.Job != nil {
		commitID, fallbackSHA := review.Job.LegacyCommentLookupTarget()
		if commitID > 0 {
			legacyParams = &daemonclient.ListCommentsParams{CommitId: &commitID}
		} else if fallbackSHA != "" {
			legacyParams = &daemonclient.ListCommentsParams{Sha: &fallbackSHA}
		}
	}
	if legacyParams != nil {
		var legacyResult struct {
			Responses []storage.Response `json:"responses"`
		}
		if err := m.loadComments(legacyParams, &legacyResult); err == nil {
			responses = storage.MergeResponses(responses, legacyResult.Responses)
		}
	}

	return responses
}

func (m model) loadPatch(jobID int64) (string, error) {
	jobIDParam := strconv.FormatInt(jobID, 10)
	resp, err := m.api.GetJobPatchWithResponse(
		m.apiContext(),
		&daemonclient.GetJobPatchParams{JobId: &jobIDParam},
	)
	if err != nil {
		return "", err
	}
	if resp.StatusCode() != http.StatusOK {
		err := apiStatusError(resp.StatusCode(), resp.Status(), resp.Body)
		if errors.Is(err, errNotFound) {
			return "", fmt.Errorf("no patch available")
		}
		return "", fmt.Errorf("fetch patch: %w", err)
	}
	patch := string(resp.Body)
	if patch == "" {
		return "", fmt.Errorf("empty patch")
	}
	return patch, nil
}

func (m model) loadComments(
	params *daemonclient.ListCommentsParams,
	out any,
) error {
	resp, err := m.api.ListCommentsWithResponse(m.apiContext(), params)
	if err != nil {
		return err
	}
	if resp.StatusCode() != http.StatusOK {
		return apiStatusError(resp.StatusCode(), resp.Status(), resp.Body)
	}
	return decodeAPIBody(resp.Body, out)
}

func (m model) loadJob(jobID int64) (*storage.ReviewJob, error) {
	params := neturl.Values{}
	params.Set("id", fmt.Sprintf("%d", jobID))

	result, err := m.loadJobsPage(params)
	if err != nil {
		return nil, fmt.Errorf("fetch job: %w", err)
	}
	for i := range result.Jobs {
		if result.Jobs[i].ID == jobID {
			return &result.Jobs[i], nil
		}
	}
	return nil, fmt.Errorf("job %d not found", jobID)
}

func (m model) fetchReview(jobID int64) tea.Cmd {
	return func() tea.Msg {
		review, err := m.loadReview(jobID)
		if err != nil {
			return errMsg(err)
		}

		responses := m.loadResponses(jobID, review)

		branchName := reviewBranchName(review.Job)

		return reviewMsg{review: review, responses: responses, jobID: jobID, branchName: branchName}
	}
}

// reviewBranchName returns the branch to display on the review screen.
// It prefers the stored job.Branch (set at enqueue time) over a dynamic
// git name-rev lookup, which can be misled by worktree branches
// reachable from the same SHA. Falls back to git lookup only for
// single-commit reviews when the stored branch is empty.
func reviewBranchName(job *storage.ReviewJob) string {
	if job == nil {
		return ""
	}
	if job.Branch == branchNone {
		return ""
	}
	if job.Branch != "" {
		return job.Branch
	}
	if job.RepoPath != "" && !strings.Contains(job.GitRef, "..") {
		return git.GetBranchName(job.RepoPath, job.GitRef)
	}
	return ""
}

func (m model) fetchReviewForPrompt(jobID int64) tea.Cmd {
	return func() tea.Msg {
		review, err := m.loadReview(jobID)
		if err != nil {
			return errMsg(err)
		}
		return promptMsg{review: review, jobID: jobID}
	}
}

// fetchPanelMembers loads a panel run's member rows via GET /api/jobs?panel_run.
// The generated client has no panel_run param, so this issues a raw request like
// fetchJobLog (and show.go's fetchPanelMembers). The endpoint returns the full
// run (members + synthesis); keep only members, sorted by member index. On error
// the msg carries err and the handler leaves the panel uncached so a later
// expand retries.
func (m model) fetchPanelMembers(runUUID string) tea.Cmd {
	baseURL := m.endpoint.BaseURL()
	client := m.client
	return func() tea.Msg {
		url := fmt.Sprintf("%s/api/jobs?panel_run=%s&limit=0", baseURL, neturl.QueryEscape(runUUID))
		resp, err := client.Get(url)
		if err != nil {
			return panelMembersMsg{runUUID: runUUID, err: err}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return panelMembersMsg{runUUID: runUUID, err: fmt.Errorf("list panel members: %s", resp.Status)}
		}
		var result struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return panelMembersMsg{runUUID: runUUID, err: err}
		}
		members := make([]storage.ReviewJob, 0, len(result.Jobs))
		for _, j := range result.Jobs {
			if j.PanelRole == storage.PanelRoleMember {
				members = append(members, j)
			}
		}
		sort.Slice(members, func(i, j int) bool {
			return members[i].PanelMemberIndex < members[j].PanelMemberIndex
		})
		return panelMembersMsg{runUUID: runUUID, members: members}
	}
}

// fetchJobLog fetches raw JSONL from /api/job/log, renders it
// through streamFormatter, and returns pre-styled logLines.
// Uses incremental fetching: only new bytes since logOffset are
// downloaded and rendered, reusing the persistent logFmtr state.
func (m model) fetchJobLog(jobID int64) tea.Cmd {
	baseURL := m.endpoint.BaseURL()
	width := m.width
	client := m.client
	style := m.glamourStyle
	offset := m.logOffset
	fmtr := m.logFmtr
	seq := m.logFetchSeq
	return func() tea.Msg {
		url := fmt.Sprintf(
			"%s/api/job/log?job_id=%d&offset=%d",
			baseURL, jobID, offset,
		)
		resp, err := client.Get(url)
		if err != nil {
			return logOutputMsg{err: err, seq: seq}
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return logOutputMsg{err: errNoLog, seq: seq}
		}
		if resp.StatusCode != http.StatusOK {
			return logOutputMsg{
				err: fmt.Errorf("fetch log: %s", resp.Status),
				seq: seq,
			}
		}

		// Determine if job is still running from header
		jobStatus := resp.Header.Get("X-Job-Status")
		hasMore := jobStatus == "running"

		// Parse new offset from response header
		newOffset := offset
		if v := resp.Header.Get("X-Log-Offset"); v != "" {
			if parsed, perr := strconv.ParseInt(
				v, 10, 64,
			); perr == nil {
				newOffset = parsed
			}
		}

		// Server reset offset (log truncated/rotated) — force
		// full replace even if we sent a nonzero offset.
		isIncremental := offset > 0 && fmtr != nil
		if newOffset < offset {
			isIncremental = false
		}

		// No new data — return early with current state
		if newOffset == offset && isIncremental {
			return logOutputMsg{
				hasMore:   hasMore,
				newOffset: newOffset,
				append:    true,
				seq:       seq,
			}
		}

		// Render JSONL through streamFormatter. Use pre-computed
		// glamour style to avoid terminal queries from goroutine.
		var buf bytes.Buffer
		var renderFmtr *streamfmt.Formatter
		if isIncremental {
			// Reuse persistent formatter — redirect its output
			// to a fresh buffer for this batch only.
			fmtr.SetWriter(&buf)
			renderFmtr = fmtr
		} else {
			renderFmtr = streamfmt.NewWithWidth(
				&buf, width, style,
			)
		}

		if err := streamfmt.RenderLogWith(
			resp.Body, renderFmtr, &buf,
		); err != nil {
			return logOutputMsg{err: err, seq: seq}
		}

		// Split rendered output into lines
		raw := buf.String()
		var lines []logLine
		if raw != "" {
			for s := range strings.SplitSeq(raw, "\n") {
				lines = append(lines, logLine{text: s})
			}
			// Remove trailing empty line from final newline
			if len(lines) > 0 &&
				lines[len(lines)-1].text == "" {
				lines = lines[:len(lines)-1]
			}
		}

		return logOutputMsg{
			lines:     lines,
			hasMore:   hasMore,
			newOffset: newOffset,
			append:    isIncremental,
			seq:       seq,
			fmtr:      renderFmtr,
		}
	}
}

func (m model) fetchReviewAndCopy(jobID int64, job *storage.ReviewJob) tea.Cmd {
	view := m.currentView // Capture view at trigger time
	return func() tea.Msg {
		review, err := m.loadReview(jobID)
		if err != nil {
			return clipboardResultMsg{err: err, view: view}
		}

		if review.Output == "" {
			return clipboardResultMsg{err: fmt.Errorf("review has no content"), view: view}
		}

		// Attach job info if not already present (for header formatting)
		if review.Job == nil && job != nil {
			review.Job = job
		}

		responses := m.loadResponses(jobID, review)

		content := formatClipboardContent(review, responses)
		err = m.clipboard.WriteText(content)
		return clipboardResultMsg{err: err, view: view}
	}
}

// fetchCommitMsg fetches commit message(s) for a job.
// For single commits, returns the commit message.
// For ranges, returns all commit messages in the range.
// For dirty reviews or prompt jobs, returns an error.
func (m model) fetchCommitMsg(job *storage.ReviewJob) tea.Cmd {
	jobID := job.ID
	return func() tea.Msg {
		// Handle task jobs first (run, analyze, custom labels)
		// Check this before dirty to handle backward compatibility with older run jobs
		if job.IsTaskJob() {
			return commitMsgMsg{
				jobID: jobID,
				err:   fmt.Errorf("no commit message for task jobs"),
			}
		}

		// Handle dirty reviews (uncommitted changes)
		if job.DiffContent != nil || job.IsDirtyJob() {
			return commitMsgMsg{
				jobID: jobID,
				err:   fmt.Errorf("no commit message for uncommitted changes"),
			}
		}

		// Handle missing GitRef (could be from incomplete job data or older versions)
		if job.GitRef == "" {
			return commitMsgMsg{
				jobID: jobID,
				err:   fmt.Errorf("no git reference available for this job"),
			}
		}

		// Check if this is a range (contains "..")
		if strings.Contains(job.GitRef, "..") {
			// Fetch all commits in range
			commits, err := git.GetRangeCommits(job.RepoPath, job.GitRef)
			if err != nil {
				return commitMsgMsg{jobID: jobID, err: err}
			}
			if len(commits) == 0 {
				return commitMsgMsg{
					jobID: jobID,
					err:   fmt.Errorf("no commits in range %s", job.GitRef),
				}
			}

			// Fetch info for each commit
			var content strings.Builder
			fmt.Fprintf(&content, "Commits in %s (%d commits):\n\n", job.GitRef, len(commits))

			for i, sha := range commits {
				info, err := git.GetCommitInfo(job.RepoPath, sha)
				if err != nil {
					fmt.Fprintf(&content, "%d. %s: (error: %v)\n\n", i+1, gitrepo.ShortSHA(sha), err)
					continue
				}
				fmt.Fprintf(&content, "%d. %s %s\n", i+1, gitrepo.ShortSHA(info.SHA), info.Subject)
				fmt.Fprintf(&content, "   Author: %s | %s\n", info.Author, info.Timestamp.Format("2006-01-02 15:04"))
				if info.Body != "" {
					// Indent body
					bodyLines := strings.SplitSeq(info.Body, "\n")
					for line := range bodyLines {
						content.WriteString("   " + line + "\n")
					}
				}
				content.WriteString("\n")
			}

			return commitMsgMsg{jobID: jobID, content: sanitizeForDisplay(content.String())}
		}

		// Single commit
		info, err := git.GetCommitInfo(job.RepoPath, job.GitRef)
		if err != nil {
			return commitMsgMsg{jobID: jobID, err: err}
		}

		var content strings.Builder
		fmt.Fprintf(&content, "Commit: %s\n", info.SHA)
		fmt.Fprintf(&content, "Author: %s\n", info.Author)
		fmt.Fprintf(&content, "Date:   %s\n\n", info.Timestamp.Format("2006-01-02 15:04:05 -0700"))
		content.WriteString(info.Subject + "\n")
		if info.Body != "" {
			content.WriteString("\n" + info.Body + "\n")
		}

		return commitMsgMsg{jobID: jobID, content: sanitizeForDisplay(content.String())}
	}
}

func (m model) fetchPatch(jobID int64) tea.Cmd {
	return func() tea.Msg {
		patch, err := m.loadPatch(jobID)
		if err != nil {
			return patchMsg{jobID: jobID, err: err}
		}
		return patchMsg{jobID: jobID, patch: patch}
	}
}

// fetchFixJobs fetches fix jobs from the daemon.
func (m model) fetchFixJobs() tea.Cmd {
	gen := m.fetchGen
	return func() tea.Msg {
		params := neturl.Values{}
		params.Set("job_type", "fix")
		params.Set("limit", "200")

		result, err := m.loadJobsPage(params)
		if err != nil {
			return fixJobsMsg{err: err, gen: gen}
		}
		return fixJobsMsg{jobs: result.Jobs, gen: gen}
	}
}

// startFetchFixJobs dispatches fetchFixJobs if no fix-jobs fetch is already
// in flight, and sets the loadingFixJobs flag. Returns nil if skipped.
func (m *model) startFetchFixJobs() tea.Cmd {
	if m.loadingFixJobs {
		return nil
	}
	m.loadingFixJobs = true
	return m.fetchFixJobs()
}

// requestFetchFixJobs is like startFetchFixJobs but for handlers that follow
// state-mutating operations (fix enqueue, patch apply). If a fetch is already
// in flight, it marks the current data as stale so handleFixJobsMsg will
// dispatch a follow-up fetch when the in-flight one returns.
func (m *model) requestFetchFixJobs() tea.Cmd {
	if m.loadingFixJobs {
		m.fixJobsStale = true
		return nil
	}
	m.loadingFixJobs = true
	return m.fetchFixJobs()
}
