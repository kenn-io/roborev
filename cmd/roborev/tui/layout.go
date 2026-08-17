package tui

import (
	"io"
	"time"

	tea "charm.land/bubbletea/v2"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
)

type layoutMode int

const (
	layoutStacked layoutMode = iota
	layoutSplit
)

func (l layoutMode) String() string {
	if l == layoutSplit {
		return "split"
	}
	return "stacked"
}

type focusPane int

const (
	focusList focusPane = iota
	focusDetail
)

func (f focusPane) String() string {
	if f == focusDetail {
		return "detail"
	}
	return "list"
}

// Split layout breakpoints and pane sizing. Detail-first allocation: the
// review pane reserves splitDetailReservedWidth cells; the list takes the
// remainder clamped to [splitListMinWidth, splitListMaxWidth].
const (
	splitMinWidth            = 140
	splitMinHeight           = 36
	splitDetailReservedWidth = 100
	splitListMinWidth        = 50
	splitListMaxWidth        = 90
)

// detailFollowDebounce coalesces held-down queue navigation into a single
// detail fetch (kata uses the same value).
const detailFollowDebounce = 75 * time.Millisecond

// splitGeom is the single source of truth for split-layout pane math.
// Outer sizes include the 1-cell border on each side; inner sizes are the
// content area inside the border.
type splitGeom struct {
	listOuterW, detailOuterW, bodyH                    int
	listInnerW, listInnerH, detailInnerW, detailInnerH int
}

// splitGeometry computes pane rectangles for the given terminal size.
// Bands: title(1) + body(bodyH) + info(1) + footer(footerLines).
func splitGeometry(width, height, footerLines int) splitGeom {
	listW := min(max(width-splitDetailReservedWidth, splitListMinWidth), splitListMaxWidth)
	detailW := width - listW
	bodyH := max(height-2-footerLines, 5)
	return splitGeom{
		listOuterW:   listW,
		detailOuterW: detailW,
		bodyH:        bodyH,
		listInnerW:   listW - 2,
		listInnerH:   bodyH - 2,
		detailInnerW: detailW - 2,
		detailInnerH: bodyH - 2,
	}
}

func pickLayout(width, height int) layoutMode {
	if width >= splitMinWidth && height >= splitMinHeight {
		return layoutSplit
	}
	return layoutStacked
}

// resolveLayout picks the layout for the current terminal size, honoring a
// manual L-toggle lock: a locked stacked preference always wins; a locked
// split preference engages whenever the terminal fits and degrades
// gracefully when it doesn't. Distraction-free mode overrides everything:
// its contract is title plus job list ONLY (docs/integrations/tui.md), and
// the split composition's detail pane, borders, info line and footer all
// violate it -- so while it is active the layout is stacked regardless of
// terminal size or lock, and every consumer keyed on m.layout (rendering,
// mouse routing, tab, reconcile) follows automatically.
func (m model) resolveLayout() layoutMode {
	if m.distractionFree {
		return layoutStacked
	}
	fits := pickLayout(m.width, m.height)
	if m.layoutLocked && m.preferredLayout == layoutStacked {
		return layoutStacked
	}
	return fits
}

// splitActive reports whether the split composition should render: split
// layout is on and the current view is one of the two split-rooted views.
// Transient views (prompt, comment, help, log, ...) remain full-screen. A
// tasks-origin review (reviewFromView == viewTasks) is ALSO excluded even
// though currentView == viewReview: fix tasks live in m.fixJobs, not
// m.jobs, so the split's list pane (which only ever shows m.jobs rows) and
// renderDetailPane's m.selectedJob() lookup (m.jobs/panelMembers-only)
// have nothing to resolve selectedJobID against -- it would render "No job
// selected" instead of the loaded review. A tasks-origin review renders
// full-screen via the ordinary review renderer instead, same as any other
// transient view.
func (m model) splitActive() bool {
	if m.currentView == viewReview && m.reviewFromView == viewTasks {
		return false
	}
	return m.layout == layoutSplit &&
		(m.currentView == viewQueue || m.currentView == viewReview)
}

// applyLayout transitions the layout, mapping view<->focus in both
// directions so selection and the open review survive the flip. When a
// transient view (help, log, comment editor, ...) is open, only m.layout is
// updated -- focus/currentView/currentReview are left untouched so the
// transient view's in-progress state (e.g. a comment being typed) survives
// a resize. normalizeSplitState reconciles focus once such a view exits.
func (m *model) applyLayout(target layoutMode) {
	if m.layout == target {
		return
	}
	m.layout = target
	if target != layoutSplit && m.paneLogStreaming {
		// Leaving split kills the pane log's poll chain: the pending
		// paneLogTickMsg is dropped by handlePaneLogTickMsg's layout gate
		// and nothing re-arms it. Invalidate the tail explicitly (the same
		// idiom handleWindowSizeMsg uses when the pane stops being the
		// visible thing) so the model stops claiming an active tail --
		// otherwise toggling back to split with the same running job still
		// selected finds startPaneLog's "already tailing this job" early
		// return and splitReconcileDetail's `!paneLogStreaming` guard both
		// satisfied, and the pane sits on frozen stale lines until the job
		// completes. Bumping the seq also rejects any response still in
		// flight from the tail we just abandoned. Done before the
		// transient-view early return below: the poll chain dies whether
		// or not a transient view happens to be open over the split.
		m.paneLogSeq++
		m.paneLogStreaming = false
	}
	if m.currentView != viewQueue && m.currentView != viewReview {
		return
	}
	if target == layoutSplit {
		if m.currentView == viewReview && m.currentReview != nil {
			m.focus = focusDetail
		} else {
			m.focus = focusList
			if m.currentView == viewReview {
				m.currentView = viewQueue
			}
		}
		return
	}
	// Leaving split: detail focus keeps the full-screen review open --
	// but only when the loaded review is the selected job's CURRENT
	// attempt (selectedReviewLoaded). A bare non-nil check would carry a
	// stale attempt's review (external rerun observed, refetch not yet
	// landed) into stacked full-screen, where splitReconcileDetail no
	// longer runs to replace it and the stacked review actions
	// (close/comment/fix) would target the obsolete attempt indefinitely.
	// A tasks-origin review is preserved unconditionally: its fix job
	// never resolves in m.jobs, so selectedReviewLoaded is false for it by
	// construction, yet the review on screen is exactly what the user is
	// reading and stacked continues to render it the same way.
	if m.focus == focusDetail && m.currentReview != nil &&
		(m.reviewFromView == viewTasks || m.selectedReviewLoaded()) {
		m.currentView = viewReview
	} else {
		m.currentView = viewQueue
		m.currentReview = nil
		m.reviewScroll = 0
		// The displayed review was just discarded, and an OPEN panel is
		// bound to whatever review is displayed (fixPromptJobID stamped
		// when it opened). Left open, the next review to render in stacked
		// -- e.g. Enter on a failed job, whose synchronous synthesis
		// assigns currentReview directly without acceptReview's wrong-job
		// panel close -- would show the panel still bound to the previous
		// job, and submitting would start a fix for a job no longer on
		// screen. A PENDING intent is left alone: it is bound to the
		// SELECTION, which this branch does not move, and its response is
		// consumed through acceptReview, which re-binds correctly.
		if m.reviewFixPanelOpen {
			m.closeFixPanel()
		}
	}
}

// followSelectionChange is the shared chokepoint every selection mutation
// runs through. It does two distinct things, and only one of them is a
// split-layout concern:
//
//  1. ABANDONMENT (any layout): a genuine selection change abandons the
//     old job's pending intents -- the pending-open request and a fix
//     panel bound to it (outside split, only a PENDING panel: an open one
//     is bound to the review displayed full-screen, not to the queue
//     selection -- see the inline comment below) -- and invalidates that
//     job's in-flight dispatch by bumping detailFollowGen. The abandonment rule is "the selection
//     leaving a job abandons its pending requests, whoever moves it";
//     layout is irrelevant to it. Gated on split layout, stacked mode
//     would let a user press Enter or F on job X, navigate to Y, return
//     to X before the response arrived, and have the abandoned response
//     accepted -- unexpectedly opening X's review or fix panel with no
//     fresh keypress. The gen bump rejects that response, and the
//     disarms make it un-rescuable.
//  2. DETAIL FOLLOW (split only): scheduleDetailFollow arms the debounced
//     fetch for the new selection, stops the old job's pane log tail and
//     clears splitDetailErr. Stacked has no pane, so none of that
//     applies; only the abandonment above runs.
//
// Paths that mutate the selection outside keyboard/mouse navigation route
// through here too: the control socket's close-review and cancel-job
// handlers move the selection when hideClosed hides the job they just
// acted on, and the optimistic-operation rollbacks
// (handleClosedResultMsg/handleCancelResultMsg) restore it when the
// server rejects the operation. Left to decide for themselves, each would
// leave the detail pane loaded for the old job until some later jobs
// refresh happened to reconcile it.
//
// Callers pass the selection as it was BEFORE their mutation and must batch
// the returned cmd with their own. Any code that mutates the selection must
// come through here (or, for handleJobsMsg's normalization and the filter
// reset, perform the same abandonment inline); calling
// scheduleDetailFollow directly is reserved for same-selection callers
// (maybeBootstrapDetail), which have nothing to abandon.
func (m model) followSelectionChange(prevSelected int64) (model, tea.Cmd) {
	if m.selectedJobID == prevSelected {
		return m, nil
	}
	// The guard above is what proves the selection GENUINELY changed --
	// the precondition both disarms require (a same-job bootstrap must
	// NOT reach these). See disarmPendingReviewOpen's doc.
	m.disarmPendingReviewOpen()
	m.abandonInFlightSelectionRequests()
	if m.layout != layoutSplit {
		// Outside split only the PENDING half of the fix intent is
		// selection-bound (the same asymmetry as handleJobsMsg's
		// normalization epilogue): F was pressed with the selection on the
		// intent's job and no review on screen yet, so the selection moving
		// off it abandons the request. An OPEN panel is different outside
		// split -- it is bound to the review displayed full-screen, and the
		// selection can move under it without the display changing (a
		// control-socket close of a hidden job, a rollback), so closing it
		// here would dump an in-progress fix prompt for a review the user
		// is still looking at. Displayed-review changes close open panels
		// at their own sites (review nav, the wrong-job acceptance guard).
		if m.reviewFixPanelPending && !m.reviewFixPanelOpen &&
			m.fixPromptJobID != m.selectedJobID {
			m.closeFixPanel()
		}
		// A detailFollowGen bumper -- classified ABANDONMENT, disarms
		// performed inline above per the field's contract (tui.go). The
		// bump dooms the abandoned dispatch's response at the gen gate;
		// with the intents disarmed it is also un-rescuable
		// (reviewIntentRescuable requires an armed intent). No tick is
		// scheduled: there is no pane to follow in stacked, and
		// handleDetailFollowTick would drop it anyway.
		m.detailFollowGen++
		return m, nil
	}
	m.closeFixPanelIfJobChanged()
	return m.scheduleDetailFollow()
}

// abandonInFlightSelectionRequests dooms the request-scoped state a genuine
// selection change leaves behind, shared by every abandonment site --
// followSelectionChange, handleJobsMsg's normalization epilogue, and
// resetQueueForFilterChange. Like disarmPendingReviewOpen it has no
// self-guard: callers must only invoke it when the selection GENUINELY
// changed.
//
//   - promptFetchSeq: an in-flight 'p' response must not be accepted if a
//     later refetch re-selects the abandoned job before it lands -- every
//     handlePromptMsg gate (jobID, attempt, origin) passes again in that
//     state, and only the bumped identity rejects it. See promptFetchSeq's
//     doc comment (tui.go).
//   - reconcileFetchJobID/Seq: the suppression slot was armed for the era
//     being abandoned; its response will be gen-rejected on arrival, so
//     left set it only SUPPRESSES the re-selected job's replacement fetch
//     until another jobs refresh, stalling the pane at "Loading review...".
//     Same belt-and-suspenders clear scheduleDetailFollow performs.
//
// detailFollowGen bumps stay at the call sites: followSelectionChange's
// split branch delegates its bump to scheduleDetailFollow, so folding gen
// in here would double-count responsibility for it.
func (m *model) abandonInFlightSelectionRequests() {
	m.promptFetchSeq++
	m.reconcileFetchJobID = 0
	m.reconcileFetchSeq = 0
}

// disarmPendingReviewOpen clears the pending-open intent
// (pendingReviewOpenJobID/Origin/Seq, tui.go). Unlike closeFixPanelIfJobChanged,
// this has no per-call job comparison to make it self-guarding -- it doesn't
// compare against anything, it just clears -- so callers must only invoke it
// at a point where the selection is ALREADY KNOWN to have genuinely changed.
//
// This deliberately does NOT live in scheduleDetailFollow:
// scheduleDetailFollow cannot tell a real selection change from a same-job
// bootstrap (maybeBootstrapDetail calls it on a plain layout toggle, where
// the selection is unchanged and any armed intent is still valid --
// clearing it there would silently discard the user's explicit Enter). It
// lives in the code that can tell: followSelectionChange, behind its
// `selectedJobID != prevSelected` guard, which every selection-changing
// path routes through (the handleKeyMsg wrapper, the step*Nav helpers, the
// pagination arms, tasks Enter/'P', handleSplitMouse, the control-socket
// handlers and the rollback restores), plus the inline sites that perform
// the same abandonment without scheduling a follow (handleJobsMsg's
// normalization and resetQueueForFilterChange).
func (m *model) disarmPendingReviewOpen() {
	m.pendingReviewOpenJobID = 0
	m.pendingReviewOpenOrigin = 0
	m.pendingReviewOpenSeq = 0
}

// scheduleDetailFollow arms the debounced detail fetch after a queue cursor
// move in split layout. The generation counter cancels earlier ticks.
func (m model) scheduleDetailFollow() (model, tea.Cmd) {
	// A detailFollowGen bumper -- see that field's doc comment (tui.go)
	// for the contract every bumper must satisfy. This one is incidental
	// refresh, not abandonment, so it must not (and does not) disarm
	// either pending intent.
	m.detailFollowGen++
	gen := m.detailFollowGen
	m.reviewScroll = 0
	m.splitDetailErr = nil
	// The selection moved to a different job -- invalidate any in-flight
	// pane log fetch/poll for the job we just navigated away from by
	// bumping paneLogSeq, so a response for it (including a late-arriving
	// error) fails handlePaneLogOutputMsg/handlePaneLogTickMsg's seq gate
	// instead of landing after the fact and getting misattributed (via
	// splitDetailErr) to the newly selected job. paneLogSeq alone gates
	// those handlers, not jobID, so this is the one place that must catch
	// every selection change.
	if m.paneLogStreaming && m.paneLogJobID != m.selectedJobID {
		m.paneLogSeq++
		m.paneLogStreaming = false
	}
	// NEITHER pending intent (pendingReviewOpen*, the fix panel) is
	// touched here. Both disarms live in followSelectionChange, behind
	// its selection-actually-changed guard, because this function cannot
	// tell a real selection change from a same-job bootstrap: it is
	// called both by followSelectionChange (abandonment already performed
	// by the time it gets here) and by maybeBootstrapDetail on a plain
	// layout toggle, where the selection is UNCHANGED and any armed
	// intent belongs to a request that is still valid -- the rescue path,
	// reviewIntentRescuable, exists to serve exactly that surviving
	// intent past this function's own gen bump. A future direct caller
	// with a genuine selection change belongs in followSelectionChange,
	// not here.
	// The gen bump above already dooms any in-flight reconcile-dispatched
	// response for the PREVIOUS selection -- it'll fail handleReviewMsg's
	// gen check, and both handlers release the suppression slot on any
	// response for the job before checking anything, so this isn't
	// load-bearing for correctness. Kept as belt-and-suspenders: it clears
	// the slot for the OLD job IMMEDIATELY, synchronously, rather than
	// waiting for that doomed response to actually arrive over the network
	// -- narrowing the window where a genuine need to re-dispatch for that
	// same job, were the user to return to it before the stale response
	// lands, would otherwise still be (correctly, if unnecessarily)
	// suppressed. Same idiom as paneLogStreaming above.
	m.reconcileFetchJobID = 0
	m.reconcileFetchSeq = 0
	return m, tea.Tick(detailFollowDebounce, func(time.Time) tea.Msg {
		return detailFollowTickMsg{gen: gen}
	})
}

// handleDetailFollowTick resolves a follow tick: fetch the review for a
// done job, synthesize the error review for a failed job, start the pane
// log for a running job. Stale generations are dropped.
//
// The Done branch's idempotency check is selectedReviewLoaded, not a bare
// JobID match: renderDetailPane gates the review body on the same
// predicate, so a stale-but-matching review (the selected job was rerun
// and completed while the selection was elsewhere, then re-selected)
// renders as "Loading review..." -- skipping the fetch here would leave
// that placeholder stalled with nothing in flight until the next fallback
// poll. The shared predicate makes "shows loading" imply "a fetch is
// coming". The Failed branch still doesn't stop an active tail, unlike
// splitReconcileDetail's twin: this only runs on a fresh selection change,
// and reconcile self-heals that narrower imprecision within one refresh.
func (m model) handleDetailFollowTick(msg detailFollowTickMsg) (tea.Model, tea.Cmd) {
	if msg.gen != m.detailFollowGen || m.layout != layoutSplit {
		return m, nil
	}
	job, ok := m.selectedJob()
	if !ok {
		return m, nil
	}
	switch job.Status {
	case storage.JobStatusDone:
		if m.selectedReviewLoaded() {
			return m, nil
		}
		// Clear any earlier splitDetailErr before issuing the follow fetch
		// so a stale error from a previous job/attempt can't briefly show
		// through for this one while the fresh fetch is in flight.
		m.splitDetailErr = nil
		// Fires once per selection-change debounce, not
		// splitReconcileDetail's every-refresh dispatch, so it needs no
		// duplicate-dispatch suppression (it never touches
		// reconcileFetchJobID) -- but it is ordered like every other
		// review fetch: dispatchReviewFollow bumps the shared epoch and
		// stamps it (see reviewFetchSeq's doc comment, tui.go).
		cmd := m.dispatchReviewFollow(job.ID)
		return m, cmd
	case storage.JobStatusFailed:
		// Synchronous rebuild routed through the shared ordered acceptance
		// (epoch bump + acceptReview) so a pre-rerun in-flight response
		// cannot overwrite it -- see acceptSynthesizedFailure. The
		// returned cmd loads the job's persisted comments.
		cmd := m.acceptSynthesizedFailure(job.ID, synthesizeFailedReview(job, m.currentReview))
		return m, cmd
	case storage.JobStatusRunning:
		return m.startPaneLog(*job)
	}
	return m, nil
}

// synthesizeFailedReview builds the synthetic review shown for a failed job
// in the split detail pane, shared by handleDetailFollowTick (selection
// change) and splitReconcileDetail (jobs refresh) so both render identically
// through renderReviewPaneBody's scrollable pane-body path rather than
// renderDetailPane's card+wrapped-error fallback.
//
// prev is the review currently displayed (m.currentReview, may be nil).
// When it belongs to this same job and carries a prompt, the prompt is
// preserved on the synthesized review: the prompt view over a queued/
// running job renders a synthetic review built from the row's Prompt
// (handlePromptKey/stepPromptNav), and the daemon strips terminal jobs'
// prompts from job listings (stripJobPrompts) -- so once the job fails,
// that displayed review is the LAST copy of the prompt the TUI will ever
// hold, and replacing it wholesale would blank an open prompt view
// unrecoverably. The same-job match accepts prev.Job.ID as well as
// prev.JobID because those synthetic prompt reviews set only the embedded
// Job, not the JobID field.
func synthesizeFailedReview(job *storage.ReviewJob, prev *storage.Review) *storage.Review {
	jobCopy := *job
	fresh := &storage.Review{
		JobID:  job.ID,
		Agent:  job.Agent,
		Prompt: job.Prompt,
		Output: "Job failed:\n\n" + job.Error,
		Job:    &jobCopy,
	}
	if fresh.Prompt == "" && prev != nil && prev.Prompt != "" &&
		(prev.JobID == job.ID || (prev.Job != nil && prev.Job.ID == job.ID)) {
		fresh.Prompt = prev.Prompt
	}
	return fresh
}

// acceptSynthesizedFailure joins the synchronous failed-job rebuild into
// the same ordered acceptance mechanism as fetched reviews. Bumping the
// shared fetch epoch first makes every older in-flight response land
// fetchSeq-superseded -- without it, a pre-rerun reviewMsg dispatched for
// this job (an EXTERNAL rerun bumps no jobAttemptGen and moves no
// selection, so the attempt/gen/jobID gates all still pass) could arrive
// after this rebuild and overwrite the synthesized failure with the
// previous attempt's review until a later refresh corrected it. Routing
// the content through acceptReview keeps every acceptance side effect --
// sibling clears, scroll reset, the scoped paneReviewSeenNonTerminalJob
// clear, wrong-job panel close, and the pending-intent consumes with
// their guarded view switches -- identical to a fetched landing. A
// superseded same-job response arriving later finds content already
// present and at most serves its own open intent via handleReviewMsg's
// fallback switch; it can no longer replace content.
//
// The returned cmd loads the job's persisted comments: acceptance clears
// currentResponses, and no review fetch can ever repopulate them for a
// synthesized review (the /api/review request 404s), so without this a
// navigation round-trip or a fresh TUI would show the failure with its
// comments permanently missing. Callers must run the cmd.
func (m *model) acceptSynthesizedFailure(jobID int64, fresh *storage.Review) tea.Cmd {
	m.reviewFetchSeq++
	m.acceptReview(reviewMsg{review: fresh, jobID: jobID, fetchSeq: m.reviewFetchSeq})
	return m.dispatchFailedCommentsFetch(jobID)
}

// reviewJobCompletionChanged reports whether job has completed AGAIN since
// review was loaded -- true means review is STALE and should be refetched.
// review.Job is the ReviewJob snapshot embedded in the review at fetch
// time; job is the freshly-polled job from m.jobs (or, for a panel member,
// from m.panelMembers -- see selectedJob). Job IDs are reused across
// reruns, so a JobID match alone (splitReconcileDetail's Done/Failed
// idempotency checks) can't tell a review from a previous attempt apart
// from the current one. job.FinishedAt is stamped fresh by the daemon on
// every completion (including a rerun's) and already tracked in memory --
// no new persisted state needed.
//
// The comparison is intentionally forward-only -- job.FinishedAt strictly
// AFTER review.Job.FinishedAt -- not a plain inequality. m.panelMembers is
// refreshed only while a member is queued/running and left FROZEN once all
// members go terminal (staleExpandedPanelRuns), so selectedJob can hand
// back a snapshot OLDER than the one already embedded in the loaded review
// (e.g. currentReview was fetched at a completion the frozen snapshot
// predates -- an external rerun of a member, review opened via stacked
// Enter, then L into split). A plain inequality would treat that as
// "changed" too and refetch on EVERY reconcile pass forever, each landing
// resetting reviewScroll to 0 -- an unscrollable pane, not just a wasted
// fetch. A nil FinishedAt on either side (the loaded review's job snapshot
// is missing it, or the polled job hasn't recorded one yet) is treated
// conservatively as "changed" so an ambiguous case triggers a refetch
// rather than risking a stuck stale display; this is unaffected by the
// forward-only change since neither side can be "after" a nil.
//
// finished_at is stored and compared at whole-second resolution (RFC3339),
// so a rerun that completes within the same wall-clock second as the
// attempt it replaced is a false negative here (no refetch triggered) --
// not a practical concern for real review jobs, which run for seconds to
// minutes, but worth noting since a plain-equality read of this comment
// would otherwise assume exact timestamps.
func reviewJobCompletionChanged(review *storage.Review, job *storage.ReviewJob) bool {
	if review.Job == nil || review.Job.FinishedAt == nil || job.FinishedAt == nil {
		return true
	}
	return job.FinishedAt.After(*review.Job.FinishedAt)
}

// maybeBootstrapDetail populates the detail pane when split engages
// (launch, resize past the breakpoint, or L) without a cursor nudge. If the
// current selection's review is already loaded AND current
// (selectedReviewLoaded -- the same predicate renderDetailPane gates the
// review body on), there's nothing to fetch; scheduling anyway would
// needlessly reset reviewScroll. A stale-but-JobID-matching review (the
// selected job was rerun and completed since it was loaded) schedules the
// follow instead: renderDetailPane shows "Loading review..." for it, and
// without the fetch that placeholder would stall until the next fallback
// poll.
//
// Deliberately does NOT call disarmPendingReviewOpen before
// scheduleDetailFollow: this function's whole point is a
// layout toggle or resize where the SELECTION HAS NOT CHANGED -- if an
// ordinary fetch is in flight for the still-selected job (e.g. stacked,
// Enter on a done job, then L before the response lands), that request's
// own "open the review" intent is still exactly what the user wants
// served once a response lands, and must survive this call.
func (m model) maybeBootstrapDetail() (model, tea.Cmd) {
	if m.layout != layoutSplit {
		return m, nil
	}
	// A tasks-origin review renders full-screen even while split is engaged
	// (splitActive excludes it): the fix job's ID doesn't resolve in
	// m.jobs/panelMembers, so the currentReview early-return below would be
	// skipped and scheduleDetailFollow would zero reviewScroll -- yanking
	// the scroll position of the review the user is READING, to bootstrap a
	// pane that isn't even rendered. Skip; splitReconcileDetail populates
	// the pane on the next jobs refresh once the tasks-origin review closes.
	if m.currentView == viewReview && m.reviewFromView == viewTasks {
		return m, nil
	}
	// Split re-binds the fix panel to the SELECTION (the pane follows it),
	// so a panel carried across the layout transition while bound to some
	// other job -- legal in stacked, where an open panel tracks the
	// full-screen review and the selection can move under it -- must not
	// survive the engage: focused over the split pane it would submit a
	// fix for a job the pane doesn't show. Self-guarding on the job
	// mismatch, so a panel or pending intent for the still-selected job
	// survives, per the no-disarm rule above.
	m.closeFixPanelIfJobChanged()
	if m.selectedReviewLoaded() {
		return m, nil
	}
	return m.scheduleDetailFollow()
}

// preserveOrClearReviewOnQueueReturn decides what to do with currentReview
// when a queue-origin transient view (the prompt view, opened via
// handlePromptKey's queue branch) exits back to viewQueue. Nil-ing
// currentReview unconditionally on such an exit blanks the split detail
// pane to "Loading review..." until the next periodic jobs refresh (up to
// ~15s) even though nothing about the review changed -- the prompt view
// doesn't mutate it. When split is active and currentReview still
// belongs to the selected job (the one the prompt was opened for, and
// which stepPromptNav keeps selectedJobID in sync with if the user
// navigated within the prompt view), retain it instead: the pane just
// keeps showing what it already had.
//
// This is safe for currentReview ITSELF: splitReconcileDetail re-validates
// and refreshes it on every subsequent jobs refresh exactly the same way
// regardless of how the pane arrived at its current content. It does NOT,
// on its own, say anything about currentReview's SIBLINGS
// (currentResponses, currentBranch) -- reconcile only inspects
// currentReview.JobID/Output/embedded-Job, never the siblings, so this
// function retaining a still-matching review does nothing to guarantee
// those siblings belong to the SAME job. That guarantee has to come from
// whoever populates currentReview: handlePromptMsg, the other producer on
// this path (stepPromptNav -> fetchReviewForPrompt), clears
// currentResponses/currentBranch itself when the incoming review's job
// differs from whatever was loaded before -- see its doc comment. Without
// that, navigating the prompt view to a DIFFERENT job's prompt (which
// updates currentReview but not the split pane's siblings) and then
// exiting here would retain the new job's review displayed alongside the
// PREVIOUS job's stale comments/branch.
//
// Stacked layout (no persistent pane to retain it for) and split with no
// matching review both keep the prior nil-and-reload behavior.
func (m model) preserveOrClearReviewOnQueueReturn() model {
	if m.layout == layoutSplit && m.currentReview != nil && m.currentReview.JobID == m.selectedJobID {
		return m
	}
	m.currentReview = nil
	return m
}

// paneLogWidth returns the split detail pane's inner content width. Both
// startPaneLog (the initial formatter) and fetchPaneLog (the formatter
// rebuilt on a non-incremental fetch) must size the streamfmt.Formatter to
// this -- not m.width -- or log text wraps for the full terminal and then
// gets hard-truncated to the pane. Shared here so the two callers can't
// drift apart.
func (m model) paneLogWidth() int {
	footerRows := m.splitFooterRows()
	g := splitGeometry(m.width, m.height, len(reflowHelpRows(footerRows, m.width)))
	return g.detailInnerW
}

// startPaneLog begins tailing a running job's log in the detail pane
// Resets the pane's log state and kicks off the first fetch;
// a no-op if the pane is already tailing this same job.
func (m model) startPaneLog(job storage.ReviewJob) (tea.Model, tea.Cmd) {
	if m.paneLogJobID == job.ID && m.paneLogStreaming {
		return m, nil // already tailing this job
	}
	m.paneLogJobID = job.ID
	m.paneLogAgent = job.Agent
	m.paneLogSource = job.Source
	m.paneLogLines = nil
	m.paneLogOffset = 0
	m.paneLogSeq++
	m.paneLogStreaming = true
	m.paneLogPaused = false
	// A fresh tail makes any prior detail-pane error stale by definition:
	// whatever failed belonged to the previous tail (or to a previous job
	// entirely, when the selection changed through a path that doesn't go
	// via scheduleDetailFollow -- e.g. the control socket's select-job).
	// renderDetailPane's running branch renders splitDetailErr
	// unconditionally, so leaving it set would show a dead error over this
	// job's live tail indefinitely. This is the single chokepoint: every
	// new tail starts here.
	m.splitDetailErr = nil
	m.paneLogFmtr = streamfmt.NewWithWidth(
		io.Discard, m.paneLogWidth(), m.glamourStyle,
		decoderForJobLog(m.paneLogAgent, m.paneLogSource),
	)
	return m, m.fetchPaneLog(job.ID)
}

// splitReconcileDetail refreshes the detail pane after a jobs update: if
// the highlighted job finished while its log was tailing (or while showing
// a stale review), fetch its review; if the highlighted job is running but
// the pane isn't (or is no longer) tailing it -- e.g. a resize while a
// transient view was open invalidated the tail rather than restarting it --
// restart the tail. Called from handleJobsMsg after a successful jobs
// refresh (SSE push or the periodic poll), so a stalled running-job pane
// recovers automatically within one refresh cycle without needing a new
// selection change to re-trigger it.
//
// Attempt identity for the Done/Failed idempotency checks below: job IDs
// are reused across reruns, so "same JobID" alone doesn't prove the loaded
// review reflects THIS attempt rather than a previous one -- both branches
// need a signal for "has a NEW attempt completed since this review was
// loaded." Two field-based signals were considered and rejected as the
// SOLE signal (each still composes in as a fallback, see below):
//   - job.FinishedAt (reviewJobCompletionChanged): stamped fresh by the
//     daemon on every completion, but stored/compared at whole-second
//     (RFC3339) resolution -- a rerun completing within the same
//     wall-clock second as the attempt it replaced compares equal, so the
//     stale review would survive.
//   - Rendered error text (Failed branch): a rerun that fails with the
//     SAME message but different execution metadata (resolved model,
//     agent, ...) produces identical Output, so the stale synthetic
//     review (including its embedded Job snapshot/title metadata) would
//     survive too.
//
// Neither timestamp precision nor rendered text is a genuine per-attempt
// identity. storage.ReviewJob has no field that is (RetryCount resets on
// some paths and isn't bumped by every rerun path; storage.Review.ID is
// per-review-ROW, changing on a rerun, but only observable AFTER a fetch
// already landed -- useful to confirm a fetch was fresh, not to decide
// whether to fetch). So instead of inventing a timestamp/ID-based identity,
// m.paneReviewSeenNonTerminal tracks a state-machine fact that's immune to
// both precision problems: a rerun MUST pass through queued/running before
// it can complete again, and this function observes the selected job on
// every jobs refresh -- so "was the selected job seen queued/running since
// the currently-loaded review was fetched" proves a fresh attempt occurred,
// independent of what its timestamp or output happen to be. Set below
// whenever the selected job is queued/running while currentReview is
// already loaded for it; consulted (as an unconditional "stale" signal) and
// cleared when a Done/Failed observation rebuilds the review.
//
// This alone has one gap, by design: if the job's ENTIRE queued+running
// window is missed between two jobs refreshes (e.g. the daemon claims and
// finishes an external rerun faster than both the SSE push and the ~15s
// poll happen to land), paneReviewSeenNonTerminal never gets set for that
// attempt, and the fallback signal (FinishedAt.After / Output comparison)
// takes over -- which is precision-limited as described above, so a
// same-second, same-text rerun that's ALSO missed entirely in-flight is the
// residual, deliberately-accepted edge case. In practice a real review job
// runs for seconds to minutes, and the daemon SSE-broadcasts both the
// running and the completed transition, so this compound miss requires
// both the SSE connection to be down/backed-off AND the poll to land
// exactly outside the run window -- narrow enough not to warrant more
// persisted state for it.
func (m model) splitReconcileDetail() (model, tea.Cmd) {
	if m.layout != layoutSplit {
		return m, nil
	}
	job, ok := m.selectedJob()
	if !ok {
		return m, nil
	}
	if job.Status == storage.JobStatusQueued || job.Status == storage.JobStatusRunning {
		if m.currentReview != nil && m.currentReview.JobID == job.ID {
			m.paneReviewSeenNonTerminalJob = job.ID
		}
		// A fix panel bound to a job observed back in queued/running is
		// bound to an attempt being replaced: submitting would start a fix
		// against the review the rerun is about to overwrite. Worse, a
		// FOCUSED panel keeps capturing every keystroke (handleKeyMsg's
		// panel capture runs before any staleness guard) even though
		// renderDetailPane now shows a status card instead of the panel.
		// Close it -- the same decision handleRerunResultMsg makes when the
		// rerun was initiated locally; this is the external-rerun twin,
		// observed via the jobs refresh rather than a local confirmation.
		if (m.reviewFixPanelOpen || m.reviewFixPanelPending) && m.fixPromptJobID == job.ID {
			m.closeFixPanel()
		}
	}
	switch job.Status {
	case storage.JobStatusDone:
		if m.paneLogJobID == job.ID && m.paneLogStreaming {
			m.paneLogSeq++
			m.paneLogStreaming = false
		}
		// job.FinishedAt is stamped fresh by the daemon on every completion
		// (including a rerun's); reviewJobCompletionChanged compares it
		// against the FinishedAt captured on the loaded review's embedded
		// Job as a supplementary signal alongside paneReviewSeenNonTerminal
		// -- see this function's doc comment for why neither is used alone.
		if m.currentReview != nil && m.currentReview.JobID == job.ID &&
			m.paneReviewSeenNonTerminalJob != job.ID && !reviewJobCompletionChanged(m.currentReview, job) {
			// Retaining the loaded review is right for its CONTENT, but
			// Closed changes independently of completion (another TUI or
			// the CLI can toggle it, arriving here as refreshed m.jobs
			// state with no new FinishedAt). Left unsynced, the pane's
			// [CLOSED] badge goes stale and the next local toggle submits
			// the already-current state -- appearing to do nothing. Sync
			// it in place; no refetch needed for a boolean the jobs
			// refresh already delivered.
			if job.Closed != nil && m.currentReview.Closed != *job.Closed {
				m.currentReview.Closed = *job.Closed
			}
			return m, nil
		}
		// paneReviewSeenNonTerminal is deliberately NOT cleared here: this
		// only DISPATCHES the fetch, it doesn't yet know whether the
		// review will actually be replaced. Clearing at dispatch time
		// would lose the "a fresh attempt was observed" fact if this
		// fetch fails (handleReviewFollowErrMsg records splitDetailErr but
		// leaves currentReview and the flag alone) -- the flag stays true
		// so the NEXT reconcile pass retries instead of being stuck behind
		// a fallback signal that's blind to same-second/same-text reruns.
		// The reset lives where the review is actually ACCEPTED: the
		// follow-landing branch in handleReviewMsg.
		//
		// A follow fetch for this job may already be in flight -- not
		// necessarily for THIS exact completion; it could have been
		// dispatched for an EARLIER completion of the same job that's
		// still unresolved, and the guard below suppresses this dispatch
		// either way (the earlier one's eventual response, whenever it
		// lands, is ordered by the shared fetch epoch like everything
		// else). This branch can be re-entered on every jobs refresh (SSE
		// push or the ~15s poll) while paneReviewSeenNonTerminal stays
		// true awaiting acceptance, unlike this function's other
		// branches/callers, which only fire once per discrete event.
		//
		// Correctness no longer depends on this guard -- the epoch alone
		// guarantees no older response can overwrite newer content. Its
		// remaining value is LIVENESS (plus not piling up duplicate
		// in-flight requests against a slow daemon): without it, a fetch
		// slower than the jobs-refresh interval would be superseded by
		// its own successor before it could ever land, and the pane would
		// never converge. Scoped to THIS job specifically (not a bare
		// bool) so a stale entry left over for a DIFFERENT job can never
		// block dispatching here. See reconcileFetchJobID's doc comment
		// (tui.go) for why no reachable sequence leaves it stuck set for
		// this job with nothing able to clear it.
		if m.reconcileFetchJobID == job.ID {
			return m, nil
		}
		// Clear any earlier splitDetailErr before issuing the follow
		// fetch so a stale error from a previous job/attempt can't
		// briefly show through for this one while the fresh fetch is
		// in flight.
		m.splitDetailErr = nil
		m.reconcileFetchJobID = job.ID
		cmd := m.dispatchReviewFollow(job.ID)
		// Captured AFTER the dispatch, which is what advanced the epoch:
		// this is the value stamped on the outgoing request, and the only
		// value whose arrival may release the slot (see
		// reconcileFetchSeq's doc comment, tui.go).
		m.reconcileFetchSeq = m.reviewFetchSeq
		return m, cmd
	case storage.JobStatusFailed:
		// Stop any live or stale tail before applying the failed-review
		// idempotency checks below.
		if m.paneLogJobID == job.ID && m.paneLogStreaming {
			m.paneLogSeq++
			m.paneLogStreaming = false
		}
		// The idempotency check composes THREE signals: same job ID with no
		// observed queued/running window since load
		// (paneReviewSeenNonTerminalJob), an unchanged completion identity
		// (reviewJobCompletionChanged on the embedded snapshot's
		// FinishedAt, mirroring the Done branch -- this is what catches a
		// missed-window rerun whose failure text happens to match), AND
		// the same rendered error text. Only all three together mean the
		// displayed review already IS today's failure; anything else gets
		// replaced. See this function's doc comment for why no single
		// signal suffices.
		fresh := synthesizeFailedReview(job, m.currentReview)
		if m.currentReview != nil && m.currentReview.JobID == job.ID &&
			m.paneReviewSeenNonTerminalJob != job.ID &&
			!reviewJobCompletionChanged(m.currentReview, job) &&
			m.currentReview.Output == fresh.Output {
			return m, nil
		}
		// Mirrors the Done branch above: clear any stale error before
		// swapping in the synthesized review so it can't briefly show
		// through for this job.
		m.splitDetailErr = nil
		// Synchronous rebuild routed through the shared ordered acceptance
		// (epoch bump + acceptReview): sibling clears, scroll reset, the
		// scoped observation clear and the pending-intent consumes all run
		// there, and the epoch bump dooms any pre-rerun in-flight response
		// for this job -- see acceptSynthesizedFailure. The returned cmd
		// loads the job's persisted comments.
		cmd := m.acceptSynthesizedFailure(job.ID, fresh)
		return m, cmd
	case storage.JobStatusRunning:
		if m.paneLogJobID != job.ID || !m.paneLogStreaming {
			// Restarting the tail invalidates any earlier error; clear it
			// here as well as in startPaneLog so this reconcile pass is
			// self-contained (startPaneLog can no-op if it is already
			// tailing this job, which the guard above rules out).
			m.splitDetailErr = nil
			mm, cmd := m.startPaneLog(*job)
			return mm.(model), cmd
		}
	}
	return m, nil
}

// normalizeSplitState reconciles view/focus state after any Update()
// handler runs -- the single chokepoint (tui.go's Update, right before
// returning) every path funnels through before the next render.
//
// Three invariants are repaired here:
//
//  1. currentView == viewReview implies currentReview != nil, in BOTH
//     layouts. Most paths that set currentView = viewReview pair it with
//     currentReview in the same assignment (handleReviewMsg, the
//     synthesized failed-job review, ...), but several "return to the
//     review" paths (handlePromptKey's viewKindPrompt branch, handleEscKey/
//     handleQuitKey's prompt/help/commitMsg returns) instead assume
//     currentReview is still whatever it was when the transient view was
//     entered. That assumption breaks if an async event nils currentReview
//     while the transient view (or viewReview itself) is on screen -- e.g.
//     a rerun success confirmed for the open review's job (see
//     handleRerunResultMsg's stale-review clear). Left unrepaired, stacked
//     layout has no self-healing path: viewContent's nil guard silently
//     falls back to rendering the queue while currentView stays viewReview,
//     so arrow/page keys keep manipulating the invisible reviewScroll
//     instead of the queue until the user presses esc. Fixing it here,
//     regardless of layout, means every one of those return paths is
//     covered by a single check instead of needing its own guard.
//  2. In split layout only, focus == focusDetail iff currentView ==
//     viewReview -- so a transient view (help, log, ...) exiting into a
//     layout that changed underneath it while it was open still lands with
//     consistent focus.
//  3. reviewFixPanelFocused implies currentView == viewReview, in BOTH
//     layouts. handleKeyMsg routes keys to the panel only there, and
//     renderReviewFixPanelPaneLines renders the focused (active input)
//     variant off the flag alone -- so a focused panel surviving a view
//     move away from viewReview LOOKS like it is capturing input while
//     every keystroke actually lands on the current view's keymap ('q'
//     quits, 'a' closes the review, 'f' opens the filter modal). Two
//     paths could previously strand that state: acceptReview's pending-'F'
//     consume when the guarded view switch declines (a transient view was
//     open at acceptance), and a same-row list-pane click in split (whose
//     followSelectionChange no-ops on the unchanged selection, so
//     closeFixPanelIfJobChanged never runs). Repairing it here covers
//     every such path at once; the panel stays OPEN (the typed prompt is
//     preserved), it just requires an explicit tab to re-focus once the
//     user is back on the review.
//
// For view/focus, transient views themselves are left untouched; invariants
// 1-2 only act when currentView is viewQueue or viewReview (after invariant
// 1's repair may have turned a stale viewReview into viewQueue). Invariant 3
// acts regardless of the current view: a focused panel is inconsistent the
// moment currentView is anything but viewReview.
func (m model) normalizeSplitState() model {
	if m.currentView == viewReview && m.currentReview == nil {
		m.currentView = viewQueue
		m.reviewScroll = 0
	}
	// Invariant 1's prompt-view twin: viewContent renders the prompt only
	// while currentReview is non-nil and silently falls back to the queue
	// otherwise -- so an async clear while the prompt view is open (e.g. a
	// control-socket rerun of the prompted job confirming, whose
	// handleRerunResultMsg nils currentReview regardless of view) would
	// leave the queue on screen while keys still route as prompt input
	// (invisible promptScroll, prompt-flavored esc). Return to the queue
	// directly: the prompt's usual viewReview return target is equally
	// unbacked with currentReview nil and invariant 1 would send it there
	// anyway.
	if m.currentView == viewKindPrompt && m.currentReview == nil {
		m.currentView = viewQueue
		m.promptScroll = 0
	}

	if m.reviewFixPanelFocused && m.currentView != viewReview {
		m.reviewFixPanelFocused = false
	}

	if m.layout != layoutSplit {
		return m
	}
	switch m.currentView {
	case viewQueue:
		m.focus = focusList
	case viewReview:
		m.focus = focusDetail
	}
	return m
}
