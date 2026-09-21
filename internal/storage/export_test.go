package storage

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestExportReviewsContentProfile(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	assert := assert.New(t)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, db.SetRepoIdentity(repo.ID, "github.com/acme/widgets"))
	commit := createCommit(t, db, repo.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	job := enqueueJob(t, db, repo.ID, commit.ID, commit.SHA)
	claimJob(t, db, "w1")
	require.NoError(t, completeReviewFixture(db, job.ID, "codex", "SYSTEM PROMPT MUST NOT EXPORT", "No issues found."))
	_, err := db.Exec(`
		UPDATE review_jobs
		SET branch = 'main',
		    model = 'gpt-test',
		    enqueued_at = '2026-06-28T19:00:00-05:00',
		    token_usage = '{"input_tokens":123,"total_output_tokens":45,"cost_usd":0.67,"has_cost":true}',
		    started_at = '2026-06-29T00:00:00Z',
		    finished_at = '2026-06-29T00:00:02Z',
		    diff_content = 'RAW DIFF MUST NOT EXPORT',
		    command_line = 'TOKEN MUST NOT EXPORT'
		WHERE id = ?`, job.ID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE reviews SET created_at = '2026-06-29 00:00:03' WHERE job_id = ?`, job.ID)
	require.NoError(t, err)

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)

	got := page.Reviews[0]
	assert.Equal("done", got.Status)
	assert.Equal("pass", got.Verdict)
	assert.Equal("repo", got.Project)
	assert.Equal("github.com/acme/widgets", got.Repo)
	assert.Equal("main", derefString(got.Branch))
	assert.Equal(commit.SHA, derefString(got.CommitSHA))
	assert.Equal("codex", got.Agent)
	assert.Equal("gpt-test", derefString(got.Model))
	assert.Equal("2026-06-29T00:00:00Z", got.CreatedAt)
	assert.Equal(int64(123), derefInt64(got.Cost.TokensIn))
	assert.Equal(int64(45), derefInt64(got.Cost.TokensOut))
	assert.InDelta(0.67, derefFloat64(got.Cost.USD), 1e-9)
	assert.Contains(derefString(got.Content), "No issues found.")
	assert.Equal(int64(2000), derefInt64(got.DurationMS))
	assert.Equal("2026-06-29T00:00:03Z", got.CompletedAt)
	assert.Empty(got.Subagents)

	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	body := string(encoded)
	assert.NotContains(body, "SYSTEM PROMPT MUST NOT EXPORT")
	assert.NotContains(body, "RAW DIFF MUST NOT EXPORT")
	assert.NotContains(body, "TOKEN MUST NOT EXPORT")
	assert.NotContains(body, "diff_content")
	assert.NotContains(body, "command_line")
	assert.NotContains(body, "prompt")
	assert.NotContains(body, "token_usage")
	assert.NotContains(body, "input_tokens")
	assert.NotContains(body, "total_output_tokens")
	assert.NotContains(body, "cost_usd")
	assert.NotContains(body, "has_cost")
}

func TestExportReviewsMetadataProfileOmitsContent(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	commit := createCommit(t, db, repo.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	job := enqueueJob(t, db, repo.ID, commit.ID, commit.SHA)
	claimJob(t, db, "w1")
	require.NoError(t, completeReviewFixture(db, job.ID, "codex", "prompt", "No issues found. SECRET_OUTPUT"))

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	assert.Nil(t, page.Reviews[0].Content)
	assert.Equal(t, "repo", page.Reviews[0].Repo)

	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "SECRET_OUTPUT")
	assert.NotContains(t, string(encoded), filepath.Dir(repo.RootPath))
}

// exportTestStoredDocument is deliberately not in canonical form: keys are out
// of order, text is padded, and severity and verdict are upper case.
const exportTestStoredDocument = `{
  "findings": [
    {"location": "auth/login.go:42", "fix": " Redact the token. ", "problem": " The token is logged. ", "severity": "HIGH"},
    {"severity": "low", "problem": "Typo in a comment.", "fix": "Fix the spelling.", "location": null}
  ],
  "verdict": "FAIL",
  "summary": "  Two problems.  ",
  "schema_version": 2
}`

const exportTestCanonicalDocument = `{"schema_version":2,"summary":"Two problems.","verdict":"fail","findings":[` +
	`{"severity":"high","problem":"The token is logged.","fix":"Redact the token.","location":"auth/login.go:42"},` +
	`{"severity":"low","problem":"Typo in a comment.","fix":"Fix the spelling.","location":null}]}`

func TestExportReviewsContentProfileExportsCanonicalDocument(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	assert := assert.New(t)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	review := seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-06-29 00:00:00", false)
	setExportReviewStructuredOutput(t, db, review.JobID, exportTestStoredDocument)

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	got := page.Reviews[0]

	require.NotNil(t, got.Document)
	assert.Equal("Two problems.", got.Document.Summary)
	assert.Equal("fail", got.Document.Verdict)
	require.Len(t, got.Document.Findings, 2)
	assert.Equal(structuredreview.Finding{
		Severity: "high",
		Problem:  "The token is logged.",
		Fix:      "Redact the token.",
		Location: "auth/login.go:42",
	}, got.Document.Findings[0])

	encoded, err := json.Marshal(got.Document)
	require.NoError(t, err)
	assert.JSONEq(exportTestCanonicalDocument, string(encoded),
		"document is the normalized document, not the stored bytes, and an empty location stays null")

	wantDoc, err := structuredreview.Decode([]byte(exportTestStoredDocument))
	require.NoError(t, err)
	assert.Equal(wantDoc.Markdown(""), derefString(got.Content), "content stays the Markdown rendering of the same document")
}

func TestExportReviewsDocumentIsNullWithoutStoredDocumentOrInMetadataProfile(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	withDocument := seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-06-29 00:00:00", false)
	setExportReviewStructuredOutput(t, db, withDocument.JobID, exportTestStoredDocument)
	withoutDocument := seedCompletedExportReview(t, db, repo.ID, "2222222222222222222222222222222222222222", "2026-06-29 00:00:01", false)
	setExportReviewStructuredOutput(t, db, withoutDocument.JobID, nil)

	exportedDocuments := func(profile ExportProfile) []any {
		page, err := db.ExportReviews(ExportReviewsOptions{Profile: profile, Limit: 10})
		require.NoError(t, err)
		encoded, err := json.Marshal(page)
		require.NoError(t, err)
		var wire struct {
			Reviews []map[string]any `json:"reviews"`
		}
		require.NoError(t, json.Unmarshal(encoded, &wire))
		require.Len(t, wire.Reviews, 2)
		documents := make([]any, 0, len(wire.Reviews))
		for _, review := range wire.Reviews {
			require.Contains(t, review, "document", "the key is always present")
			documents = append(documents, review["document"])
		}
		return documents
	}

	content := exportedDocuments(ExportProfileContent)
	assert.IsType(t, map[string]any{}, content[0], "a stored document exports as a JSON object")
	assert.Nil(t, content[1], "a review without structured_output exports document: null")

	assert.Equal(t, []any{nil, nil}, exportedDocuments(ExportProfileMetadata),
		"the metadata profile never exports the document")
}

func TestExportReviewsExportsSubagentDocuments(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	assert := assert.New(t)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	panelRun := testUUID("panel-run-documents")
	member := seedPanelExportJob(t, db, repo.ID, panelRun, "member", "security", 0, "codex", "security", "2026-06-29 00:00:01", "- High — issue")
	setExportReviewStructuredOutput(t, db, member.JobID, exportTestStoredDocument)
	seedPanelExportJob(t, db, repo.ID, panelRun, "synthesis", "", 0, "codex", "", "2026-06-29 00:00:03", "- Medium — synthesized")

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	synthesis := page.Reviews[0]
	require.Len(t, synthesis.Subagents, 1)

	subagent := synthesis.Subagents[0]
	require.NotNil(t, subagent.Document)
	encoded, err := json.Marshal(subagent.Document)
	require.NoError(t, err)
	assert.JSONEq(exportTestCanonicalDocument, string(encoded))
	assert.Equal(subagent.Document.Markdown(""), derefString(subagent.Content))

	// The synthesis review carries its own document, with the provenance the
	// member documents do not have.
	require.NotNil(t, synthesis.Document)
	assert.NotEqual(subagent.Document.Summary, synthesis.Document.Summary)
	assert.Equal([]string{"codex"}, synthesis.Document.SourceLabels)
	require.Len(t, synthesis.Document.Findings, 1)
	assert.Equal([]int{1}, synthesis.Document.Findings[0].Sources)

	metadata, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Limit: 10})
	require.NoError(t, err)
	require.Len(t, metadata.Reviews, 1)
	require.Len(t, metadata.Reviews[0].Subagents, 1)
	assert.Nil(metadata.Reviews[0].Document)
	assert.Nil(metadata.Reviews[0].Subagents[0].Document)
}

func TestExportReviewsRepoFilterUsesExportedRepoIdentifier(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-06-29 00:00:00", false)

	byRootPath, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Repo: repo.RootPath, Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, byRootPath.Reviews)

	byFallbackName, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Repo: repo.Name, Limit: 10})
	require.NoError(t, err)
	require.Len(t, byFallbackName.Reviews, 1)
	assert.Equal(t, repo.Name, byFallbackName.Reviews[0].Repo)

	require.NoError(t, db.SetRepoIdentity(repo.ID, "github.com/acme/widgets"))
	byOldFallbackName, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Repo: repo.Name, Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, byOldFallbackName.Reviews)

	byIdentity, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Repo: "github.com/acme/widgets", Limit: 10})
	require.NoError(t, err)
	require.Len(t, byIdentity.Reviews, 1)
	assert.Equal(t, "github.com/acme/widgets", byIdentity.Reviews[0].Repo)
}

func TestExportReviewsFiltersAndOrdering(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	assert := assert.New(t)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	older := seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-06-28 23:59:59", false)
	includedA := seedCompletedExportReview(t, db, repo.ID, "2222222222222222222222222222222222222222", "2026-06-29 00:00:00", false)
	includedB := seedCompletedExportReview(t, db, repo.ID, "3333333333333333333333333333333333333333", "2026-06-29T00:00:00Z", true)
	unfinished := enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, "4444444444444444444444444444444444444444").ID, "4444444444444444444444444444444444444444")
	_, err := db.Exec(`UPDATE review_jobs SET status = 'done', job_type = 'task' WHERE id = ?`, older.ID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'queued' WHERE id = ?`, unfinished.ID)
	require.NoError(t, err)
	for i, jobType := range []string{JobTypeTask, JobTypeInsights, JobTypeFix, JobTypeClassify, JobTypeCompact} {
		excluded := seedCompletedExportReview(t, db, repo.ID, fmt.Sprintf("%040x", 50+i), "2026-06-29 00:00:00", false)
		_, err = db.Exec(`UPDATE review_jobs SET job_type = ? WHERE id = ?`, jobType, excluded.JobID)
		require.NoError(t, err)
	}
	empty := enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, "5555555555555555555555555555555555555555").ID, "5555555555555555555555555555555555555555")
	claimJob(t, db, "w-empty")
	require.NoError(t, completeReviewFixture(db, empty.ID, "codex", "prompt", ""))
	_, err = db.Exec(`UPDATE reviews SET created_at = '2026-06-29 00:00:00' WHERE job_id = ?`, empty.ID)
	require.NoError(t, err)

	since := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	page, err := db.ExportReviews(ExportReviewsOptions{
		Profile: ExportProfileContent,
		Since:   since,
		Until:   until,
		Limit:   10,
	})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 2)
	wantIDs := []uuid.UUID{*includedA.UUID, *includedB.UUID}
	slices.SortFunc(wantIDs, uuid.UUID.Compare)
	assert.Equal(wantIDs, []uuid.UUID{page.Reviews[0].ReviewID, page.Reviews[1].ReviewID})

	closedOnly, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, ClosedOnly: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, closedOnly.Reviews, 1)
	assert.Equal(*includedB.UUID, closedOnly.Reviews[0].ReviewID)
}

func TestExportReviewsReportsClosedStateAndUpdatedAt(t *testing.T) {
	for _, profile := range []ExportProfile{ExportProfileContent, ExportProfileMetadata} {
		t.Run(string(profile), func(t *testing.T) {
			db := openTestDB(t)
			defer db.Close()
			assert := assert.New(t)

			repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
			review := seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-01-05 00:00:00", false)
			setExportReviewUpdatedAt(t, db, review.JobID, "2026-01-05T00:00:07-05:00")
			exportOne := func() ExportReview {
				page, err := db.ExportReviews(ExportReviewsOptions{Profile: profile, Limit: 10})
				require.NoError(t, err)
				require.Len(t, page.Reviews, 1)
				return page.Reviews[0]
			}

			open := exportOne()
			assert.False(open.Closed)
			assert.Equal("2026-01-05T05:00:07Z", open.UpdatedAt)

			require.NoError(t, db.MarkReviewClosedByJobID(review.JobID, true))
			closed := exportOne()
			assert.True(closed.Closed)
			assert.Equal(open.CompletedAt, closed.CompletedAt)
			openUpdated, err := time.Parse(time.RFC3339, open.UpdatedAt)
			require.NoError(t, err)
			closedUpdated, err := time.Parse(time.RFC3339, closed.UpdatedAt)
			require.NoError(t, err)
			assert.Greater(closedUpdated, openUpdated, "closing must advance updated_at")
			assert.Equal(time.UTC, closedUpdated.Location())

			require.NoError(t, db.MarkReviewClosedByJobID(review.JobID, false))
			assert.False(exportOne().Closed)
		})
	}
}

func TestExportReviewsUpdatedAtFallsBackToCompletedAt(t *testing.T) {
	for name, updatedAt := range map[string]any{"null": nil, "empty": "", "blank": "  "} {
		t.Run(name, func(t *testing.T) {
			db := openTestDB(t)
			defer db.Close()

			repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
			review := seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-01-05 00:00:00", false)
			setExportReviewUpdatedAt(t, db, review.JobID, updatedAt)

			matching, err := db.ExportReviews(ExportReviewsOptions{
				Profile:      ExportProfileMetadata,
				UpdatedSince: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
				Limit:        10,
			})
			require.NoError(t, err)
			require.Len(t, matching.Reviews, 1)
			assert.Equal(t, "2026-01-05T00:00:00Z", matching.Reviews[0].CompletedAt)
			assert.Equal(t, "2026-01-05T00:00:00Z", matching.Reviews[0].UpdatedAt)

			later, err := db.ExportReviews(ExportReviewsOptions{
				Profile:      ExportProfileMetadata,
				UpdatedSince: time.Date(2026, 1, 5, 0, 0, 1, 0, time.UTC),
				Limit:        10,
			})
			require.NoError(t, err)
			assert.Empty(t, later.Reviews)
		})
	}
}

func TestExportReviewsUpdatedSinceReturnsReviewClosedAfterBound(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	untouched := seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-01-05 00:00:00", false)
	setExportReviewUpdatedAt(t, db, untouched.JobID, "2026-01-05T00:00:00Z")
	closedLater := seedCompletedExportReview(t, db, repo.ID, "2222222222222222222222222222222222222222", "2026-01-06 00:00:00", false)
	setExportReviewUpdatedAt(t, db, closedLater.JobID, "2026-01-06T00:00:00Z")

	// Both reviews completed long before the bound. Only the close happens
	// after it.
	bound := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, db.MarkReviewClosed(closedLater.ID, true))

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, UpdatedSince: bound, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	assert.Equal(t, *closedLater.UUID, page.Reviews[0].ReviewID)
	assert.True(t, page.Reviews[0].Closed)
	assert.Equal(t, "2026-01-06T00:00:00Z", page.Reviews[0].CompletedAt)
}

func TestExportReviewsUpdatedSinceIsInclusiveAndCombinesWithOtherFilters(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	assert := assert.New(t)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	bound := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	seed := func(sha, completedAt, updatedAt string, closed bool) uuid.UUID {
		review := seedCompletedExportReview(t, db, repo.ID, sha, completedAt, closed)
		setExportReviewUpdatedAt(t, db, review.JobID, updatedAt)
		return *review.UUID
	}
	seed("1111111111111111111111111111111111111111", "2026-01-05 00:00:00", "2026-03-01T11:59:59Z", true)
	atBound := seed("2222222222222222222222222222222222222222", "2026-01-06 00:00:00", "2026-03-01 12:00:00", true)
	// A local-offset timestamp that is before the bound only as text.
	offsetAfter := seed("3333333333333333333333333333333333333333", "2026-01-07 00:00:00", "2026-03-01T08:00:00-05:00", false)
	lateCompletion := seed("4444444444444444444444444444444444444444", "2026-02-10 00:00:00", "2026-03-02T00:00:00Z", true)

	exportIDs := func(opts ExportReviewsOptions) []uuid.UUID {
		opts.Profile = ExportProfileMetadata
		opts.UpdatedSince = bound
		opts.Limit = 10
		page, err := db.ExportReviews(opts)
		require.NoError(t, err)
		ids := make([]uuid.UUID, 0, len(page.Reviews))
		for _, review := range page.Reviews {
			ids = append(ids, review.ReviewID)
		}
		return ids
	}

	assert.Equal([]uuid.UUID{atBound, offsetAfter, lateCompletion}, exportIDs(ExportReviewsOptions{}),
		"filtered rows stay ordered by completed_at")
	assert.Equal([]uuid.UUID{atBound, lateCompletion}, exportIDs(ExportReviewsOptions{ClosedOnly: true}))
	assert.Equal([]uuid.UUID{offsetAfter, lateCompletion}, exportIDs(ExportReviewsOptions{
		Since: time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC),
	}))
	assert.Equal([]uuid.UUID{atBound, offsetAfter}, exportIDs(ExportReviewsOptions{
		Until: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	}))
}

func TestExportReviewsCursorPaginationWithUpdatedSinceReturnsEachMatchOnce(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	bound := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	var want []uuid.UUID
	for i := range 9 {
		// Pairs share a completed_at second so the cursor tie-break on
		// review_id is exercised with the filter applied.
		completedAt := fmt.Sprintf("2026-01-05 00:00:%02d", i/2)
		review := seedCompletedExportReview(t, db, repo.ID, fmt.Sprintf("%040x", i+1), completedAt, false)
		updatedAt := "2026-01-05T00:00:00Z"
		if i%3 != 1 {
			updatedAt = fmt.Sprintf("2026-03-%02dT00:00:00Z", 28-i)
			want = append(want, *review.UUID)
		}
		setExportReviewUpdatedAt(t, db, review.JobID, updatedAt)
	}

	full, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, UpdatedSince: bound, Limit: 100})
	require.NoError(t, err)
	fullIDs := make([]uuid.UUID, 0, len(full.Reviews))
	for _, review := range full.Reviews {
		fullIDs = append(fullIDs, review.ReviewID)
	}
	assert.ElementsMatch(t, want, fullIDs)

	var cursor string
	var pagedIDs []uuid.UUID
	for {
		page, err := db.ExportReviews(ExportReviewsOptions{
			Profile:      ExportProfileMetadata,
			UpdatedSince: bound,
			Cursor:       cursor,
			Limit:        2,
		})
		require.NoError(t, err)
		for _, review := range page.Reviews {
			pagedIDs = append(pagedIDs, review.ReviewID)
		}
		if page.NextCursor == nil || len(page.Reviews) == 0 {
			break
		}
		cursor = *page.NextCursor
	}
	assert.Equal(t, fullIDs, pagedIDs)
	assert.Len(t, uniqueValues(pagedIDs), len(want), "paged export must return every match exactly once")
}

func TestExportReviewsPagination(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	assert := assert.New(t)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	first := seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-06-29 00:00:00", false)
	second := seedCompletedExportReview(t, db, repo.ID, "2222222222222222222222222222222222222222", "2026-06-29 00:00:01", false)
	third := seedCompletedExportReview(t, db, repo.ID, "3333333333333333333333333333333333333333", "2026-06-29 00:00:02", false)

	page1, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page1.Reviews, 2)
	assert.True(page1.Truncated)
	require.NotNil(t, page1.NextCursor)
	assert.Equal([]uuid.UUID{*first.UUID, *second.UUID}, []uuid.UUID{page1.Reviews[0].ReviewID, page1.Reviews[1].ReviewID})

	page2, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Cursor: *page1.NextCursor, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page2.Reviews, 1)
	assert.False(page2.Truncated)
	require.NotNil(t, page2.NextCursor)
	assert.Equal(*third.UUID, page2.Reviews[0].ReviewID)

	page3, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Cursor: *page2.NextCursor, Limit: 2})
	require.NoError(t, err)
	assert.Empty(page3.Reviews)
	assert.False(page3.Truncated)
	assert.Nil(page3.NextCursor)
}

func TestExportReviewsCursorRoundTripAcrossPagesMatchesUnpaginated(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	for i := range 5 {
		seedCompletedExportReview(
			t, db, repo.ID, fmt.Sprintf("%040x", i+1),
			fmt.Sprintf("2026-06-29 00:00:%02d", i), false,
		)
	}

	full, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Limit: 10})
	require.NoError(t, err)
	require.Len(t, full.Reviews, 5)

	var cursor string
	var pagedIDs []uuid.UUID
	for {
		page, err := db.ExportReviews(ExportReviewsOptions{
			Profile: ExportProfileMetadata,
			Cursor:  cursor,
			Limit:   2,
		})
		require.NoError(t, err)
		for _, review := range page.Reviews {
			pagedIDs = append(pagedIDs, review.ReviewID)
		}
		if page.NextCursor == nil || len(page.Reviews) == 0 {
			break
		}
		cursor = *page.NextCursor
	}

	fullIDs := make([]uuid.UUID, 0, len(full.Reviews))
	for _, review := range full.Reviews {
		fullIDs = append(fullIDs, review.ReviewID)
	}
	assert.Equal(t, fullIDs, pagedIDs)
	assert.Len(t, uniqueValues(pagedIDs), len(pagedIDs), "paged export must not duplicate reviews")
}

func TestExportReviewsCursorPaginatesSameCompletedTimestamp(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	first := seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-06-29 00:00:00", false)
	second := seedCompletedExportReview(t, db, repo.ID, "2222222222222222222222222222222222222222", "2026-06-29 00:00:00", false)

	page1, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 1})
	require.NoError(t, err)
	require.Len(t, page1.Reviews, 1)
	require.NotNil(t, page1.NextCursor)
	assert.Equal(t, *first.UUID, page1.Reviews[0].ReviewID)

	page2, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Cursor: *page1.NextCursor, Limit: 1})
	require.NoError(t, err)
	require.Len(t, page2.Reviews, 1)
	assert.Equal(t, *second.UUID, page2.Reviews[0].ReviewID)
	assert.NotNil(t, page2.NextCursor)
}

func TestExportReviewsCursorPaginatesSameCompletedTimestampPileup(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	wantIDs := make([]uuid.UUID, 0, 5)
	for i := range 5 {
		review := seedCompletedExportReview(
			t, db, repo.ID, fmt.Sprintf("%040x", i+1),
			"2026-06-29 00:00:00", false,
		)
		wantIDs = append(wantIDs, *review.UUID)
	}
	slices.SortFunc(wantIDs, uuid.UUID.Compare)

	var cursor string
	var gotIDs []uuid.UUID
	for {
		page, err := db.ExportReviews(ExportReviewsOptions{
			Profile: ExportProfileContent,
			Cursor:  cursor,
			Limit:   2,
		})
		require.NoError(t, err)
		for _, review := range page.Reviews {
			gotIDs = append(gotIDs, review.ReviewID)
		}
		if page.NextCursor == nil || len(page.Reviews) == 0 {
			break
		}
		cursor = *page.NextCursor
	}

	assert.Equal(t, wantIDs, gotIDs)
	assert.Len(t, uniqueValues(gotIDs), len(gotIDs), "same-timestamp pagination must not duplicate reviews")
}

func TestExportReviewsDefaultLimitIsBounded(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	for i := range exportDefaultPageLimit + 1 {
		seedCompletedExportReview(t, db, repo.ID, fmt.Sprintf("%040x", i+1), "2026-06-29 00:00:00", false)
	}

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent})
	require.NoError(t, err)
	assert.Len(t, page.Reviews, exportDefaultPageLimit)
	assert.True(t, page.Truncated)
	assert.NotNil(t, page.NextCursor)
}

func TestExportReviewsEmptyPageUsesEmptyReviewsArray(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	assert.NotNil(t, page.Reviews)

	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"reviews":[]`)
	assert.NotContains(t, string(encoded), `"reviews":null`)
}

func TestExportReviewsNonTruncatedPageIncludesResumeCursor(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	first := seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-06-29 00:00:00", false)

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	assert.False(t, page.Truncated)
	require.NotNil(t, page.NextCursor)
	assert.Equal(t, *first.UUID, page.Reviews[0].ReviewID)

	resumed, err := db.ExportReviews(ExportReviewsOptions{
		Profile: ExportProfileMetadata,
		Cursor:  *page.NextCursor,
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Empty(t, resumed.Reviews)
	assert.False(t, resumed.Truncated)
	assert.Nil(t, resumed.NextCursor)
}

func TestExportReviewsRejectsInvalidCursorTimestamp(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	databaseID, err := db.GetDatabaseID()
	require.NoError(t, err)
	cursor := encodeExportCursorForTest(t, databaseID, "not-a-time", testUUID("r1"))

	_, err = db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Cursor: cursor, Limit: 10})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid export cursor timestamp")
}

func TestExportReviewsRejectsCorruptCursor(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, err := db.ExportReviews(ExportReviewsOptions{
		Profile: ExportProfileContent,
		Cursor:  "not base64",
		Limit:   10,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid export cursor")
}

func TestExportReviewsRejectsCursorFromDifferentDatabaseID(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	first := seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-06-29 00:00:00", false)
	cursor := encodeExportCursorForTest(t, testUUID("other-database"), formatExportTime(first.CreatedAt), *first.UUID)

	_, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Cursor: cursor, Limit: 10})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrExportCursorDatabaseMismatch)
	assert.Contains(t, err.Error(), "database reset")
}

func TestExportReviewsRejectsNoLongerResolvableCursor(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	databaseID, err := db.GetDatabaseID()
	require.NoError(t, err)
	cursor := encodeExportCursorForTest(t, databaseID, "2026-06-29T00:00:00Z", testUUID("missing-review"))

	_, err = db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Cursor: cursor, Limit: 10})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrExportCursorNotFound)
	assert.Contains(t, err.Error(), "no longer resolvable")
}

func TestExportReviewsPanelSubagents(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	assert := assert.New(t)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	panelRun := testUUID("panel-run-1")
	memberA := seedPanelExportJob(t, db, repo.ID, panelRun, "member", "security", 0, "codex", "security", "2026-06-29 00:00:01", "- High — issue")
	memberB := seedPanelExportJob(t, db, repo.ID, panelRun, "member", "style", 1, "claude", "style", "2026-06-29 00:00:02", "No issues found.")
	synthesis := seedPanelExportJob(t, db, repo.ID, panelRun, "synthesis", "", 0, "codex", "", "2026-06-29 00:00:03", "- Medium — synthesized")
	_, err := db.Exec(`
		UPDATE review_jobs
		SET token_usage = '{"input_tokens":7,"total_output_tokens":9,"cost_usd":0.12,"has_cost":true}'
		WHERE id = ?`, memberA.JobID)
	require.NoError(t, err)
	queuedMember, err := db.EnqueueJob(EnqueueOpts{
		RepoID:           repo.ID,
		GitRef:           "aaaa..dddd",
		Agent:            "codex",
		PanelRunUUID:     &panelRun,
		PanelRole:        PanelRoleMember,
		PanelMemberName:  "queued",
		PanelMemberIndex: 2,
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET job_type = 'range' WHERE id = ?`, queuedMember.ID)
	require.NoError(t, err)
	seedPanelMemberWithoutExportableReview(t, db, repo.ID, panelRun, "failed", 3, JobStatusFailed)
	seedPanelMemberWithoutExportableReview(t, db, repo.ID, panelRun, "skipped", 4, JobStatusSkipped)
	seedPanelMemberWithoutExportableReview(t, db, repo.ID, panelRun, "running", 5, JobStatusRunning)
	emptyMember := seedPanelExportJob(t, db, repo.ID, panelRun, "member", "empty", 6, "codex", "", "2026-06-29 00:00:04", "")
	require.Nil(t, emptyMember.VerdictBool)

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	got := page.Reviews[0]
	assert.Equal(*synthesis.UUID, got.ReviewID)
	assert.Equal("dddddddddddddddddddddddddddddddddddddddd", derefString(got.CommitSHA))
	assert.Nil(got.PRNumber)
	assert.Nil(got.PRURL)
	require.Len(t, got.Subagents, 2)
	assert.Equal(*memberA.UUID, got.Subagents[0].ReviewID)
	assert.Equal("security", got.Subagents[0].Name)
	assert.Equal("fail", got.Subagents[0].Verdict)
	assert.Equal("security", derefString(got.Subagents[0].ReviewType))
	assert.Contains(derefString(got.Subagents[0].Content), "- High — issue")
	assert.Equal(int64(7), derefInt64(got.Subagents[0].Cost.TokensIn))
	assert.Equal(int64(9), derefInt64(got.Subagents[0].Cost.TokensOut))
	assert.InDelta(0.12, derefFloat64(got.Subagents[0].Cost.USD), 1e-9)
	assert.Equal(*memberB.UUID, got.Subagents[1].ReviewID)
	assert.Equal("style", got.Subagents[1].Name)
	assert.Equal("pass", got.Subagents[1].Verdict)

	metadata, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Limit: 10})
	require.NoError(t, err)
	require.Len(t, metadata.Reviews, 1)
	require.Len(t, metadata.Reviews[0].Subagents, 2)
	assert.Nil(metadata.Reviews[0].Content)
	assert.Nil(metadata.Reviews[0].Subagents[0].Content)
}

func TestExportReviewsSynthesisWithoutMembersUsesEmptySubagentsArray(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	synthesis := seedPanelExportJob(t, db, repo.ID, testUUID("panel-run-empty"), "synthesis", "", 0, "codex", "", "2026-06-29 00:00:03", "No issues found.")

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	assert.Equal(t, *synthesis.UUID, page.Reviews[0].ReviewID)
	assert.NotNil(t, page.Reviews[0].Subagents)

	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"subagents":[]`)
	assert.NotContains(t, string(encoded), `"subagents":null`)
}

func TestExportReviewsCISynthesisFields(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	panelRun := testUUID("ci-panel-run")
	synthesis := seedPanelExportJob(t, db, repo.ID, panelRun, "synthesis", "", 0, "codex", "", "2026-06-29 00:00:03", "- Medium — synthesized")
	_, err := db.Exec(`
		UPDATE review_jobs
		SET ci_base_branch = 'main',
		    git_ref = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa..bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
		WHERE id = ?`, synthesis.JobID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO ci_pr_panels (github_repo, pr_number, head_sha, panel_run_uuid, synthesis_job_id)
		VALUES ('acme/widgets', 42, 'cccccccccccccccccccccccccccccccccccccccc', ?, ?)`,
		panelRun, synthesis.JobID)
	require.NoError(t, err)

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	got := page.Reviews[0]
	assert.Equal(t, "main", derefString(got.Branch))
	assert.Equal(t, "cccccccccccccccccccccccccccccccccccccccc", derefString(got.CommitSHA))
	assert.Equal(t, int64(42), derefInt64(got.PRNumber))
	assert.Equal(t, "https://github.com/acme/widgets/pull/42", derefString(got.PRURL))
}

func TestExportReviewsPreservesLargeContent(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	commit := createCommit(t, db, repo.ID, "cccccccccccccccccccccccccccccccccccccccc")
	job := enqueueJob(t, db, repo.ID, commit.ID, commit.SHA)
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJobResult(
		job.ID, "codex", "prompt", ReviewCompletion{
			StructuredOutput: reviewFixtureJSON(strings.Repeat("x", (1<<20)+100)),
			Output:           strings.Repeat("x", (1<<20)+100),
			Verdict:          VerdictFail,
		},
	))

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	require.NotNil(t, page.Reviews[0].Content)
	assert.Contains(t, *page.Reviews[0].Content, strings.Repeat("x", (1<<20)+100))
}

func TestOpenBackfillsVerdictBoolForExport(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(dbPath)
	require.NoError(t, err)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	commit := createCommit(t, db, repo.ID, "dddddddddddddddddddddddddddddddddddddddd")
	job := enqueueJob(t, db, repo.ID, commit.ID, commit.SHA)
	claimJob(t, db, "w1")
	require.NoError(t, completeReviewFixture(db, job.ID, "codex", "prompt", "No issues found."))
	_, err = db.Exec(`UPDATE reviews SET verdict_bool = NULL WHERE job_id = ?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	reopened, err := Open(dbPath)
	require.NoError(t, err)
	defer reopened.Close()

	page, err := reopened.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	assert.Equal(t, "pass", page.Reviews[0].Verdict)
}

func TestUpsertPulledReviewWithEmptyOutputIsNotExportable(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	commit := createCommit(t, db, repo.ID, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	job := enqueueJob(t, db, repo.ID, commit.ID, commit.SHA)
	_, err := db.Exec(`UPDATE review_jobs SET status = 'done' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, db.UpsertPulledReview(PulledReview{
		UUID:               testUUID("pulled-review"),
		JobUUID:            *job.UUID,
		Agent:              "codex",
		Prompt:             "prompt",
		Output:             "",
		UpdatedByMachineID: testUUID("remote"),
		CreatedAt:          time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
		UpdatedAt:          time.Date(2026, 6, 29, 0, 0, 1, 0, time.UTC),
	}))

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, page.Reviews)
}

func seedCompletedExportReview(t *testing.T, db *DB, repoID int64, sha, completedAt string, closed bool) *Review {
	t.Helper()
	commit := createCommit(t, db, repoID, sha)
	job := enqueueJob(t, db, repoID, commit.ID, sha)
	markExportJobRunning(t, db, job.ID)
	require.NoError(t, completeReviewFixture(db, job.ID, "codex", "prompt", "No issues found."))
	_, err := db.Exec(`UPDATE reviews SET uuid = ?, created_at = ?, closed = ? WHERE job_id = ?`, testUUID("review-"+sha), completedAt, boolInt(closed), job.ID)
	require.NoError(t, err)
	review, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	return review
}

// setExportReviewStructuredOutput overwrites the stored document bytes so a
// test controls exactly what is in the column. A nil value stores NULL.
func setExportReviewStructuredOutput(t *testing.T, db *DB, jobID int64, document any) {
	t.Helper()
	_, err := db.Exec(`UPDATE reviews SET structured_output = ? WHERE job_id = ?`, document, jobID)
	require.NoError(t, err)
}

// setExportReviewUpdatedAt overwrites a review's updated_at. A nil value
// stores NULL, matching rows written before the column existed.
func setExportReviewUpdatedAt(t *testing.T, db *DB, jobID int64, updatedAt any) {
	t.Helper()
	_, err := db.Exec(`UPDATE reviews SET updated_at = ? WHERE job_id = ?`, updatedAt, jobID)
	require.NoError(t, err)
}

func seedPanelMemberWithoutExportableReview(t *testing.T, db *DB, repoID int64, panelRun uuid.UUID, memberName string, memberIndex int, status JobStatus) {
	t.Helper()
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:           repoID,
		GitRef:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa..dddddddddddddddddddddddddddddddddddddddd",
		Agent:            "codex",
		PanelRunUUID:     &panelRun,
		PanelRole:        PanelRoleMember,
		PanelMemberName:  memberName,
		PanelMemberIndex: memberIndex,
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET job_type = 'range', status = ? WHERE id = ?`, status, job.ID)
	require.NoError(t, err)
}

func seedPanelExportJob(
	t *testing.T,
	db *DB,
	repoID int64,
	panelRun uuid.UUID,
	role string,
	memberName string,
	memberIndex int,
	agentName string,
	reviewType string,
	completedAt string,
	output string,
) *Review {
	t.Helper()
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:           repoID,
		GitRef:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa..dddddddddddddddddddddddddddddddddddddddd",
		Agent:            agentName,
		ReviewType:       reviewType,
		PanelRunUUID:     &panelRun,
		PanelRole:        role,
		PanelMemberName:  memberName,
		PanelMemberIndex: memberIndex,
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET job_type = ?, started_at = '2026-06-29T00:00:00Z' WHERE id = ?`, panelJobType(role), job.ID)
	require.NoError(t, err)
	markExportJobRunning(t, db, job.ID)
	require.NoError(t, completeReviewFixture(db, job.ID, agentName, "prompt", output))
	_, err = db.Exec(`UPDATE reviews SET created_at = ? WHERE job_id = ?`, completedAt, job.ID)
	require.NoError(t, err)
	review, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	return review
}

func markExportJobRunning(t *testing.T, db *DB, jobID int64) {
	t.Helper()
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = 'export-test', started_at = '2026-06-29T00:00:00Z' WHERE id = ?`, jobID)
	require.NoError(t, err)
}

func panelJobType(role string) string {
	if role == PanelRoleSynthesis {
		return JobTypeSynthesis
	}
	return JobTypeRange
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func derefFloat64(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func encodeExportCursorForTest(t *testing.T, databaseID uuid.UUID, completedAt string, reviewID uuid.UUID) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"version":      1,
		"database_id":  databaseID,
		"completed_at": completedAt,
		"review_id":    reviewID,
	})
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(data)
}

func uniqueValues[T comparable](values []T) map[T]struct{} {
	out := make(map[T]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}
