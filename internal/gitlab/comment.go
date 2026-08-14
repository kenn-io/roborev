package gitlab

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"

	gogitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"go.kenn.io/roborev/internal/review"
)

// CommentMarker is an invisible HTML marker embedded in every roborev merge
// request note so subsequent runs can find and update the existing note
// instead of creating duplicates. It is intentionally identical to
// github.CommentMarker so comment formatting stays consistent across forges.
const CommentMarker = "<!-- roborev-pr-comment -->"

// MRRefs are the commits that bound a merge request's diff.
type MRRefs struct {
	// HeadSHA is the head commit of the source branch.
	HeadSHA string
	// BaseSHA is the commit the merge request is diffed against. It is empty
	// when GitLab reports no diff refs, which happens on merge requests it has
	// not finished preparing.
	BaseSHA string
}

// MergeRequestRefs returns the commits bounding the merge request's diff.
// Callers use them to check that the range they reviewed is the one the note
// will describe: --pr and --ref are independent inputs, so nothing else ties
// the verdict to the merge request it lands on, in either extent or head.
func (c *Client) MergeRequestRefs(
	ctx context.Context, project string, mrIID int,
) (MRRefs, error) {
	project, err := parseProject(project)
	if err != nil {
		return MRRefs{}, err
	}
	mr, _, err := c.api.MergeRequests.GetMergeRequest(
		project, int64(mrIID), nil, gogitlab.WithContext(ctx))
	if err != nil {
		return MRRefs{}, fmt.Errorf("get merge request: %w", err)
	}
	if mr == nil || strings.TrimSpace(mr.SHA) == "" {
		return MRRefs{}, fmt.Errorf(
			"merge request !%d reports no head commit", mrIID)
	}
	head := strings.TrimSpace(mr.SHA)
	// GitLab recomputes diff_refs asynchronously, so shortly after a push the
	// reported base can still describe the previous head. Returning that base
	// alongside the current head would have callers check a range against a
	// diff that never existed, so say the refs disagree instead.
	if diffHead := strings.TrimSpace(mr.DiffRefs.HeadSha); diffHead != "" &&
		!strings.EqualFold(diffHead, head) {
		return MRRefs{}, fmt.Errorf(
			"merge request !%d is at %s but its diff still describes %s "+
				"(GitLab is preparing the diff — retry shortly)",
			mrIID, head, diffHead)
	}
	return MRRefs{
		HeadSHA: head,
		BaseSHA: strings.TrimSpace(mr.DiffRefs.BaseSha),
	}, nil
}

// FindExistingMRComment searches for an existing roborev note on the given
// merge request. It returns the note ID if found, or 0 if no match exists.
func (c *Client) FindExistingMRComment(ctx context.Context, project string, mrIID int) (int64, error) {
	var lastID int64
	err := c.eachMRNote(ctx, project, mrIID, func(note *gogitlab.Note) {
		if strings.Contains(note.Body, CommentMarker) {
			lastID = note.ID
		}
	})
	if err != nil {
		return 0, err
	}
	return lastID, nil
}

// eachMRNote calls visit for every non-system note on the merge request,
// following pagination. System notes are skipped: they are GitLab's own activity
// entries, never roborev comments.
func (c *Client) eachMRNote(
	ctx context.Context, project string, mrIID int, visit func(*gogitlab.Note),
) error {
	project, err := parseProject(project)
	if err != nil {
		return err
	}

	opts := &gogitlab.ListMergeRequestNotesOptions{
		OrderBy: ptr("created_at"),
		Sort:    ptr("asc"),
		ListOptions: gogitlab.ListOptions{
			PerPage: 100,
		},
	}

	for {
		notes, resp, err := c.api.Notes.ListMergeRequestNotes(
			project, int64(mrIID), opts, gogitlab.WithContext(ctx))
		if err != nil {
			return fmt.Errorf("list merge request notes: %w", err)
		}
		for _, note := range notes {
			if note == nil || note.System {
				continue
			}
			visit(note)
		}
		if resp == nil || resp.NextPage == 0 {
			return nil
		}
		opts.Page = resp.NextPage
	}
}

// quickActionPattern matches the start of a line GitLab's quick action
// extractor would run as a command: a forward slash in the very first column
// followed by an ASCII letter. GitLab anchors its command regex at `^/`, so
// indented slashes and mid-line slashes are never commands. The command name
// is deliberately not matched against the known action list — that list
// depends on the instance version and edition, so every `/word` line is
// escaped. Escaping an unknown command such as `/usr/bin/foo` is harmless:
// CommonMark renders `\/` as a literal `/`, so the line still reads the same.
var quickActionPattern = regexp.MustCompile(`^/[a-zA-Z]`)

// stripCR removes every carriage return, the way GitLab's extractor deletes
// them from the whole body before it matches.
func stripCR(line string) string {
	return strings.ReplaceAll(line, "\r", "")
}

// neutralizeQuickActions escapes every line GitLab could execute as a quick
// action (`/approve`, `/close`, `/target_branch foo`, ...). Model-generated
// review text can be steered by an untrusted merge request author, and notes
// created by the CI token would run those commands with the token's
// permissions.
//
// Escaping prefixes the leading slash with a backslash: CommonMark renders
// `\/approve` as the literal text `/approve`, while GitLab's extractor no
// longer sees a slash in the first column. It is idempotent, because an
// already-escaped line starts with `\` rather than `/`.
//
// Escaping is deliberately unconditional, code fences included. GitLab's
// extractor does skip code fences, inline code, HTML blocks, and quote blocks,
// but it does so through one regex alternation whose branches interact: an
// empty fence pair is not a block, an HTML or `>>>` wrapper can swallow a fence
// line, and newer versions route the body through a markdown pipeline first.
// Reproducing that in Go means tracking every branch of an upstream regex
// across GitLab versions and editions, and a one-line disagreement hands the
// bypass back. Over-escaping only costs appearance — a literal `\/` inside a
// code sample — and a slash in the first column followed by a letter is rare in
// review snippets, since `//` and `/*` comment markers do not match. Missing a
// line would be a privilege escalation, so the trade runs the safe way.
func neutralizeQuickActions(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = escapeQuickAction(line)
	}
	return strings.Join(lines, "\n")
}

// escapeQuickAction escapes a single line if GitLab would read it as a
// command. Detection runs on the carriage-return-stripped line because GitLab
// deletes every carriage return before matching, so neither a leading nor an
// interposed CR stops a slash from reaching the first column.
func escapeQuickAction(line string) string {
	if !quickActionPattern.MatchString(stripCR(line)) {
		return line
	}
	// The stripped line starts with the slash, so the first slash in the
	// original is preceded by carriage returns only. Escaping it there
	// leaves a backslash in front once GitLab drops those returns.
	slash := strings.IndexByte(line, '/')
	return line[:slash] + `\` + line[slash:]
}

// prepareBody prepends the CommentMarker, neutralizes GitLab quick actions,
// and truncates to review.MaxCommentLen, preserving UTF-8 safety.
func prepareBody(body string) string {
	return review.TruncateComment(CommentMarker + "\n" + neutralizeQuickActions(body))
}

// CreateMRComment posts a new roborev merge request note. It prepends the
// CommentMarker and truncates to review.MaxCommentLen, then always creates a
// new note (no find/update).
func (c *Client) CreateMRComment(ctx context.Context, project string, mrIID int, body string) error {
	return c.createPreparedComment(ctx, project, mrIID, prepareBody(body))
}

// UpsertMRComment creates or updates a roborev merge request note. It prepends
// the CommentMarker, truncates to review.MaxCommentLen, and either updates an
// existing marker note or creates a new one.
func (c *Client) UpsertMRComment(ctx context.Context, project string, mrIID int, body string) error {
	body = prepareBody(body)

	existingID, err := c.FindExistingMRComment(ctx, project, mrIID)
	if err != nil {
		return fmt.Errorf("find existing comment: %w", err)
	}

	if existingID > 0 {
		if err := c.updateComment(ctx, project, mrIID, existingID, body); err != nil {
			if isGitLabStatus(err, http.StatusForbidden, http.StatusNotFound) {
				log.Printf("warning: update note %d: %v (falling back to new comment)", existingID, err)
			} else {
				return fmt.Errorf("update note %d: %w", existingID, err)
			}
		} else {
			return nil
		}
	}
	if err := c.createPreparedComment(ctx, project, mrIID, body); err != nil {
		return c.recoverFailedCreate(ctx, project, mrIID, body, err)
	}
	return nil
}

// recoverFailedCreate retries a create that failed, after checking whether it
// actually applied.
//
// A 5xx create is not replayed by the client library (see
// createPreparedComment), so the failure reaches roborev instead of becoming a
// duplicate note. GitLab may still have applied the note before answering with
// an error, so re-list first and only create again when it did not land. The
// notes API takes no idempotency key, so the body is the identity check: the
// create posts exactly this body, so a marker note carrying it byte for byte is
// this request's own note.
//
// Matching on the body rather than on "a marker note that wasn't there before"
// is what keeps a concurrent pipeline safe. Another job can add or update its
// own marker note between the list and the create, and adopting that note would
// overwrite a newer review with this older one. A byte-identical note needs no
// write at all, which also removes the second failure mode: an update that
// fails and reports the whole post as failed when it had in fact succeeded.
func (c *Client) recoverFailedCreate(
	ctx context.Context, project string, mrIID int, body string, createErr error,
) error {
	landed, findErr := c.mrNoteExistsWithBody(ctx, project, mrIID, body)
	if findErr != nil {
		return fmt.Errorf(
			"%w (could not check whether the note landed: %w)", createErr, findErr)
	}
	if landed {
		// This exact body is already posted; nothing left to do.
		return nil
	}
	// An authorization or not-found failure will fail again for the same reason
	// — a Guest-role or read_api-only token can list notes but not create them,
	// so retrying just yields a second identical rejection under a "transient
	// failure" label that hides the real cause.
	if isPermanentFailure(createErr) {
		return createErr
	}
	// This body is not on the merge request, so posting it cannot duplicate it.
	if err := c.createPreparedComment(ctx, project, mrIID, body); err != nil {
		return fmt.Errorf("%w (retry after transient failure also failed: %w)",
			createErr, err)
	}
	return nil
}

// mrNoteExistsWithBody reports whether the merge request already carries a note
// whose body is exactly body.
func (c *Client) mrNoteExistsWithBody(
	ctx context.Context, project string, mrIID int, body string,
) (bool, error) {
	found := false
	err := c.eachMRNote(ctx, project, mrIID, func(note *gogitlab.Note) {
		if note.Body == body {
			found = true
		}
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// isPermanentFailure reports whether err carries a 4xx status that retrying
// cannot fix. Two 4xx statuses are excluded:
//
//   - 429, which this package treats as replayable everywhere else (see
//     retryRateLimitOnly): the limiter rejects the request before the note is
//     applied. Dropping a rate-limited note here would cost a hard-limited job
//     its last attempt.
//   - 408, where the server gave up before receiving the full request, so it is
//     transient rather than a verdict on the request. Nothing else in this
//     package retries it, which makes recoverFailedCreate its only replay — and
//     that path re-lists first, so a note that did land is adopted rather than
//     duplicated.
func isPermanentFailure(err error) bool {
	status, ok := gitlabStatusCode(err)
	if !ok {
		return false
	}
	switch status {
	case http.StatusTooManyRequests, http.StatusRequestTimeout:
		return false
	}
	return status >= 400 && status < 500
}

// createPreparedComment posts a body that already went through prepareBody.
//
// The POST goes out with a narrowed retry policy: rate limits still retry,
// 5xx answers do not. The library's default retries both for every method, and
// creating a note is not idempotent — GitLab can apply the note and still answer
// with 5xx, so a replay leaves two notes behind and a later upsert updates only
// the newer one. Declining that replay hands the decision to
// recoverFailedCreate, which re-lists before trying again.
func (c *Client) createPreparedComment(ctx context.Context, project string, mrIID int, body string) error {
	project, err := parseProject(project)
	if err != nil {
		return err
	}
	_, _, err = c.api.Notes.CreateMergeRequestNote(
		project, int64(mrIID),
		&gogitlab.CreateMergeRequestNoteOptions{Body: ptr(body)},
		gogitlab.WithContext(ctx), gogitlab.WithRequestRetry(c.retryRateLimitOnly))
	if err != nil {
		return fmt.Errorf("create MR comment: %w", err)
	}
	return nil
}

// retryRateLimitOnly retries a rate-limited request and nothing else.
//
// A 429 comes from GitLab's rate limiter before the note is applied, so
// replaying it cannot duplicate anything, and the client's backoff honors
// RateLimit-Reset. Keeping that retry matters for CreateMRComment too, which
// posts without an upsert and so has no recovery path of its own. Returning a
// nil error otherwise leaves the response to the library's normal handling,
// matching what its own policy does for a non-retryable case.
//
// The disableRetries check has to be repeated here: a per-request policy
// replaces the library's own, and only that one consults the flag, so without
// it WithoutRetries() would still replay a 429.
func (c *Client) retryRateLimitOnly(ctx context.Context, resp *http.Response, _ error) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if c.disableRetries {
		return false, nil
	}
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		return true, nil
	}
	return false, nil
}

func (c *Client) updateComment(ctx context.Context, project string, mrIID int, noteID int64, body string) error {
	project, err := parseProject(project)
	if err != nil {
		return err
	}
	_, _, err = c.api.Notes.UpdateMergeRequestNote(
		project, int64(mrIID), noteID,
		&gogitlab.UpdateMergeRequestNoteOptions{Body: ptr(body)},
		gogitlab.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("update merge request note: %w", err)
	}
	return nil
}

func isGitLabStatus(err error, statuses ...int) bool {
	status, ok := gitlabStatusCode(err)
	if !ok {
		return false
	}
	return slices.Contains(statuses, status)
}

// gitlabStatusCode extracts the HTTP status an error carries, reporting false
// when it carries none. It is the single place that knows how the client
// library shapes its errors, so isGitLabStatus and isPermanentFailure cannot
// drift apart when the library changes that mapping.
func gitlabStatusCode(err error) (int, bool) {
	// The library maps 404 to a sentinel error rather than an *ErrorResponse.
	if errors.Is(err, gogitlab.ErrNotFound) {
		return http.StatusNotFound, true
	}
	var gitlabErr *gogitlab.ErrorResponse
	if !errors.As(err, &gitlabErr) || gitlabErr.Response == nil {
		return 0, false
	}
	return gitlabErr.Response.StatusCode, true
}
