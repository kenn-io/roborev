package tui

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	gansi "charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/mattn/go-runewidth"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

// Tick intervals for local redraws and fallback polling.
const (
	displayTickInterval  = 1 * time.Second  // Repaint only (elapsed counters, flash expiry)
	tickIntervalFallback = 15 * time.Second // Fallback poll; SSE handles real-time updates
)

// TUI styles using AdaptiveColor for light/dark terminal support.
// Light colors are chosen for dark-on-light terminals; Dark colors for light-on-dark.
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(adaptiveColor("125", "205")) // Magenta/Pink

	statusStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("242", "246")) // Gray

	selectedStyle = lipgloss.NewStyle().
			Background(adaptiveColor("153", "24")) // Light blue background

	queuedStyle   = lipgloss.NewStyle().Foreground(adaptiveColor("136", "226")) // Yellow/Gold
	runningStyle  = lipgloss.NewStyle().Foreground(adaptiveColor("25", "33"))   // Blue
	doneStyle     = lipgloss.NewStyle().Foreground(adaptiveColor("27", "39"))   // Blue (completed)
	failedStyle   = lipgloss.NewStyle().Foreground(adaptiveColor("166", "208")) // Orange (job error)
	canceledStyle = lipgloss.NewStyle().Foreground(adaptiveColor("243", "245")) // Gray

	passStyle   = lipgloss.NewStyle().Foreground(adaptiveColor("28", "46"))   // Green
	failStyle   = lipgloss.NewStyle().Foreground(adaptiveColor("124", "196")) // Red (review found issues)
	closedStyle = lipgloss.NewStyle().Foreground(adaptiveColor("30", "51"))   // Cyan

	helpStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("242", "246")) // Gray

	helpKeyStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("242", "246")) // Gray (matches status/scroll text)
	helpDescStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("248", "240")) // Dimmer gray for descriptions

	errorStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("124", "196")).Bold(true) // Red

	flashStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("28", "46")) // Green

	warningFlashStyle = lipgloss.NewStyle().
				Foreground(adaptiveColor("136", "226")).Bold(true) // Yellow/Gold

	updateStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("136", "226")).Bold(true) // Yellow/Gold
)

// reflowHelpRows redistributes items across rows so that when rendered
// as an aligned table (columns sized to the widest cell), the result
// fits within width. Each cell's visible width is key + space + desc,
// and non-first columns add 2 chars (▕ border + padding). If width is
// <= 0, rows are returned unchanged.
func reflowHelpRows(rows [][]helpItem, width int) [][]helpItem {
	if width <= 0 {
		return rows
	}

	// cellWidth returns the visible width of a help item (key + space + desc,
	// or just key when desc is empty).
	cellWidth := func(item helpItem) int {
		w := runewidth.StringWidth(item.key)
		if item.desc != "" {
			w += 1 + runewidth.StringWidth(item.desc)
		}
		return w
	}

	// Find the max items in any single input row.
	maxItemsPerRow := 0
	for _, row := range rows {
		if len(row) > maxItemsPerRow {
			maxItemsPerRow = len(row)
		}
	}

	// Try ncols from max down to 1. For each candidate, chunk every
	// input row into sub-rows of at most ncols items, compute aligned
	// column widths, and check if the total fits within width.
	for ncols := maxItemsPerRow; ncols >= 1; ncols-- {
		var candidate [][]helpItem
		for _, row := range rows {
			for i := 0; i < len(row); i += ncols {
				end := min(i+ncols, len(row))
				candidate = append(candidate, row[i:end])
			}
		}

		// Compute max column widths across all candidate rows.
		colW := make([]int, ncols)
		for _, crow := range candidate {
			for c, item := range crow {
				if w := cellWidth(item); w > colW[c] {
					colW[c] = w
				}
			}
		}

		// Total rendered width.
		total := 0
		for c, w := range colW {
			total += w
			if c > 0 {
				total += 2 // ▕ + padding
			}
		}

		if total <= width {
			return candidate
		}
	}

	// Fallback: one item per row.
	var result [][]helpItem
	for _, row := range rows {
		for _, item := range row {
			result = append(result, []helpItem{item})
		}
	}
	return result
}

// renderHelpTable renders helpItem entries as an aligned table.
// Keys and descriptions are two-tone gray, separated by a thin ▕ border
// that is hidden for column 0 and trailing empty cells.
func renderHelpTable(rows [][]helpItem, width int) string {
	rows = reflowHelpRows(rows, width)
	if len(rows) == 0 {
		return ""
	}

	borderColor := adaptiveColor("248", "242")
	cellStyle := lipgloss.NewStyle()
	// PaddingLeft gaps the ▕ from cell text.
	cellWithBorder := lipgloss.NewStyle().
		PaddingLeft(1).
		Border(lipgloss.Border{Left: "▕"}, false, false, false, true).
		BorderForeground(borderColor)

	// Pad rows to the same number of columns so the table aligns.
	maxCols := 0
	for _, row := range rows {
		if len(row) > maxCols {
			maxCols = len(row)
		}
	}

	// Compute minimum visible width per column.
	colMinW := make([]int, maxCols)
	for _, row := range rows {
		for c, item := range row {
			w := runewidth.StringWidth(item.key)
			if item.desc != "" {
				w += 1 + runewidth.StringWidth(item.desc)
			}
			if w > colMinW[c] {
				colMinW[c] = w
			}
		}
	}

	// Track which cells have content for conditional borders.
	empty := make([][]bool, len(rows))

	t := table.New().
		BorderTop(false).
		BorderBottom(false).
		BorderLeft(false).
		BorderRight(false).
		BorderColumn(false).
		BorderRow(false).
		StyleFunc(func(row, col int) lipgloss.Style {
			minW := 0
			if col < len(colMinW) {
				minW = colMinW[col]
			}
			if col == 0 || (row < len(empty) && col < len(empty[row]) && empty[row][col]) {
				return cellStyle.Width(minW)
			}
			return cellWithBorder.Width(minW + 2) // +2 for ▕ border + padding
		}).
		Wrap(false)

	for ri, row := range rows {
		styled := make([]string, maxCols)
		empty[ri] = make([]bool, maxCols)
		for i, item := range row {
			if item.desc != "" {
				styled[i] = helpKeyStyle.Render(item.key) + " " + helpDescStyle.Render(item.desc)
			} else {
				styled[i] = helpKeyStyle.Render(item.key)
			}
		}
		for i := len(row); i < maxCols; i++ {
			empty[ri][i] = true
		}
		t = t.Row(styled...)
	}

	return t.Render()
}

// fullSHAPattern matches a 40-character hex git SHA (not ranges or branch names)
var fullSHAPattern = regexp.MustCompile(`(?i)^[0-9a-f]{40}$`)

// ansiEscapePattern matches ANSI escape sequences (colors, cursor movement, etc.)
// Handles CSI sequences (\x1b[...X) and OSC sequences terminated by BEL (\x07) or ST (\x1b\\)
var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\]([^\x07\x1b]|\x1b[^\\])*(\x07|\x1b\\)`)

// sanitizeForDisplay strips ANSI escape sequences and control characters from text
// to prevent terminal injection when displaying untrusted content (e.g., commit messages).
func sanitizeForDisplay(s string) string {
	// Strip ANSI escape sequences
	s = ansiEscapePattern.ReplaceAllString(s, "")
	// Strip control characters except newline (\n) and tab (\t)
	var result strings.Builder
	result.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			result.WriteRune(r)
		}
	}
	return result.String()
}

type model struct {
	endpoint         daemon.DaemonEndpoint
	daemonVersion    string
	client           *http.Client
	api              *roborevclient.Client
	glamourStyle     gansi.StyleConfig // detected once at init
	jobs             []storage.ReviewJob
	jobStats         storage.JobStats // aggregate done/closed/open from server
	status           storage.DaemonStatus
	selectedIdx      int
	selectedJobID    int64                          // Track selected job by ID to maintain position on refresh
	expandedPanels   map[string]bool                // panel_run_uuid -> expanded
	panelMembers     map[string][]storage.ReviewJob // panel_run_uuid -> side-fetched members
	currentView      viewKind
	currentReview    *storage.Review
	currentResponses []storage.Response // Responses for current review (fetched with review)
	currentBranch    string             // Cached branch name for current review (computed on load)
	reviewScroll     int
	promptScroll     int
	promptFromQueue  bool // true if prompt view was entered from queue (not review)
	width            int
	height           int
	err              error
	updateAvailable  string // Latest version if update available, empty if up to date
	updateIsDevBuild bool   // True if running a dev build
	versionMismatch  bool   // True if daemon version doesn't match TUI version

	// CLI-locked filters: set via --repo/--branch flags, cannot be cleared by the user
	lockedRepoFilter   bool // true if repo filter was set via --repo flag
	lockedBranchFilter bool // true if branch filter was set via --branch flag

	// Pagination state
	hasMore        bool     // true if there are more jobs to load
	loadingMore    bool     // true if currently loading more jobs (pagination)
	loadingJobs    bool     // true if currently loading jobs (full refresh)
	loadingStatus  bool     // true if currently loading daemon status
	loadingFixJobs bool     // true if currently loading fix jobs
	statusStale    bool     // true if a state change occurred while status fetch was in flight
	fixJobsStale   bool     // true if a state change occurred while fix-jobs fetch was in flight
	fetchGen       uint64   // monotonic generation; incremented on reconnect to discard stale responses
	heightDetected bool     // true after first WindowSizeMsg (real terminal height known)
	fetchSeq       int      // incremented on filter changes; stale fetch responses are discarded
	paginateNav    viewKind // non-zero: auto-navigate in this view after pagination loads

	// Unified tree filter modal state
	filterTree        []treeFilterNode  // Tree of repos (each may have branch children)
	filterFlatList    []flatFilterEntry // Flattened visible rows for navigation
	filterSelectedIdx int               // Currently highlighted row in flat list
	filterSearch      string            // Search/filter text typed by user
	filterSearchSeq   int               // Incremented on search text changes; gates stale fetchFailed
	filterBranchMode  bool              // True when opened via 'b' key (auto-expand repo to branches)

	// Comment modal state
	commentText     string   // The response text being typed
	commentJobID    int64    // Job ID we're responding to
	commentCommit   string   // Short commit SHA for display
	commentFromView viewKind // View to return to after comment modal closes

	// Active filter (applied to queue view)
	activeRepoFilter   []string // Empty = show all, otherwise repo root_paths to filter by
	autoRepoFilter     bool     // true when activeRepoFilter came from auto_filter_repo
	activeBranchFilter string   // Empty = show all, otherwise branch name to filter by
	filterStack        []string // Order of applied filters: "repo", "branch" - for escape to pop in order
	hideClosed         bool     // When true, hide jobs with closed reviews
	// globalCfg is cached at startup and consulted at fetch time so that
	// show_classify_jobs can be resolved against whichever repo is the
	// currently active single-repo filter (rather than baked in once
	// from the cwd at launch).
	globalCfg *config.Config
	// classifyOverride is a session-level toggle that overrides the
	// config-derived show_classify_jobs value. nil = follow config;
	// non-nil = use the override regardless of config.
	classifyOverride *bool
	// classifyEffective caches the resolved show-classify-jobs value
	// so render and fetch paths don't hit disk via LoadRepoConfig on
	// every paint or event. Recomputed by recomputeClassifyEffective
	// whenever classifyOverride or activeRepoFilter changes.
	classifyEffective bool

	// Display name cache (keyed by repo path)
	displayNames map[string]string

	// Repo name lookup (display name → root paths), populated from
	// /api/repos at init. Used by control socket set-filter to resolve
	// display names to the root paths that the daemon API expects.
	repoNames      map[string][]string
	repoIdentities map[string][]string

	// Branch name cache (keyed by job ID) - caches derived branches to avoid repeated git calls
	branchNames map[int64]string

	// Track if branch backfill has run this session (one-time migration)
	branchBackfillDone bool

	// Repo root and branch detected from cwd at launch (for filter sort priority)
	cwdRepoRoot     string
	cwdRepoIdentity string
	cwdBranch       string

	// Pending closed state changes (prevents flash during refresh race)
	// Each pending entry stores the requested state and a sequence number to
	// distinguish between multiple requests for the same state (e.g., true→false→true)
	pendingClosed map[int64]pendingState // job ID -> pending state

	// Flash message (temporary status message shown briefly)
	flashMessage   string
	flashExpiresAt time.Time
	flashView      viewKind // View where flash was triggered (only show in same view)
	flashWarning   bool

	// Track config reload notifications
	lastConfigReloadCounter uint64                 // Last known ConfigReloadCounter from daemon status
	statusFetchedOnce       bool                   // True after first successful status fetch (for flash logic)
	pendingReviewClosed     map[int64]pendingState // review ID -> pending state (for reviews without jobs)
	closedSeq               uint64                 // monotonic counter for request sequencing

	// Daemon reconnection state
	consecutiveErrors int  // Count of consecutive connection failures
	reconnecting      bool // True if currently attempting reconnection

	// Commit message view state
	commitMsgContent  string   // Formatted commit message(s) content
	commitMsgScroll   int      // Scroll position in commit message view
	commitMsgJobID    int64    // Job ID for the commit message being viewed
	commitMsgFromView viewKind // View to return to after closing commit message view

	// Help view state
	helpFromView viewKind // View to return to after closing help
	helpScroll   int      // Scroll position in help view

	// Log view state
	logJobID          int64                // Job being viewed
	logLines          []logLine            // Buffer of output lines
	logScroll         int                  // Scroll position
	logStreaming      bool                 // True if job is still running
	logFromView       viewKind             // View to return to
	logReviewAnchored bool                 // Opened from a review-rooted context
	logFollow         bool                 // True if auto-scrolling to bottom (follow mode)
	logOffset         int64                // Byte offset for next incremental fetch
	logFmtr           *streamfmt.Formatter // Persistent formatter across polls
	logLoading        bool                 // True while a fetch is in-flight
	logFetchSeq       uint64               // Monotonic seq to drop stale responses

	// cmdExpanded toggles full (wrapped) display of the agent command-line
	// header shown in the Log and Prompt views. Collapsed (default) shows a
	// single line truncated to the terminal width.
	cmdExpanded bool

	// Glamour markdown render cache (pointer so View's value receiver can update it)
	mdCache *markdownCache

	distractionFree   bool // hide status line, headers, footer, scroll indicator
	clipboard         ClipboardWriter
	tasksEnabled      bool          // Enables advanced tasks workflow in the TUI
	mouseEnabled      bool          // Enables mouse capture and mouse-driven interactions in the TUI
	noQuit            bool          // Suppress keyboard quit (for managed TUI instances)
	controlSocket     string        // Socket path for runtime metadata updates (empty if disabled)
	ready             chan struct{} // Closed on first Update; signals event loop is running
	sseCh             chan struct{} // Signals from SSE goroutine; nil when external IO disabled
	sseStop           chan struct{} // Close to stop SSE goroutine; nil when external IO disabled
	ssePendingRefresh bool          // True when an SSE event arrived during an in-flight fetch

	// Review view navigation
	reviewFromView viewKind // View to return to when exiting review (queue or tasks)

	// Fix task state
	fixJobs              []storage.ReviewJob // Fix jobs for tasks view
	fixSelectedIdx       int                 // Selected index in tasks view
	fixPromptText        string              // Editable fix prompt text
	fixPromptJobID       int64               // Parent job ID for fix prompt modal
	fixShowHelp          bool                // Show help overlay in tasks view
	patchText            string              // Current patch text for patch viewer
	patchScroll          int                 // Scroll offset in patch viewer
	patchJobID           int64               // Job ID of the patch being viewed
	savePatchInputActive bool                // Whether the save-filename input is visible
	savePatchInput       string              // Current text in the save-filename input

	// Inline fix panel (review view)
	reviewFixPanelOpen    bool // true when fix panel is visible in review view
	reviewFixPanelFocused bool // true when keyboard focus is on the fix panel
	reviewFixPanelPending bool // true when 'F' from queue; panel opens on review load

	worktreeConfirmJobID  int64  // Job ID pending worktree-apply confirmation
	worktreeConfirmBranch string // Branch name for worktree confirmation prompt

	// Column options modal
	colOptionsIdx        int            // Cursor in modal
	colOptionsList       []columnOption // Items in modal (columns + borders toggle)
	colOptionsDirty      bool           // True if options changed since modal opened
	colBordersOn         bool           // Column borders enabled
	hiddenColumns        map[int]bool   // Set of hidden column IDs
	columnOrder          []int          // Ordered toggleable queue columns
	taskColumnOrder      []int          // Ordered task columns
	colOptionsReturnView viewKind       // Return-to view from column options

	// Column width caches (avoid full-scan on every render)
	queueColCache *colWidthCache // cached per-column max widths for queue
	queueColGen   int            // bumped when queue data/filters/columns change
	taskColCache  *colWidthCache // cached per-column max widths for tasks
	taskColGen    int            // bumped when fixJobs/columns change
}

// isConnectionError checks if an error indicates a network/connection failure
// (as opposed to an application-level error like 404 or invalid response).
// Only connection errors should trigger reconnection attempts.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	// Check for url.Error (wraps network errors from http.Client)
	var urlErr *neturl.Error
	if errors.As(err, &urlErr) {
		return true
	}
	// Check for net.Error (timeout, connection refused, etc.)
	var netErr net.Error
	return errors.As(err, &netErr)
}

// newClipboard returns the appropriate ClipboardWriter for the current environment.
// When running over SSH (detected via SSH_TTY, SSH_CLIENT, or SSH_CONNECTION env vars),
// it returns an osc52Clipboard that writes clipboard data through OSC52 terminal escape
// sequences — this works without X11 forwarding and is supported by all modern terminals.
// Otherwise, it falls back to realClipboard which uses the local system clipboard tools.
func newClipboard() ClipboardWriter {
	if os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_CONNECTION") != "" {
		return &osc52Clipboard{output: os.Stderr}
	}
	return &realClipboard{}
}

func newModel(ep daemon.DaemonEndpoint, opts ...option) model {
	var opt options
	for _, o := range opts {
		o(&opt)
	}
	ctx := opt.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	daemonVersion := "?"
	hideClosed := false
	autoFilterRepo := false
	autoFilterBranch := false
	mouseEnabled := true
	tabWidth := 2
	columnBorders := false
	tasksEnabled := false
	hiddenCols := parseHiddenColumns(nil)
	colOrder := parseColumnOrder(nil)
	taskColOrder := parseTaskColumnOrder(nil)
	var cwdRepoRoot, cwdRepoIdentity, cwdBranch string
	var globalCfg *config.Config

	if !opt.disableExternalIO {
		// Read daemon version from runtime file
		if info, err := daemon.GetAnyRunningDaemon(); err == nil && info.Version != "" {
			daemonVersion = info.Version
		}

		// Load preferences from config
		if cfg, err := config.LoadGlobal(); err == nil {
			globalCfg = cfg
			hideClosed = cfg.HideClosedByDefault
			autoFilterRepo = cfg.AutoFilterRepo
			autoFilterBranch = cfg.AutoFilterBranch
			mouseEnabled = cfg.MouseEnabled
			if cfg.TabWidth > 0 {
				tabWidth = cfg.TabWidth
			}
			columnBorders = cfg.ColumnBorders
			tasksEnabled = cfg.Advanced.TasksEnabled

			if migrateColumnConfig(cfg) {
				if err := config.SaveGlobal(cfg); err != nil {
					log.Printf("warning: failed to save migrated config: %v", err)
				}
			}

			hiddenCols = parseHiddenColumns(cfg.HiddenColumns)
			colOrder = parseColumnOrder(cfg.ColumnOrder)
			taskColOrder = parseTaskColumnOrder(cfg.TaskColumnOrder)
		}

		// Detect current repo/branch for filter sort priority
		if repoRoot, err := gitrepo.MainRoot(ctx, "."); err == nil && repoRoot != "" {
			cwdRepoRoot = repoRoot
			cwdRepoIdentity = config.ResolveRepoIdentity(repoRoot, nil)
			cwdBranch = gitrepo.CurrentBranch(ctx, ".")
		}
	}

	var sseCh chan struct{}
	var sseStop chan struct{}
	if !opt.disableExternalIO {
		sseCh = make(chan struct{}, 1)
		sseStop = make(chan struct{})
		go startSSESubscription(ep, sseCh, sseStop)
	}

	// Test overrides for auto-filter simulation
	if opt.autoFilterRepo {
		autoFilterRepo = true
		cwdRepoRoot = opt.cwdRepoRoot
		cwdRepoIdentity = opt.cwdRepoIdentity
	}
	if opt.autoFilterBranch {
		autoFilterBranch = true
		cwdBranch = opt.cwdBranch
	}

	// Determine active filters: CLI flags take priority over auto-filter config
	var activeRepoFilter []string
	var filterStack []string
	var autoRepoFilterActive bool
	var lockedRepo, lockedBranch bool

	if opt.repoFilter != "" {
		activeRepoFilter = []string{opt.repoFilter}
		filterStack = append(filterStack, filterTypeRepo)
		lockedRepo = true
	} else if autoFilterRepo && cwdRepoRoot != "" {
		activeRepoFilter = []string{cwdRepoRoot}
		filterStack = append(filterStack, filterTypeRepo)
		autoRepoFilterActive = true
	}

	var activeBranchFilter string
	if opt.branchFilter != "" {
		activeBranchFilter = opt.branchFilter
		filterStack = append(filterStack, filterTypeBranch)
		lockedBranch = true
	} else if autoFilterBranch && cwdBranch != "" {
		activeBranchFilter = cwdBranch
		filterStack = append(filterStack, filterTypeBranch)
	}

	httpClient := ep.HTTPClient(10 * time.Second)
	m := model{
		endpoint:            ep,
		daemonVersion:       daemonVersion,
		client:              httpClient,
		api:                 newDaemonAPI(ep, httpClient),
		glamourStyle:        streamfmt.GlamourStyle(),
		jobs:                []storage.ReviewJob{},
		currentView:         viewQueue,
		width:               80, // sensible defaults until we get WindowSizeMsg
		height:              24,
		loadingJobs:         true, // Init() calls fetchJobs, so mark as loading
		loadingStatus:       true, // Init() calls fetchStatus, so mark as loading
		hideClosed:          hideClosed,
		globalCfg:           globalCfg,
		activeRepoFilter:    activeRepoFilter,
		autoRepoFilter:      autoRepoFilterActive,
		activeBranchFilter:  activeBranchFilter,
		filterStack:         filterStack,
		lockedRepoFilter:    lockedRepo,
		lockedBranchFilter:  lockedBranch,
		cwdRepoRoot:         cwdRepoRoot,
		cwdRepoIdentity:     cwdRepoIdentity,
		cwdBranch:           cwdBranch,
		displayNames:        make(map[string]string),      // Cache display names to avoid disk reads on render
		branchNames:         make(map[int64]string),       // Cache derived branch names to avoid git calls on render
		pendingClosed:       make(map[int64]pendingState), // Track pending closed changes (by job ID)
		pendingReviewClosed: make(map[int64]pendingState), // Track pending closed changes (by review ID)
		clipboard:           newClipboard(),
		mdCache:             newMarkdownCache(tabWidth),
		tasksEnabled:        tasksEnabled,
		mouseEnabled:        mouseEnabled,
		noQuit:              opt.noQuit,
		ready:               make(chan struct{}),
		sseCh:               sseCh,
		sseStop:             sseStop,
		colBordersOn:        columnBorders,
		hiddenColumns:       hiddenCols,
		columnOrder:         colOrder,
		taskColumnOrder:     taskColOrder,
		queueColCache:       &colWidthCache{gen: -1},
		taskColCache:        &colWidthCache{gen: -1},
		expandedPanels:      map[string]bool{},
		panelMembers:        map[string][]storage.ReviewJob{},
	}
	// Seed the cached classify-visibility decision once so render and
	// fetch can read m.classifyEffective without hitting disk.
	m.recomputeClassifyEffective()
	return m
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		tea.RequestWindowSize,
		m.displayTick(),
		m.tick(),
		m.fetchJobs(),
		m.fetchStatus(),
		m.fetchRepoNames(),
		m.checkForUpdate(),
	}
	if m.sseCh != nil {
		cmds = append(cmds, waitForSSE(m.sseCh, m.sseStop))
	}
	return tea.Batch(cmds...)
}

func (m model) tasksWorkflowEnabled() bool {
	return m.tasksEnabled
}

func (m model) tasksDisabledMessage() string {
	return "Tasks workflow disabled. Set advanced.tasks_enabled=true in global config to enable it."
}

// shouldShowClassifyJobs returns the cached effective value computed
// by recomputeClassifyEffective. Hot path: callable from render and
// fetch without filesystem I/O.
func (m model) shouldShowClassifyJobs() bool {
	return m.classifyEffective
}

// recomputeClassifyEffective resolves whether the queue should include
// auto-design-router classifier rows and skipped design rows, and
// caches the result in m.classifyEffective.
//
// Precedence: a session-level toggle (the 's' key) takes priority over
// config. When unset, falls back to the per-repo override if the queue
// is filtered to a single repo, otherwise the global setting. This
// avoids the surprise of one repo's override leaking into a multi-repo
// view while still letting the user flip the toggle on the fly.
//
// Call this from any handler that mutates classifyOverride or
// activeRepoFilter. It only loads repo config from disk under the
// single-repo-filter branch, so amortizing it to filter/toggle changes
// keeps the render and fetch paths I/O-free.
func (m *model) recomputeClassifyEffective() {
	switch {
	case m.classifyOverride != nil:
		m.classifyEffective = *m.classifyOverride
	case len(m.activeRepoFilter) == 1:
		m.classifyEffective = config.ResolveShowClassifyJobs(
			m.activeRepoFilter[0], m.globalCfg,
		)
	case m.globalCfg != nil:
		m.classifyEffective = m.globalCfg.ShowClassifyJobs
	default:
		m.classifyEffective = false
	}
}

// getDisplayName returns the display name for a repo, using the cache.
// Falls back to loading from config on cache miss, then to the provided default name.
func (m *model) getDisplayName(repoPath, defaultName string) string {
	if repoPath == "" {
		return defaultName
	}
	if displayName, ok := m.displayNames[repoPath]; ok {
		if displayName != "" {
			return displayName
		}
		return defaultName
	}
	// Cache miss - load from config (handles reviews for repos not in jobs list)
	displayName := config.GetDisplayName(repoPath)
	m.displayNames[repoPath] = displayName
	if displayName != "" {
		return displayName
	}
	return defaultName
}

// updateDisplayNameCache refreshes display names for the given repo paths.
// Called on each jobs fetch to pick up config changes without restart.
func (m *model) updateDisplayNameCache(jobs []storage.ReviewJob) {
	for _, repoPath := range uniqueJobRepoPaths(jobs) {
		// Always refresh to pick up config changes
		m.displayNames[repoPath] = config.GetDisplayName(repoPath)
	}
}

func uniqueJobRepoPaths(jobs []storage.ReviewJob) []string {
	seen := make(map[string]struct{})
	paths := make([]string, 0)
	for _, job := range jobs {
		if job.RepoPath == "" {
			continue
		}
		if _, ok := seen[job.RepoPath]; ok {
			continue
		}
		seen[job.RepoPath] = struct{}{}
		paths = append(paths, job.RepoPath)
	}
	return paths
}

// getBranchForJob returns the branch name for a job, falling back to git lookup
// if the stored branch is empty and the repo is available locally.
// Results are cached to avoid repeated git calls on render.
func (m *model) getBranchForJob(job storage.ReviewJob) string {
	// Use stored branch if available
	if job.Branch != "" {
		return job.Branch
	}

	// Check cache for previously derived branch (if cache exists)
	if m.branchNames != nil {
		if cached, ok := m.branchNames[job.ID]; ok {
			return cached
		}
	}

	// For task jobs (run, analyze, custom) or dirty jobs, no branch makes sense
	if job.IsTaskJob() || job.IsDirtyJob() {
		return ""
	}

	// Fall back to git lookup if repo path exists locally and we have a SHA
	// Only try if repo path is set and is not from a remote machine
	if job.RepoPath == "" || (m.status.MachineID != "" && job.SourceMachineID != "" && job.SourceMachineID != m.status.MachineID) {
		// Don't cache - repo might become available later
		return ""
	}

	// Check if repo exists locally before attempting git lookup
	// Return early on any error (not exists, permission denied, I/O failure)
	// to avoid caching incorrect results
	if _, err := os.Stat(job.RepoPath); err != nil {
		return ""
	}

	// For ranges (SHA..SHA), use the end SHA
	sha := job.GitRef
	if idx := strings.Index(sha, ".."); idx != -1 {
		sha = sha[idx+2:]
	}

	branch := git.GetBranchName(job.RepoPath, sha)
	// Cache result (including empty for detached HEAD / commit not on branch)
	// We only skip caching above when repo doesn't exist yet
	if m.branchNames != nil {
		m.branchNames[job.ID] = branch
	}
	return branch
}

// mouseDisabledView returns true for views where mouse capture should be
// released to allow native terminal text selection (copy/paste).
func mouseDisabledView(v viewKind) bool {
	switch v {
	case viewLog, viewReview, viewKindPrompt, viewPatch, viewCommitMsg:
		return true
	}
	return false
}

func mouseCaptureEnabled(v viewKind, mouseEnabled bool) bool {
	return mouseEnabled && !mouseDisabledView(v)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Signal that the event loop is running (once). The control
	// listener waits on this before accepting connections.
	if m.ready != nil {
		select {
		case <-m.ready:
		default:
			close(m.ready)
		}
	}

	var result tea.Model
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		result, cmd = m.handleKeyMsg(msg)
	case tea.MouseMsg:
		if !m.mouseEnabled {
			return m, nil
		}
		result, cmd = m.handleMouseMsg(msg)
	case tea.WindowSizeMsg:
		result, cmd = m.handleWindowSizeMsg(msg)
	case displayTickMsg:
		return m, m.displayTick()
	case tickMsg:
		result, cmd = m.handleTickMsg(msg)
	case logTickMsg:
		result, cmd = m.handleLogTickMsg(msg)
	case jobsMsg:
		result, cmd = m.handleJobsMsg(msg)
	case statusMsg:
		result, cmd = m.handleStatusMsg(msg)
	case updateCheckMsg:
		result, cmd = m.handleUpdateCheckMsg(msg)
	case reviewMsg:
		result, cmd = m.handleReviewMsg(msg)
	case promptMsg:
		result, cmd = m.handlePromptMsg(msg)
	case logOutputMsg:
		result, cmd = m.handleLogOutputMsg(msg)
	case closedMsg:
		result, cmd = m.handleClosedToggleMsg(msg)
	case closedResultMsg:
		result, cmd = m.handleClosedResultMsg(msg)
	case cancelResultMsg:
		result, cmd = m.handleCancelResultMsg(msg)
	case rerunResultMsg:
		result, cmd = m.handleRerunResultMsg(msg)
	case repoNamesMsg:
		result, cmd = m.handleRepoNamesMsg(msg)
	case reposMsg:
		result, cmd = m.handleReposMsg(msg)
	case repoBranchesMsg:
		result, cmd = m.handleRepoBranchesMsg(msg)
	case branchesMsg:
		result, cmd = m.handleBranchesMsg(msg)
	case commentResultMsg:
		result, cmd = m.handleCommentResultMsg(msg)
	case clipboardResultMsg:
		result, cmd = m.handleClipboardResultMsg(msg)
	case commitMsgMsg:
		result, cmd = m.handleCommitMsgMsg(msg)
	case jobsErrMsg:
		result, cmd = m.handleJobsErrMsg(msg)
	case paginationErrMsg:
		result, cmd = m.handlePaginationErrMsg(msg)
	case statusErrMsg:
		result, cmd = m.handleStatusErrMsg(msg)
	case errMsg:
		result, cmd = m.handleErrMsg(msg)
	case reconnectMsg:
		result, cmd = m.handleReconnectMsg(msg)
	case sseEventMsg:
		result, cmd = m.handleSSEEventMsg()
	case fixJobsMsg:
		result, cmd = m.handleFixJobsMsg(msg)
	case fixTriggerResultMsg:
		result, cmd = m.handleFixTriggerResultMsg(msg)
	case patchMsg:
		result, cmd = m.handlePatchResultMsg(msg)
	case panelMembersMsg:
		result, cmd = m.handlePanelMembersMsg(msg)
	case applyPatchResultMsg:
		result, cmd = m.handleApplyPatchResultMsg(msg)
	case savePatchResultMsg:
		result, cmd = m.handleSavePatchResultMsg(msg)
	case configSaveErrMsg:
		m.colOptionsDirty = true
		m.setFlash(
			"Config save failed: "+msg.err.Error(),
			5*time.Second, m.currentView,
		)
		result = m
	case controlSocketReadyMsg:
		m.controlSocket = msg.socketPath
		result = m
	case controlQueryMsg:
		result, cmd = m.handleControlQuery(msg)
	case controlMutationMsg:
		result, cmd = m.handleControlMutation(msg)
	default:
		// Unknown message types cannot change the view, so no mouse
		// toggle is needed. If a new message type is added that can
		// change currentView, add a case above.
		return m, nil
	}

	return result, cmd
}

func (m model) viewContent() string {
	if m.currentView == viewKindComment {
		return m.renderRespondView()
	}
	if m.currentView == viewFilter {
		return m.renderFilterView()
	}
	if m.currentView == viewCommitMsg {
		return m.renderCommitMsgView()
	}
	if m.currentView == viewHelp {
		return m.renderHelpView()
	}
	if m.currentView == viewLog {
		return m.renderLogView()
	}
	if m.currentView == viewTasks {
		return m.renderTasksView()
	}
	if m.currentView == viewKindWorktreeConfirm {
		return m.renderWorktreeConfirmView()
	}
	if m.currentView == viewPatch {
		return m.renderPatchView()
	}
	if m.currentView == viewColumnOptions {
		return m.renderColumnOptionsView()
	}
	if m.currentView == viewKindPrompt && m.currentReview != nil {
		return m.renderPromptView()
	}
	if m.currentView == viewReview && m.currentReview != nil {
		return m.renderReviewView()
	}
	return m.renderQueueView()
}

func (m model) View() tea.View {
	v := tea.NewView(m.viewContent())
	v.AltScreen = true
	if mouseCaptureEnabled(m.currentView, m.mouseEnabled) {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

// helpLines builds the help content lines from shortcut definitions.

// helpMaxScroll returns the maximum scroll offset for the help view.

// Config holds resolved parameters for running the TUI.
type Config struct {
	Context       context.Context
	Endpoint      daemon.DaemonEndpoint
	RepoFilter    string
	BranchFilter  string
	ControlSocket string // Unix socket path for external control (default: auto)
	NoQuit        bool   // Suppress keyboard quit (for managed TUI instances)
}

func programOptionsForModel(m model) []tea.ProgramOption {
	return nil
}

// Run starts the interactive TUI.
func Run(cfg Config) error {
	// Clean up sockets from dead TUI processes before starting
	CleanupStaleTUIRuntimes()

	var opts []option
	if cfg.Context != nil {
		opts = append(opts, withContext(cfg.Context))
	}
	if cfg.RepoFilter != "" {
		opts = append(opts, withRepoFilter(cfg.RepoFilter))
	}
	if cfg.BranchFilter != "" {
		opts = append(opts, withBranchFilter(cfg.BranchFilter))
	}
	if cfg.NoQuit {
		opts = append(opts, withNoQuit())
	}
	// Resolve socket path before creating the model so the
	// model knows its socket path for runtime metadata updates.
	socketPath := cfg.ControlSocket
	if socketPath == "" {
		socketPath = defaultControlSocketPath()
		// Only tighten directory permissions for the default
		// managed directory. Custom --control-socket paths may
		// point anywhere (e.g. /tmp, repo root) and mutating
		// their parent would be destructive.
		if err := ensureSocketDir(
			filepath.Dir(socketPath),
		); err != nil {
			log.Printf("warning: control socket disabled: %v", err)
			socketPath = ""
		}
	}

	m := newModel(cfg.Endpoint, opts...)
	p := tea.NewProgram(
		m,
		programOptionsForModel(m)...,
	)

	// Start the control listener after the event loop is running
	// so that p.Send (used by control handlers) never blocks. The
	// model closes m.ready on its first Update call. Skip if
	// socketPath is empty (ensureSocketDir failed).
	// cleanupDone is closed after socket/runtime cleanup finishes
	// so Run() can wait for it before returning, preventing stale
	// files if the process exits immediately after p.Run().
	// programDone is closed after p.Run() returns so the goroutine
	// can unblock if the program exits before m.ready fires.
	cleanupDone := make(chan struct{})
	programDone := make(chan struct{})
	if socketPath != "" {
		go func() {
			defer close(cleanupDone)

			// Wait for either the event loop to start or the
			// program to exit (e.g. terminal init failure).
			select {
			case <-m.ready:
			case <-programDone:
				return
			}

			cleanup, err := startControlListener(socketPath, p)
			if err != nil {
				log.Printf(
					"warning: control socket disabled: %v", err,
				)
				return
			}

			// Notify the model that the control socket is
			// active so it can update runtime metadata on
			// reconnect. This is deferred until after the
			// listener succeeds to avoid advertising a
			// socket that failed to bind.
			p.Send(controlSocketReadyMsg{
				socketPath: socketPath,
			})

			rtInfo := buildTUIRuntimeInfo(
				socketPath, cfg.Endpoint.ConfigAddr(),
			)
			if err := WriteTUIRuntime(rtInfo); err != nil {
				log.Printf(
					"warning: failed to write TUI runtime info: %v",
					err,
				)
			}

			// Block until the program exits, then clean up.
			p.Wait()
			cleanup()
			RemoveTUIRuntime()
		}()
	} else {
		close(cleanupDone)
	}

	finalModel, err := p.Run()
	// Stop SSE subscription goroutine. Use the final model (not the
	// initial m) because reconnect may have replaced sseStop — closing
	// the original would double-close.
	if fm, ok := finalModel.(model); ok && fm.sseStop != nil {
		close(fm.sseStop)
	}
	close(programDone)
	<-cleanupDone
	return err
}

// renderTasksView renders the background fix tasks list.

// fetchPatch fetches the patch for a fix job from the daemon.

// renderWorktreeConfirmView renders the worktree creation confirmation modal.
