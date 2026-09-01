package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"uuid"

	tea "charm.land/bubbletea/v2"
	gansi "charm.land/glamour/v2/ansi"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
	"go.kenn.io/roborev/internal/update"
	daemonclient "go.kenn.io/roborev/pkg/client/generated"
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
	apiParams := daemonclient.ListJobsRequestOptions{Query: listJobsQuery(params)}
	resp, err := m.api.ListJobsWithResponse(m.apiContext(), &apiParams)
	if err != nil && resp == nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
	}
	var result jobsPageResult
	if err := decodeAPIBody(resp.Body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (m model) loadRepoList(branchFilter string) (*repoListResult, bool, error) {
	branchFiltered := branchFilter != "" && branchFilter != branchNone
	var query daemonclient.ListReposQuery
	if branchFiltered {
		query.Branch = &branchFilter
	}
	resp, err := m.api.ListReposWithResponse(
		m.apiContext(),
		&daemonclient.ListReposRequestOptions{Query: &query},
	)
	if err != nil && resp == nil {
		return nil, false, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
	}
	var result repoListResult
	if err := decodeAPIBody(resp.Body, &result); err != nil {
		return nil, false, err
	}
	return &result, branchFiltered, nil
}

func (m model) loadBranchList(rootPaths []string) (*branchListResult, error) {
	var query daemonclient.ListBranchesQuery
	if len(rootPaths) > 0 {
		query.Repo = append([]string(nil), rootPaths...)
	}
	resp, err := m.api.ListBranchesWithResponse(
		m.apiContext(),
		&daemonclient.ListBranchesRequestOptions{Query: &query},
	)
	if err != nil && resp == nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
	}
	var result branchListResult
	if err := decodeAPIBody(resp.Body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func listJobsQuery(values neturl.Values) *daemonclient.ListJobsQuery {
	query := daemonclient.ListJobsQuery{}
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

	setIntParam("id", &query.ID)
	setStringParam("status", &query.Status)
	// repo is repeatable: a display name spanning multiple repos sends one
	// value per path, scoped server-side via an IN clause.
	if repos := values["repo"]; len(repos) > 0 {
		query.Repo = append([]string(nil), repos...)
	}
	setStringParam("git_ref", &query.GitRef)
	setStringParam("branch", &query.Branch)
	if value := values.Get("branch_include_empty"); value != "" {
		typed := daemonclient.ListJobsQueryBranchIncludeEmpty(value)
		query.BranchIncludeEmpty = &typed
	}
	if value := values.Get("closed"); value != "" {
		typed := daemonclient.ListJobsQueryClosed(value)
		query.Closed = &typed
	}
	setStringParam("job_type", &query.JobType)
	setStringParam("exclude_job_type", &query.ExcludeJobType)
	if value := values.Get("hide_classify_jobs"); value != "" {
		typed := daemonclient.ListJobsQueryHideClassifyJobs(value)
		query.HideClassifyJobs = &typed
	}
	if value := values.Get("omit_prompt"); value != "" {
		typed := daemonclient.ListJobsQueryOmitPrompt(value)
		query.OmitPrompt = &typed
	}
	setStringParam("repo_prefix", &query.RepoPrefix)
	setIntParam("limit", &query.Limit)
	setIntParam("offset", &query.Offset)
	setIntParam("before", &query.Before)
	return &query
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

	jobsCmd := func() tea.Msg {
		// Build URL with server-side filters where possible, falling back to
		// limit=0 (no pagination) only when client-side filtering is required.
		params := neturl.Values{}

		// Repo filter: one ?repo= per path. A display name spanning multiple
		// repos is scoped server-side via an IN clause, so it paginates like a
		// single repo rather than loading every job for client-side filtering.
		needsAllJobs := false
		for _, path := range m.activeRepoFilter {
			params.Add("repo", path)
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

		// Metadata-only rows: completed jobs' prompts are never rendered
		// from the queue and dominate the payload. Queued/running jobs
		// keep their prompt server-side for the prompt view.
		params.Set("omit_prompt", "true")

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
	// fetchCost must always return a non-nil command: tests (fetchJobsMessage)
	// rely on this staying a 2-element BatchMsg whose first command is jobsCmd.
	return tea.Batch(jobsCmd, m.fetchCost())
}

func (m model) fetchMoreJobs() tea.Cmd {
	seq := m.fetchSeq
	return func() tea.Msg {
		// Only fetch more when not doing client-side filtering that loads all jobs.
		// Multi-repo display names paginate server-side via repeated repo params;
		// the "(none)" branch sentinel still loads everything client-side.
		if m.activeBranchFilter == branchNone {
			return nil
		}
		offset := len(m.jobs)
		params := neturl.Values{}
		params.Set("limit", "50")
		params.Set("offset", fmt.Sprintf("%d", offset))
		params.Set("omit_prompt", "true")
		for _, path := range m.activeRepoFilter {
			params.Add("repo", path)
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

// fetchCost retrieves the approximate cost aggregate for the active filter scope.
// It rides the same fetchSeq stale guard as fetchJobs so a response that arrives
// after a filter change is discarded. Any error, non-200, or decode failure
// yields a nil cost so the queue-header segment hides rather than showing stale
// or bogus data.
func (m model) fetchCost() tea.Cmd {
	seq := m.fetchSeq
	var query daemonclient.GetCostQuery
	if len(m.activeRepoFilter) > 0 {
		query.Repo = append([]string(nil), m.activeRepoFilter...)
	}
	if m.activeBranchFilter == branchNone {
		be := daemonclient.True
		query.BranchEmpty = &be
	} else if m.activeBranchFilter != "" {
		branch := m.activeBranchFilter
		query.Branch = &branch
	}
	return func() tea.Msg {
		resp, err := m.api.GetCostWithResponse(
			m.apiContext(),
			&daemonclient.GetCostRequestOptions{Query: &query},
		)
		if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
			return costMsg{cost: nil, seq: seq}
		}
		var c storage.CostAggregate
		if err := decodeAPIBody(resp.Body, &c); err != nil {
			return costMsg{cost: nil, seq: seq}
		}
		return costMsg{cost: &c, seq: seq}
	}
}

func (m model) fetchStatus() tea.Cmd {
	gen := m.fetchGen
	return func() tea.Msg {
		resp, err := m.api.GetStatusWithResponse(m.apiContext())
		if err != nil && resp == nil {
			return statusErrMsg{err: err, gen: gen}
		}
		if resp.StatusCode != http.StatusOK {
			return statusErrMsg{
				err: apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body),
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

// backfillBranchValue decides what branch value, if any, to persist for a
// job with no stored branch. ok=false means the row is deliberately left
// unbackfilled: a detached single-commit review renders a
// "(detached @ <sha>)" placeholder from its empty stored branch, and
// persisting the branchNone sentinel would freeze the row at "(none)"
// Backfill runs once per TUI session (branchBackfillDone), so the
// repeated lookup cost for skipped rows is bounded.
func backfillBranchValue(job storage.ReviewJob, machineID *uuid.UUID) (string, bool) {
	// Mark task jobs (run, analyze, custom) or dirty jobs with no-branch sentinel
	if job.IsTaskJob() || job.IsDirtyJob() {
		return branchNone, true
	}
	// Mark remote jobs with no-branch sentinel (can't look up)
	if job.RepoPath == "" || (machineID != nil && job.SourceMachineID != nil && *job.SourceMachineID != *machineID) {
		return branchNone, true
	}

	// Preserve the old sentinel backfill when the repo cannot be verified
	// locally: an empty lookup there means "couldn't look", not "detached",
	// and skipping would strand the row with NullsRemaining nonzero.
	if _, err := os.Stat(job.RepoPath); err != nil {
		return branchNone, true
	}

	sha := job.GitRef
	if idx := strings.Index(sha, ".."); idx != -1 {
		sha = sha[idx+2:]
	}
	branch := git.GetBranchName(job.RepoPath, sha)
	if branch == "" {
		if detachedBranchLabel(job) != "" {
			return "", false
		}
		branch = branchNone // Mark as attempted but not found
	}
	return branch, true
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
				branch, ok := backfillBranchValue(job, machineID)
				if !ok {
					continue
				}
				toBackfill = append(toBackfill, backfillJob{id: job.ID, branch: branch})
			}

			for _, bf := range toBackfill {
				resp, err := m.api.UpdateJobBranchWithResponse(
					m.apiContext(),
					&daemonclient.UpdateJobBranchRequestOptions{
						Body: &daemonclient.UpdateJobBranchRequest{
							JobID:  bf.id,
							Branch: bf.branch,
						},
					},
				)
				if err == nil && resp.StatusCode == http.StatusOK {
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
		&daemonclient.GetReviewRequestOptions{Query: &daemonclient.GetReviewQuery{JobID: &jobID}},
	)
	if err != nil && resp == nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		err := apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
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
		&daemonclient.ListCommentsQuery{JobID: &jobID},
		&jobResult,
	); err == nil {
		responses = jobResult.Responses
	}

	// Also fetch legacy commit-based responses and merge.
	// Prefer commit_id (unambiguous), fall back to SHA for legacy jobs.
	var legacyParams *daemonclient.ListCommentsQuery
	if review.Job != nil {
		commitID, fallbackSHA := review.Job.LegacyCommentLookupTarget()
		if commitID > 0 {
			legacyParams = &daemonclient.ListCommentsQuery{CommitID: &commitID}
		} else if fallbackSHA != "" {
			legacyParams = &daemonclient.ListCommentsQuery{Sha: &fallbackSHA}
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
		&daemonclient.GetJobPatchRequestOptions{Query: &daemonclient.GetJobPatchQuery{JobID: &jobIDParam}},
	)
	if err != nil && resp == nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		err := apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
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
	query *daemonclient.ListCommentsQuery,
	out any,
) error {
	resp, err := m.api.ListCommentsWithResponse(
		m.apiContext(),
		&daemonclient.ListCommentsRequestOptions{Query: query},
	)
	if err != nil && resp == nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return apiStatusError(resp.StatusCode, apiStatus(resp.StatusCode), resp.Body)
	}
	return decodeAPIBody(resp.Body, out)
}

// dispatchFailedCommentsFetch is the single entry point for the
// synthesized-review comments fetch: it advances the side channel's own
// request identity and stamps it onto the outgoing request, so a stale
// response can never overwrite a newer fetch's result or a post-success
// local append. Standalone-statement rule applies (see
// dispatchReviewFetch's evaluation-order pitfall above).
func (m *model) dispatchFailedCommentsFetch(jobID int64) tea.Cmd {
	m.failedCommentsSeq++
	return m.fetchFailedJobComments(jobID, m.failedCommentsSeq)
}

// fetchFailedJobComments loads persisted comments for a job whose review
// is synthesized -- see failedCommentsMsg. Reached only through
// dispatchFailedCommentsFetch, which owns the seq bump.
func (m model) fetchFailedJobComments(jobID int64, seq uint64) tea.Cmd {
	return func() tea.Msg {
		var result struct {
			Responses []storage.Response `json:"responses"`
		}
		query := &daemonclient.ListCommentsQuery{JobID: &jobID}
		if err := m.loadComments(query, &result); err != nil {
			return failedCommentsMsg{jobID: jobID, err: err, seq: seq}
		}
		return failedCommentsMsg{jobID: jobID, responses: result.Responses, seq: seq}
	}
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

// dispatchReviewFetch is the ONE entry point for an ordinary (non-follow)
// review-content fetch: it bumps the shared ordering epoch and stamps the
// new value onto the request. Every caller goes through it (or through
// dispatchReviewFollow, its follow-tagged twin) so no dispatcher can be
// added that skips the epoch -- see m.reviewFetchSeq's doc comment
// (tui.go). The pointer receiver is what makes the bump stick; the value
// snapshot the command closes over is taken after it.
//
// CALL IT ON ITS OWN LINE -- every one of the call sites is written
//
//	cmd := m.dispatchReviewFetch(job.ID)
//	return m, cmd
//
// and NOT the tempting one-liner `return m, m.dispatchReviewFetch(job.ID)`.
// In a return statement Go evaluates the function calls among the operands
// first, but the order of a plain variable operand (m) relative to those
// calls is unspecified -- so the one-liner may return the model as it was
// BEFORE the bump, silently stamping a request with an epoch the model
// never advanced to. Such a request can never be accepted (its stamp will
// never equal m.reviewFetchSeq) and the fetch is simply lost. The same
// applies to any other pointer-receiver mutation returned alongside m.
func (m *model) dispatchReviewFetch(jobID int64) tea.Cmd {
	m.reviewFetchSeq++
	// Arm the pending-open intent (see pendingReviewOpenJobID's doc comment,
	// tui.go) BEFORE the fetch closure captures m.currentView below, so a
	// follow fetch that later races this one and lands first still knows
	// where to switch to. Matches what fetchReview stamps onto the message
	// as dispatchedFrom -- both read m.currentView at this same point.
	m.pendingReviewOpenJobID = jobID
	m.pendingReviewOpenOrigin = m.currentView
	// This dispatch's own identity (see pendingReviewOpenSeq's
	// doc comment, tui.go): m.reviewFetchSeq was just bumped above, so this
	// is the exact value fetchReview will stamp onto the outgoing request.
	m.pendingReviewOpenSeq = m.reviewFetchSeq
	// A fresh arm gets its own single follow-failure retry (see
	// pendingReviewOpenRetried's doc comment, tui.go). Reset here rather
	// than at every clear site: "retried" is only ever read while an
	// intent is armed.
	m.pendingReviewOpenRetried = false
	return m.fetchReview(jobID, m.reviewFetchSeq)
}

// dispatchReviewFollow is dispatchReviewFetch's follow-tagged twin: same
// epoch, same stamp, but the response updates the split detail pane's
// content without stealing focus or switching views, and its failures land
// in m.splitDetailErr. See fetchReviewFollow.
func (m *model) dispatchReviewFollow(jobID int64) tea.Cmd {
	m.reviewFetchSeq++
	return m.fetchReviewFollow(jobID, m.reviewFetchSeq)
}

// fetchReview dispatches the review fetch shared by every review-loading
// path -- queue Enter, tasks Enter/P, stepReviewNav, pagination nav, the
// queue 'F' fix-panel fetch, and (via fetchReviewFollow) the split-view
// debounced follow. The resulting reviewMsg is stamped with the dispatch
// origin, m.detailFollowGen, the fetch epoch fetchSeq, and this job's
// attempt counter m.jobAttemptGen[jobID], all captured at command-CREATION
// time (m is a value snapshot here, so these reflect state at dispatch, not
// whatever it drifts to before the response lands).
// Callers reach this through dispatchReviewFetch/dispatchReviewFollow,
// which own the epoch bump.
func (m model) fetchReview(jobID int64, fetchSeq uint64) tea.Cmd {
	// dispatchedFrom: the view the user was actually on when the fetch was
	// issued, regardless of where they navigate before it resolves (see
	// reviewMsg.dispatchedFrom).
	origin := m.currentView
	// gen: originally stamped only by fetchReviewFollow for split-view
	// follow fetches, so handleReviewMsg's follow path could reject a
	// response whose m.detailFollowGen had moved on by the time it landed
	// (a new selection via scheduleDetailFollow, or a rerun-success
	// clear/bump of the same job via handleRerunResultMsg -- see that
	// handler's doc comment for the full race). Stamping it here instead,
	// on every fetchReview call, closes the same race for NON-follow
	// fetches: a regular fetchReview already in flight when a rerun of the
	// SAME selected job succeeds can land afterward with the jobID check
	// alone still passing (a rerun reuses the job ID and doesn't move the
	// selection), restoring the previous attempt's review -- after which
	// splitReconcileDetail/handleDetailFollowTick see currentReview.JobID
	// already matching and skip fetching the rerun's actual result.
	// detailFollowGen is bumped by every abandonment path in BOTH layouts
	// -- followSelectionChange's stacked branch, scheduleDetailFollow,
	// handleJobsMsg's normalization epilogue, resetQueueForFilterChange
	// (the authoritative bumper list lives on the field's doc comment,
	// tui.go) -- so a fetch dispatched and landing without an intervening
	// abandonment lands at an unchanged gen and is unaffected. The
	// stacked-mode bump is load-bearing: it is what dooms an abandoned
	// dispatch's response after Enter on X, navigate to Y, return to X.
	// See handleReviewMsg for the rejection check, and detailFollowGen's
	// doc comment (tui.go) for the contract any bumper must satisfy.
	//
	// Rerun invalidation is NOT gen's job -- the per-job attempt stamp
	// below covers that, for any job.
	gen := m.detailFollowGen
	// attempt: this JOB's confirmed-rerun count at dispatch time. Stamped
	// here, alongside gen and fetchSeq, because fetchReview is the single
	// constructor of reviewMsg/reviewErrMsg -- clause 3 of jobAttemptGen's
	// contract (tui.go). Reading a nil map is legal and yields 0, the
	// correct "no rerun of this job observed yet" value, so no dispatcher
	// has to care whether the map has been populated.
	attempt := m.jobAttemptGen[jobID]
	return func() tea.Msg {
		review, err := m.loadReview(jobID)
		if err != nil {
			// Typed, not the generic errMsg:
			// jobID/gen/fetchSeq let handleReviewErrMsg resolve
			// pendingReviewOpenJobID/the pending fix panel for THIS job on a
			// genuine failure, the same way handleReviewFollowErrMsg already
			// does for a follow's failure. fetchReviewFollow below re-tags
			// this into reviewFollowErrMsg for its own wrapped call.
			return reviewErrMsg{
				jobID: jobID, err: err, gen: gen,
				fetchSeq: fetchSeq, attempt: attempt,
			}
		}

		responses := m.loadResponses(jobID, review)

		branchName := reviewBranchName(review.Job)

		return reviewMsg{
			review: review, responses: responses, jobID: jobID,
			branchName: branchName, dispatchedFrom: origin, gen: gen,
			fetchSeq: fetchSeq, attempt: attempt,
		}
	}
}

// fetchReviewFollow wraps fetchReview, tagging the resulting reviewMsg as a
// split-view follow fetch so the handler updates the pane without stealing
// focus or switching views. A fetch failure is re-tagged from fetchReview's
// own reviewErrMsg (jobID/gen/fetchSeq already correct, captured by
// fetchReview at the same command-creation point) into reviewFollowErrMsg,
// so handleReviewFollowErrMsg can record it in m.splitDetailErr for the pane
// to render instead of the plain review-open resolution
// handleReviewErrMsg performs for an ordinary fetch's failure.
func (m model) fetchReviewFollow(jobID int64, fetchSeq uint64) tea.Cmd {
	inner := m.fetchReview(jobID, fetchSeq)
	return func() tea.Msg {
		msg := inner()
		if rm, ok := msg.(reviewMsg); ok {
			rm.follow = true
			return rm
		}
		if em, ok := msg.(reviewErrMsg); ok {
			return reviewFollowErrMsg(em)
		}
		return msg
	}
}

// reviewBranchName returns the branch to display on the review screen.
// It prefers the stored job.Branch (set at enqueue time) over a dynamic
// git name-rev lookup, which can be misled by worktree branches
// reachable from the same SHA. Falls back to git lookup only for
// single-commit reviews when the stored branch is empty, and finally to
// a "(detached @ <sha>)" placeholder when neither resolves a branch, so a
// commit made on top of a detached HEAD doesn't render as a blank field
// (#499).
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
		if branch := git.GetBranchName(job.RepoPath, job.GitRef); branch != "" {
			return branch
		}
	}
	return detachedBranchLabel(*job)
}

// dispatchPromptFetch is the single entry point for prompt fetches: it
// advances the prompt path's own request identity and stamps it (plus the
// dispatch-origin view) onto the outgoing request. Callers must invoke it
// as a standalone statement, never inline in a return -- the same
// evaluation-order pitfall documented on dispatchReviewFetch above.
func (m *model) dispatchPromptFetch(jobID int64) tea.Cmd {
	m.promptFetchSeq++
	return m.fetchReviewForPrompt(jobID, m.promptFetchSeq)
}

// fetchReviewForPrompt loads a done job's review so the prompt view can
// show the prompt it was built from. It writes the SAME currentReview
// field as fetchReview, so it stamps the same per-job attempt counter at
// dispatch (jobAttemptGen contract clause 3, tui.go) -- a rerun of the job
// abandons this request exactly as it abandons a review fetch. It does not
// join the shared fetch epoch (see handlePromptMsg for why); staleness is
// tracked by the prompt path's own promptFetchSeq, stamped here along with
// the dispatch-origin view. Reached only through dispatchPromptFetch,
// which owns the seq bump.
func (m model) fetchReviewForPrompt(jobID int64, promptSeq uint64) tea.Cmd {
	attempt := m.jobAttemptGen[jobID]
	origin := m.currentView
	return func() tea.Msg {
		review, err := m.loadReview(jobID)
		if err != nil {
			return errMsg(err)
		}
		return promptMsg{
			review: review, jobID: jobID, attempt: attempt,
			promptSeq: promptSeq, dispatchedFrom: origin,
		}
	}
}

// fetchPanelMembers loads a panel run's member rows via GET /api/jobs?panel_run.
// The generated client has no panel_run param, so this issues a raw request like
// fetchJobLog (and show.go's fetchPanelMembers). The endpoint returns the full
// run (members + synthesis); keep only members, sorted by member index. On error
// the msg carries err and the handler leaves the panel uncached so a later
// expand retries.
func (m model) fetchPanelMembers(runUUID uuid.UUID) tea.Cmd {
	baseURL := m.endpoint.BaseURL()
	client := m.client
	return func() tea.Msg {
		url := fmt.Sprintf("%s/api/jobs?panel_run=%s&limit=0&omit_prompt=true", baseURL,
			neturl.QueryEscape(runUUID.String())) //nolint:forbidigo // HTTP query parameter boundary.
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
	state := logFetchState{
		baseURL: m.endpoint.BaseURL(),
		client:  m.client,
		width:   m.width,
		style:   m.glamourStyle,
		offset:  m.logOffset,
		fmtr:    m.logFmtr,
		agent:   m.logAgent,
		source:  m.logSource,
	}
	seq := m.logFetchSeq
	return func() tea.Msg {
		result := fetchLog(jobID, state)
		return logOutputMsg{
			lines:     result.lines,
			hasMore:   result.hasMore,
			err:       result.err,
			newOffset: result.newOffset,
			append:    result.append,
			agent:     result.agent,
			source:    result.source,
			seq:       seq,
			fmtr:      result.fmtr,
		}
	}
}

// fetchPaneLog uses the split detail pane's independent stream state while
// sharing the log-fetch protocol with the full-screen view.
func (m model) fetchPaneLog(jobID int64) tea.Cmd {
	state := logFetchState{
		baseURL: m.endpoint.BaseURL(),
		client:  m.client,
		width:   m.paneLogWidth(),
		style:   m.glamourStyle,
		offset:  m.paneLogOffset,
		fmtr:    m.paneLogFmtr,
		agent:   m.paneLogAgent,
		source:  m.paneLogSource,
	}
	seq := m.paneLogSeq
	return func() tea.Msg {
		result := fetchLog(jobID, state)
		return paneLogOutputMsg{
			jobID:     jobID,
			lines:     result.lines,
			hasMore:   result.hasMore,
			err:       result.err,
			newOffset: result.newOffset,
			append:    result.append,
			agent:     result.agent,
			source:    result.source,
			seq:       seq,
			fmtr:      result.fmtr,
		}
	}
}

type logFetchState struct {
	baseURL string
	client  *http.Client
	width   int
	style   gansi.StyleConfig
	offset  int64
	fmtr    *streamfmt.Formatter
	agent   string
	source  string
}

type logFetchResult struct {
	lines     []logLine
	hasMore   bool
	err       error
	newOffset int64
	append    bool
	agent     string
	source    string
	fmtr      *streamfmt.Formatter
}

func fetchLog(jobID int64, state logFetchState) logFetchResult {
	url := fmt.Sprintf(
		"%s/api/job/log?job_id=%d&offset=%d",
		state.baseURL, jobID, state.offset,
	)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return logFetchResult{err: err}
	}
	if state.agent != "" {
		req.Header.Set("X-Job-Agent", state.agent)
	}
	resp, err := state.client.Do(req)
	if err != nil {
		return logFetchResult{err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return logFetchResult{err: errNoLog}
	}
	if resp.StatusCode != http.StatusOK {
		return logFetchResult{err: fmt.Errorf("fetch log: %s", resp.Status)}
	}

	hasMore := resp.Header.Get("X-Job-Status") == "running"
	responseAgent := resp.Header.Get("X-Job-Agent")
	if responseAgent == "" {
		responseAgent = state.agent
	}
	responseSource := state.source
	if _, ok := resp.Header[http.CanonicalHeaderKey("X-Job-Source")]; ok {
		responseSource = resp.Header.Get("X-Job-Source")
	}
	identityChanged := responseSource != state.source ||
		(responseSource != storage.JobSourceAutoDesign && responseAgent != state.agent)
	serverReset := resp.Header.Get("X-Log-Reset") == "true"

	newOffset := state.offset
	if value := resp.Header.Get("X-Log-Offset"); value != "" {
		if parsed, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil {
			newOffset = parsed
		}
	}

	isIncremental := state.offset > 0 && state.fmtr != nil
	if newOffset < state.offset || identityChanged || serverReset {
		isIncremental = false
	}
	if newOffset == state.offset && isIncremental && hasMore {
		return logFetchResult{
			hasMore:   true,
			newOffset: newOffset,
			append:    true,
			agent:     responseAgent,
			source:    responseSource,
		}
	}

	var buf bytes.Buffer
	renderFmtr := state.fmtr
	if isIncremental {
		renderFmtr.SetWriter(&buf)
	} else {
		renderFmtr = streamfmt.NewWithWidth(
			&buf, state.width, state.style,
			decoderForJobLog(responseAgent, responseSource),
		)
	}

	renderLog := streamfmt.RenderLogWith
	if hasMore {
		renderLog = streamfmt.RenderLogChunkWith
	}
	if err := renderLog(resp.Body, renderFmtr); err != nil {
		return logFetchResult{err: err}
	}

	raw := buf.String()
	var lines []logLine
	if raw != "" {
		for line := range strings.SplitSeq(raw, "\n") {
			lines = append(lines, logLine{text: line})
		}
		if len(lines) > 0 && lines[len(lines)-1].text == "" {
			lines = lines[:len(lines)-1]
		}
	}

	return logFetchResult{
		lines:     lines,
		hasMore:   hasMore,
		newOffset: newOffset,
		append:    isIncremental,
		agent:     responseAgent,
		source:    responseSource,
		fmtr:      renderFmtr,
	}
}

func decoderForJobLog(agent, source string) streamfmt.Decoder {
	if source == storage.JobSourceAutoDesign {
		return streamfmt.LegacyMixedDecoder(agent)
	}
	return streamfmt.DecoderForAgent(agent)
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
						content.WriteString("   ")
						content.WriteString(line)
						content.WriteByte('\n')
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
		content.WriteString(info.Subject)
		content.WriteByte('\n')
		if info.Body != "" {
			content.WriteByte('\n')
			content.WriteString(info.Body)
			content.WriteByte('\n')
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
		params.Set("omit_prompt", "true")

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
