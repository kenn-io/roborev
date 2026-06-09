package tui

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/atotto/clipboard"
	osc52 "github.com/aymanbagabas/go-osc52/v2"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
)

type viewKind int

const (
	viewQueue viewKind = iota
	viewReview
	viewKindPrompt
	viewFilter
	viewKindComment
	viewCommitMsg
	viewHelp
	viewLog
	viewTasks               // Background fix tasks view
	viewKindWorktreeConfirm // Confirm creating a worktree to apply patch
	viewPatch               // Patch viewer for fix jobs
	viewColumnOptions       // Column toggle modal
)

// queuePrefetchBuffer is the number of extra rows to fetch beyond what's visible,
// providing a buffer for smooth scrolling without needing immediate pagination.
const queuePrefetchBuffer = 10

// repoFilterItem represents a repo (or group of repos with same display name) in the filter modal
type repoFilterItem struct {
	name      string   // Display name. Empty string means "All repos"
	rootPaths []string // Repo paths that share this display name. Empty for "All repos"
	count     int
}

// branchFilterItem represents a branch in the filter modal
type branchFilterItem struct {
	name  string // Branch name. Empty string means "All branches"
	count int
}

// treeFilterNode represents a repo node in the unified tree filter
type treeFilterNode struct {
	name          string             // Display name
	rootPaths     []string           // Repo paths (for API calls)
	count         int                // Total job count
	expanded      bool               // Whether children are visible
	userCollapsed bool               // User explicitly collapsed during search
	children      []branchFilterItem // Branch children (lazy-loaded)
	loading       bool               // True while branch fetch is in-flight
	fetchFailed   bool               // True after a search-triggered fetch failed
}

// flatFilterEntry represents a single row in the flattened tree filter view
type flatFilterEntry struct {
	repoIdx   int // Index into filterTree (-1 for "All")
	branchIdx int // Index into children (-1 for repo-level)
}

// pendingState tracks a pending closed toggle with sequence number
type pendingState struct {
	newState bool
	seq      uint64
}

// logLine represents a single pre-rendered line of agent output in the log view.
// Text is already styled via streamFormatter (ANSI codes included).
type logLine struct {
	text string
}

// colWidthCache stores computed per-column max content widths alongside a
// generation counter so renders can skip the full-scan when data hasn't changed.
type colWidthCache struct {
	gen           int
	contentWidths map[int]int
}

// Sentinel IDs for non-column toggles in the column options modal.
const (
	colOptionBorders       = -1
	colOptionMouse         = -2
	colOptionTasksWorkflow = -3
)

// columnOption represents an item in the column options modal.
// id is a queue column constant or a sentinel option ID.
type columnOption struct {
	id      int    // column constant or sentinel option ID
	name    string // display label
	enabled bool   // visible/on
}

// helpItem is a single help-bar entry with a key label and description.
type helpItem struct {
	key  string
	desc string
}

// logOutputMsg delivers output lines from the daemon
type logOutputMsg struct {
	lines     []logLine
	hasMore   bool // true if job is still running
	err       error
	newOffset int64                // byte offset for next fetch
	append    bool                 // true = append lines, false = replace
	seq       uint64               // fetch sequence number for stale detection
	fmtr      *streamfmt.Formatter // formatter used for rendering (persist for incremental reuse)
}

// logTickMsg triggers a refresh of the log output
type logTickMsg struct{}

// displayTickMsg triggers a local repaint without polling the daemon.
type displayTickMsg struct{}

type (
	tickMsg time.Time
	jobsMsg struct {
		jobs    []storage.ReviewJob
		hasMore bool
		append  bool             // true to append to existing jobs, false to replace
		seq     int              // fetch sequence number — stale responses (seq < model.fetchSeq) are discarded
		stats   storage.JobStats // aggregate counts from server
	}
)

type statusMsg struct {
	status storage.DaemonStatus
	gen    uint64 // fetch generation — discard if < model.fetchGen
}
type costMsg struct {
	cost *storage.CostAggregate // nil = hide segment (error or unexpressible scope)
	seq  int                    // fetch sequence — stale responses (seq < model.fetchSeq) discarded
}
type reviewMsg struct {
	review     *storage.Review
	responses  []storage.Response // Responses for this review
	jobID      int64              // The job ID that was requested (for race condition detection)
	branchName string             // Pre-computed branch name (empty if not applicable)
}
type promptMsg struct {
	review *storage.Review
	jobID  int64 // The job ID that was requested (for stale response detection)
}
type (
	closedMsg       bool
	closedResultMsg struct {
		jobID            int64 // job ID for queue view rollback
		reviewID         int64 // review ID for review view rollback
		reviewView       bool  // true if from review view (rollback currentReview)
		restoreSelection bool
		oldState         bool
		newState         bool   // the requested state (for pendingClosed validation)
		seq              uint64 // request sequence number (for distinguishing same-state rapid toggles)
		err              error
	}
)

type cancelResultMsg struct {
	jobID            int64
	oldState         storage.JobStatus
	oldFinishedAt    *time.Time
	restoreSelection bool
	err              error
}
type rerunResultMsg struct {
	jobID         int64
	oldState      storage.JobStatus
	oldStartedAt  *time.Time
	oldFinishedAt *time.Time
	oldError      string
	oldClosed     *bool
	oldVerdict    *string
	err           error
}
type (
	errMsg       error
	statusErrMsg struct {
		err error
		gen uint64
	}
)

type (
	configSaveErrMsg struct{ err error }
	jobsErrMsg       struct {
		err error
		seq int // fetch sequence number for staleness check
	}
)

type paginationErrMsg struct {
	err error
	seq int // fetch sequence number for staleness check
}
type updateCheckMsg struct {
	version    string // Latest version if available, empty if up to date
	isDevBuild bool   // True if running a dev build
}
type reposMsg struct {
	repos          []repoFilterItem
	identities     map[string][]string
	branchFiltered bool // true if fetched with a branch constraint
}

// repoNamesMsg delivers the display-name-to-root-paths mapping from
// /api/repos, used by the control socket to resolve display names in
// set-filter commands. Fetched once at init.
type repoNamesMsg struct {
	names      map[string][]string // display name → root paths
	identities map[string][]string // repo identity → root paths
}
type branchesMsg struct {
	backfillCount int // Number of branches successfully backfilled to the database
}
type repoBranchesMsg struct {
	repoIdx      int                // Which repo in filterTree
	rootPaths    []string           // Repo identity (for stale message detection)
	branches     []branchFilterItem // Branch data
	err          error              // Non-nil on fetch failure
	expandOnLoad bool               // Set expanded=true when branches arrive
	searchSeq    int                // Search generation; stale errors don't set fetchFailed
}
type commentResultMsg struct {
	jobID int64
	err   error
}
type clipboardResultMsg struct {
	err  error
	view viewKind // The view where copy was triggered (for flash attribution)
}
type commitMsgMsg struct {
	jobID   int64
	content string
	err     error
}
type reconnectMsg struct {
	endpoint daemon.DaemonEndpoint // New daemon endpoint if found
	version  string                // Daemon version (to avoid sync call in Update)
	err      error
}

type fixJobsMsg struct {
	jobs []storage.ReviewJob
	err  error
	gen  uint64 // fetch generation — discard if < model.fetchGen
}

type fixTriggerResultMsg struct {
	job     *storage.ReviewJob
	err     error
	warning string // non-fatal issue (e.g. failed to mark stale job)
}

type applyPatchResultMsg struct {
	jobID        int64
	parentJobID  int64 // Parent review job (to mark closed on success)
	success      bool
	commitFailed bool // True only when patch applied but git commit failed (working tree is dirty)
	err          error
	rebase       bool   // True if patch didn't apply and needs rebase
	needWorktree bool   // True if branch is not checked out and needs a worktree
	branch       string // Branch name (for worktree creation prompt)
	worktreeDir  string // Non-empty if a temp worktree was kept for recovery
}

type patchMsg struct {
	jobID int64
	patch string
	err   error
}

// panelMembersMsg delivers the side-fetched member rows for a panel run, or an
// error. On error the handler leaves the panel uncached so a later expand
// retries.
type panelMembersMsg struct {
	runUUID string
	members []storage.ReviewJob
	err     error
}

type savePatchResultMsg struct {
	path string
	err  error
}

// ClipboardWriter is an interface for clipboard operations (allows mocking in tests)
type ClipboardWriter interface {
	WriteText(text string) error
}

// realClipboard implements ClipboardWriter using the system clipboard
type realClipboard struct{}

func (r *realClipboard) WriteText(text string) error {
	return clipboard.WriteAll(text)
}

// osc52Clipboard implements ClipboardWriter using OSC52 terminal escape sequences.
// It writes clipboard data through the terminal emulator, which makes it work
// correctly over SSH without requiring X11 forwarding.
type osc52Clipboard struct {
	output io.Writer
}

func (c *osc52Clipboard) WriteText(text string) error {
	_, err := osc52.New(text).WriteTo(c.output)
	return err
}

// option func(*options) is a functional option for TUI.
type option func(*options)

// withRepoFilter locks the TUI filter to a specific repo.
func withRepoFilter(repo string) option {
	return func(o *options) { o.repoFilter = repo }
}

// withBranchFilter locks the TUI filter to a specific branch.
func withBranchFilter(branch string) option {
	return func(o *options) { o.branchFilter = branch }
}

func withContext(ctx context.Context) option {
	return func(o *options) { o.ctx = ctx }
}

// withExternalIODisabled disables daemon/config/git calls in newModel.
func withExternalIODisabled() option {
	return func(o *options) { o.disableExternalIO = true }
}

// withAutoFilterBranch simulates auto_filter_branch config in tests.
func withAutoFilterBranch(branch string) option {
	return func(o *options) {
		o.autoFilterBranch = true
		o.cwdBranch = branch
	}
}

// withAutoFilterRepo simulates auto_filter_repo config in tests.
func withAutoFilterRepo(repo string) option {
	return func(o *options) {
		o.autoFilterRepo = true
		o.cwdRepoRoot = repo
	}
}

func withCwdRepoIdentity(identity string) option {
	return func(o *options) { o.cwdRepoIdentity = identity }
}

// options holds optional overrides for the TUI model, set from CLI flags.
type options struct {
	ctx               context.Context
	repoFilter        string // --repo flag: lock filter to this repo path
	branchFilter      string // --branch flag: lock filter to this branch
	noQuit            bool   // --no-quit flag: suppress keyboard quit
	disableExternalIO bool   // tests: disable daemon/config/git calls
	autoFilterRepo    bool   // tests: simulate auto_filter_repo config
	autoFilterBranch  bool   // tests: simulate auto_filter_branch config
	cwdRepoRoot       string // tests: simulate detected repo root
	cwdRepoIdentity   string // tests: simulate detected repo identity
	cwdBranch         string // tests: simulate detected branch
}

// withNoQuit disables keyboard-initiated quit (q key).
func withNoQuit() option {
	return func(o *options) { o.noQuit = true }
}

// errNoLog is a sentinel error returned when the job log API
// returns 404 (job has no log output yet or was never started).
var errNoLog = errors.New("no log available")
