package storage

import (
	"strings"
	"time"

	gitrepo "go.kenn.io/kit/git/repo"
)

type Repo struct {
	ID        int64     `json:"id"`
	RootPath  string    `json:"root_path"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Identity  string    `json:"identity,omitempty"` // Unique identity for sync (git remote URL, .roborev-id, or local path)
}

type Commit struct {
	ID        int64     `json:"id"`
	RepoID    int64     `json:"repo_id"`
	SHA       string    `json:"sha"`
	Author    string    `json:"author"`
	Subject   string    `json:"subject"`
	Timestamp time.Time `json:"timestamp"`
	CreatedAt time.Time `json:"created_at"`
}

type JobStatus string

const (
	JobStatusQueued   JobStatus = "queued"
	JobStatusRunning  JobStatus = "running"
	JobStatusDone     JobStatus = "done"
	JobStatusFailed   JobStatus = "failed"
	JobStatusCanceled JobStatus = "canceled"
	JobStatusApplied  JobStatus = "applied"
	JobStatusRebased  JobStatus = "rebased"
	JobStatusSkipped  JobStatus = "skipped"
)

// JobType classifies what kind of work a review job represents.
const (
	JobTypeReview    = "review"    // Single commit review
	JobTypeRange     = "range"     // Commit range review
	JobTypeDirty     = "dirty"     // Uncommitted changes review
	JobTypeTask      = "task"      // Run/analyze/design/custom prompt
	JobTypeInsights  = "insights"  // Historical review insights analysis
	JobTypeCompact   = "compact"   // Consolidated review verification
	JobTypeFix       = "fix"       // Background fix using worktree
	JobTypeClassify  = "classify"  // Routing classifier that decides whether to enqueue a design review
	JobTypeSynthesis = "synthesis" // Panel synthesis job: produces the canonical review from member reviews
)

// Panel roles classify a review_jobs row within a panel run. These values
// must equal the bare string literals used in the panel SQL predicates.
const (
	PanelRoleMember    = "member"
	PanelRoleSynthesis = "synthesis"
)

// Job source values identify daemon-created automation rows.
const (
	JobSourceAutoDesign = "auto_design"
	JobSourceCI         = "ci"
)

type ReviewJob struct {
	ID                int64      `json:"id"`
	RepoID            int64      `json:"repo_id"`
	CommitID          *int64     `json:"commit_id,omitempty"`  // nil for ranges
	GitRef            string     `json:"git_ref"`              // SHA or "start..end" for ranges
	Branch            string     `json:"branch,omitempty"`     // Branch name at time of job creation
	CIBaseBranch      string     `json:"-"`                    // PR base branch for CI jobs; daemon-internal, used only for event/hook branch matching
	SessionID         string     `json:"session_id,omitempty"` // Reused prior session or captured current session ID
	Agent             string     `json:"agent"`
	Model             string     `json:"model,omitempty"`              // Effective model for this run (for opencode: provider/model format)
	Provider          string     `json:"provider,omitempty"`           // Effective provider for this run (e.g., anthropic, openai)
	RequestedModel    string     `json:"requested_model,omitempty"`    // Explicitly requested model; empty means reevaluate on rerun
	RequestedProvider string     `json:"requested_provider,omitempty"` // Explicitly requested provider; empty means reevaluate on rerun
	Reasoning         string     `json:"reasoning,omitempty"`          // thorough, standard, fast (default: thorough)
	JobType           string     `json:"job_type"`                     // one of the JobType* constants above
	Status            JobStatus  `json:"status"`
	EnqueuedAt        time.Time  `json:"enqueued_at"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	WorkerID          string     `json:"worker_id,omitempty"`
	Error             string     `json:"error,omitempty"`
	Prompt            string     `json:"prompt,omitempty"`
	RetryCount        int        `json:"retry_count"`
	DiffContent       *string    `json:"diff_content,omitempty"`  // For dirty reviews (uncommitted changes)
	DirtyFiles        []string   `json:"dirty_files,omitempty"`   // Unfiltered dirty file names for prompt metadata
	Agentic           bool       `json:"agentic"`                 // Enable agentic mode (allow file edits)
	PromptPrebuilt    bool       `json:"prompt_prebuilt"`         // Prompt was set at enqueue time and should be used as-is
	ReviewType        string     `json:"review_type,omitempty"`   // Review type (e.g., "security") - changes system prompt
	PatchID           string     `json:"patch_id,omitempty"`      // Stable patch-id for rebase tracking
	OutputPrefix      string     `json:"output_prefix,omitempty"` // Prefix to prepend to review output
	SkipReason        string     `json:"skip_reason,omitempty"`   // Reason a design review was skipped (status=skipped only)
	Source            string     `json:"source,omitempty"`        // Automation source; empty for explicit/user rows
	ParentJobID       *int64     `json:"parent_job_id,omitempty"` // Job being fixed (for fix jobs)
	Patch             *string    `json:"patch,omitempty"`         // Generated diff patch (for completed fix jobs)
	WorktreePath      string     `json:"worktree_path,omitempty"` // Worktree checkout path (empty = use RepoPath)
	CommandLine       string     `json:"command_line,omitempty"`  // Actual agent command line used for this run
	MinSeverity       string     `json:"min_severity,omitempty"`
	// Job-level failover override (F7): when set, the worker prefers these
	// over the workflow-resolved backup agent/model for this job's failover.
	BackupAgent string `json:"backup_agent,omitempty"`
	BackupModel string `json:"backup_model,omitempty"`
	// Panel relation (subagent review panels). Synced columns group member
	// + synthesis jobs of one panel run; ClaimBlocked is local-only.
	PanelRunUUID          string `json:"panel_run_uuid,omitempty"`
	PanelRole             string `json:"panel_role,omitempty"` // "" (non-panel), "member", or "synthesis"
	PanelName             string `json:"panel_name,omitempty"`
	PanelMemberName       string `json:"panel_member_name,omitempty"`
	PanelMemberIndex      int    `json:"panel_member_index,omitempty"`
	PanelMemberConfigJSON string `json:"panel_member_config_json,omitempty"`
	ClaimBlocked          bool   `json:"claim_blocked,omitempty"` // local-only scheduling gate
	TokenUsage            string `json:"token_usage,omitempty"`   // JSON blob from agentsview (token consumption)
	// Sync fields
	UUID            string     `json:"uuid,omitempty"`              // Globally unique identifier for sync
	SourceMachineID string     `json:"source_machine_id,omitempty"` // Machine that created this job
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`        // Last modification time
	SyncedAt        *time.Time `json:"synced_at,omitempty"`         // Last sync time

	// Joined fields for convenience
	RepoPath      string  `json:"repo_path,omitempty"`
	RepoName      string  `json:"repo_name,omitempty"`
	CommitSubject string  `json:"commit_subject,omitempty"` // empty for ranges
	Closed        *bool   `json:"closed,omitempty"`         // nil if no review yet
	Verdict       *string `json:"verdict,omitempty"`        // P/F parsed from review output
	// PanelSummary is the member breakdown for a synthesis (parent) row,
	// attached by the listing handler for collapsed panel display. Nil for
	// non-panel jobs and member rows.
	PanelSummary *PanelSummary `json:"panel_summary,omitempty"`

	// ReusableSessionTarget is a joined, non-serialized SHA used only by
	// session-reuse candidate validation. Dirty jobs keep GitRef="dirty" and
	// carry their base HEAD through this field.
	ReusableSessionTarget string `json:"-"`
}

// HookBranch returns the branch used for event/hook branch matching: the
// local branch the job was enqueued from, or the PR base (target) branch for
// CI jobs. CI jobs deliberately leave Branch empty so branch-scoped local
// flows (fix/refine discovery, fix-ref selection, session reuse) never treat
// a CI review as local work on the base branch.
func (j ReviewJob) HookBranch() string {
	if j.Branch != "" {
		return j.Branch
	}
	return j.CIBaseBranch
}

// IsCIReview returns true if this job was enqueued by the CI poller:
// CI jobs are tagged Source=ci by CreateCIPanelRun and record the PR base
// branch. Either marker counts so older rows without ci_base_branch are
// still recognized as CI.
func (j ReviewJob) IsCIReview() bool {
	return j.Source == JobSourceCI || j.CIBaseBranch != ""
}

// IsDirtyJob returns true if this is a dirty review (uncommitted changes).
func (j ReviewJob) IsDirtyJob() bool {
	if j.JobType != "" {
		return j.JobType == JobTypeDirty
	}
	// Fallback heuristic for jobs without job_type (e.g., from old sync data)
	return j.DiffContent != nil || j.GitRef == "dirty"
}

// CommitIDValue returns the commit ID as a plain int64 (0 if nil).
func (j ReviewJob) CommitIDValue() int64 {
	if j.CommitID != nil {
		return *j.CommitID
	}
	return 0
}

// IsTaskJob returns true if this is a task job (run, analyze, custom label) rather than
// a commit review or dirty review. Task jobs have pre-stored prompts and no verdicts.
// Compact jobs are not considered task jobs since they produce P/F verdicts.
func (j ReviewJob) IsTaskJob() bool {
	if j.JobType != "" {
		return j.JobType == JobTypeTask || j.JobType == JobTypeInsights
	}
	// Fallback heuristic for jobs without job_type (e.g., from old sync data)
	if j.CommitID != nil {
		return false
	}
	if j.DiffContent != nil {
		return false
	}
	if j.GitRef == "dirty" {
		return false
	}
	if strings.Contains(j.GitRef, "..") {
		return false
	}
	if j.GitRef == "" {
		return false
	}
	return true
}

// UsesStoredPrompt returns true if this job type uses a pre-stored prompt
// (task, insights, compact, or fix). These job types have prompts built at enqueue
// time, not constructed by the worker from git data.
func (j ReviewJob) UsesStoredPrompt() bool {
	return j.JobType == JobTypeTask ||
		j.JobType == JobTypeInsights ||
		j.JobType == JobTypeCompact ||
		j.JobType == JobTypeFix
}

// IsReviewJob returns true if this is an actual code review
// (single commit, range, or dirty) rather than a task, insights,
// compact, or fix job. The legacy fallback uses positive review
// signals (CommitID, dirty ref, range ref) to avoid misclassifying
// old stored-prompt jobs that happen to have a GitRef.
func (j ReviewJob) IsReviewJob() bool {
	if j.JobType != "" {
		return j.JobType == JobTypeReview ||
			j.JobType == JobTypeRange ||
			j.JobType == JobTypeDirty
	}
	// Positive heuristic for legacy jobs without job_type:
	// a review has a commit, is dirty, or is a range.
	if j.CommitID != nil {
		return true
	}
	if j.GitRef == "dirty" || j.DiffContent != nil {
		return true
	}
	if strings.Contains(j.GitRef, "..") {
		return true
	}
	return false
}

// IsFixJob returns true if this is a background fix job.
func (j ReviewJob) IsFixJob() bool {
	return j.JobType == JobTypeFix
}

// IsSynthesisJob returns true if this is a panel synthesis (parent) job.
// A synthesis job carries the canonical, verdict-parseable review for a
// panel run. It is neither a git-prompt job (review/range/dirty) nor a
// stored-prompt job (task/compact/fix) — it has its own worker path.
func (j ReviewJob) IsSynthesisJob() bool {
	return j.JobType == JobTypeSynthesis
}

// LegacyCommentLookupTarget returns the legacy commit-comment lookup key for
// this job. Only single-commit review rows are eligible: dirty jobs may carry a
// base HEAD commit_id for session reuse, but that base is not the reviewed
// subject and must not pull commit-scoped comments into dirty-review prompts.
func (j ReviewJob) LegacyCommentLookupTarget() (commitID int64, fallbackSHA string) {
	if j.IsDirtyJob() || strings.Contains(j.GitRef, "..") ||
		j.UsesStoredPrompt() || j.IsSynthesisJob() {
		return 0, ""
	}
	if j.CommitID != nil {
		return *j.CommitID, ""
	}
	if gitrepo.LooksLikeSHA(j.GitRef) {
		return 0, j.GitRef
	}
	return 0, ""
}

// HasViewableOutput returns true if this job has completed and its review/patch
// can be viewed. This covers done, applied, and rebased terminal states.
func (j ReviewJob) HasViewableOutput() bool {
	return j.Status == JobStatusDone || j.Status == JobStatusApplied || j.Status == JobStatusRebased
}

// JobWithReview pairs a job with its review for batch operations
type JobWithReview struct {
	Job    ReviewJob `json:"job"`
	Review *Review   `json:"review,omitempty"`
}

type Review struct {
	ID        int64     `json:"id"`
	JobID     int64     `json:"job_id"`
	Agent     string    `json:"agent"`
	Prompt    string    `json:"prompt"`
	Output    string    `json:"output"`
	CreatedAt time.Time `json:"created_at"`
	Closed    bool      `json:"closed"`

	// Sync fields
	UUID               string     `json:"uuid,omitempty"`                  // Globally unique identifier for sync
	UpdatedAt          *time.Time `json:"updated_at,omitempty"`            // Last modification time
	UpdatedByMachineID string     `json:"updated_by_machine_id,omitempty"` // Machine that last modified this review
	SyncedAt           *time.Time `json:"synced_at,omitempty"`             // Last sync time

	// Stored verdict: 1=pass, 0=fail, NULL=legacy (not yet backfilled)
	VerdictBool *int `json:"verdict_bool,omitempty"`

	// Joined fields
	Job *ReviewJob `json:"job,omitempty"`
}

type Response struct {
	ID        int64     `json:"id"`
	CommitID  *int64    `json:"commit_id,omitempty"` // For commit-based responses (legacy)
	JobID     *int64    `json:"job_id,omitempty"`    // For job/review-based responses
	Responder string    `json:"responder"`
	Response  string    `json:"response"`
	CreatedAt time.Time `json:"created_at"`

	// Sync fields
	UUID            string     `json:"uuid,omitempty"`              // Globally unique identifier for sync
	SourceMachineID string     `json:"source_machine_id,omitempty"` // Machine that created this response
	SyncedAt        *time.Time `json:"synced_at,omitempty"`         // Last sync time
}

// AutoDesignStatus carries per-outcome counters for the automatic
// design-review router. Only emitted when the feature is effectively
// enabled for at least one repo on the daemon.
type AutoDesignStatus struct {
	Enabled             bool  `json:"enabled"`
	TriggeredHeuristic  int64 `json:"triggered_heuristic"`
	SkippedHeuristic    int64 `json:"skipped_heuristic"`
	TriggeredClassifier int64 `json:"triggered_classifier"`
	SkippedClassifier   int64 `json:"skipped_classifier"`
	ClassifierFailed    int64 `json:"classifier_failed"`
}

type DaemonStatus struct {
	Version             string `json:"version"`
	QueuedJobs          int    `json:"queued_jobs"`
	RunningJobs         int    `json:"running_jobs"`
	CompletedJobs       int    `json:"completed_jobs"`
	FailedJobs          int    `json:"failed_jobs"`
	CanceledJobs        int    `json:"canceled_jobs"`
	AppliedJobs         int    `json:"applied_jobs"`
	RebasedJobs         int    `json:"rebased_jobs"`
	SkippedJobs         int    `json:"skipped_jobs"`
	ActiveWorkers       int    `json:"active_workers"`
	MaxWorkers          int    `json:"max_workers"`
	QueuePaused         bool   `json:"queue_paused"`
	Network             string `json:"network,omitempty"`
	Address             string `json:"address,omitempty"`
	Port                int    `json:"port,omitempty"`
	MachineID           string `json:"machine_id,omitempty"`            // Local machine ID for remote job detection
	ConfigReloadedAt    string `json:"config_reloaded_at,omitempty"`    // Last config reload timestamp (RFC3339Nano)
	ConfigReloadCounter uint64 `json:"config_reload_counter,omitempty"` // Monotonic reload counter (for sub-second detection)

	AutoDesign *AutoDesignStatus `json:"auto_design,omitempty"` // Auto design review counters; nil when disabled everywhere
}

// HealthStatus represents the overall daemon health
type HealthStatus struct {
	Healthy      bool              `json:"healthy"`
	Uptime       string            `json:"uptime"`
	Version      string            `json:"version"`
	Components   []ComponentHealth `json:"components"`
	RecentErrors []ErrorEntry      `json:"recent_errors"`
	ErrorCount   int               `json:"error_count_24h"`
}

// ComponentHealth represents the health of a single component
type ComponentHealth struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Message string `json:"message,omitempty"`
}

// ErrorEntry represents a single error log entry (mirrors daemon.ErrorEntry for API)
type ErrorEntry struct {
	Timestamp time.Time `json:"ts"`
	Level     string    `json:"level"`
	Component string    `json:"component"`
	Message   string    `json:"message"`
	JobID     int64     `json:"job_id,omitempty"`
}
