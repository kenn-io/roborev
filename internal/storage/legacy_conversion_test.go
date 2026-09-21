package storage

import (
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/structuredreview"
)

const (
	legacyFindingsMarkdown = "## Review Findings\n\n" +
		"- **Severity**: High\n" +
		"- **Location**: store/save.go:88\n" +
		"- **Problem**: The write is not atomic.\n" +
		"- **Fix**: Write to a temporary file and rename it.\n" +
		"---\n" +
		"- **Severity**: Low\n" +
		"- **Location**: store/save.go:12\n" +
		"- **Problem**: The comment is stale.\n" +
		"- **Fix**: Update the comment.\n\n" +
		"## Summary\n\nThe change adds a save routine."
	legacyNoIssuesMarkdown = "No issues found.\n\nSummary: The change renames a helper."
	legacyProseMarkdown    = "The save routine looks risky because the write is not atomic. Consider a rename."
)

// legacyFixture is one Markdown-only review as databases held it before
// reviews were stored as JSON.
type legacyFixture struct {
	jobID     int64
	markdown  string
	verdict   any
	closed    bool
	createdAt string
}

// seedLegacyMarkdownReview stores a Markdown-only review on a new job, with an
// old created_at and updated_at so a test can tell what a conversion changed.
func seedLegacyMarkdownReview(t *testing.T, db *DB, repoID int64, name, markdown string, verdict any, closed bool) legacyFixture {
	t.Helper()
	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repoID, GitRef: name, Agent: "test"})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'done' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	fixture := legacyFixture{jobID: job.ID, markdown: markdown, verdict: verdict, closed: closed, createdAt: "2026-01-05 00:00:00"}
	_, err = db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, created_at, updated_at, closed, verdict_bool, uuid, synced_at)
 VALUES (?, 'test', 'prompt', ?, ?, ?, ?, ?, ?, '2026-01-06T00:00:00Z')`,
		job.ID, markdown, fixture.createdAt, "2026-01-05T00:00:00Z", closed, verdict, testUUID("legacy-"+name))
	require.NoError(t, err)
	return fixture
}

// archiveAsOlderRelease moves every Markdown-only review to legacy_reviews the
// way the first JSON release did, before any automatic conversion existed.
func archiveAsOlderRelease(t *testing.T, db *DB) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO legacy_reviews (` + legacyReviewColumns + `, migration_error, resolved_at)
 SELECT ` + legacyReviewColumns + `, 'No valid review JSON document; AI conversion required', NULL
 FROM reviews WHERE structured_output IS NULL`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM reviews WHERE structured_output IS NULL`)
	require.NoError(t, err)
}

type storedLegacyReview struct {
	output, document, createdAt, updatedAt string
	closed                                 bool
	verdict                                *bool
	synced                                 *string
}

func readStoredReview(t *testing.T, db *DB, jobID int64) storedLegacyReview {
	t.Helper()
	var got storedLegacyReview
	require.NoError(t, db.QueryRow(`SELECT output, COALESCE(structured_output, ''), created_at, updated_at, closed, verdict_bool, synced_at
 FROM reviews WHERE job_id = ?`, jobID).Scan(&got.output, &got.document, &got.createdAt, &got.updatedAt, &got.closed, &got.verdict, &got.synced))
	return got
}

func TestUpgradeConvertsMarkdownReviewsRoborevWroteAndArchivesTheRest(t *testing.T) {
	env := setupJobEnv(t, "/tmp/legacy-upgrade", "upgrade-head")
	findings := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "findings", legacyFindingsMarkdown, 0, true)
	clean := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "clean", legacyNoIssuesMarkdown, 1, false)
	marker := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "marker", "SEVERITY_THRESHOLD_MET", 1, false)
	rendered := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "rendered", structuredreview.Document{
		SchemaVersion: 2, Summary: "The change adds a cache.", Verdict: structuredreview.VerdictFail,
		Findings: []structuredreview.Finding{{Severity: "medium", Problem: "The cache is never invalidated.", Fix: "Invalidate on write."}},
	}.Markdown(""), 0, false)
	prose := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "prose", legacyProseMarkdown, 0, false)

	require.NoError(t, env.db.migrateLegacyReviews())

	t.Run("findings list becomes a document with every stated field", func(t *testing.T) {
		assert := assert.New(t)
		got := readStoredReview(t, env.db, findings.jobID)
		assert.Empty(got.output)
		assert.JSONEq(`{"schema_version":1,"summary":"The change adds a save routine.","findings":[
			{"severity":"high","problem":"The write is not atomic.","fix":"Write to a temporary file and rename it.","location":"store/save.go:88"},
			{"severity":"low","problem":"The comment is stale.","fix":"Update the comment.","location":"store/save.go:12"}]}`, got.document)
		assert.True(got.closed, "closed state is unchanged")
		assert.Equal(findings.createdAt, got.createdAt, "completion time is unchanged")
		require.NotNil(t, got.verdict)
		assert.False(*got.verdict, "verdict is unchanged")
		assert.Nil(got.synced, "the converted review is sent to the PostgreSQL mirror again")
		assert.NotEqual("2026-01-05T00:00:00Z", got.updatedAt, "updated_at records that the review changed")

		review, err := env.db.GetReviewByJobID(findings.jobID)
		require.NoError(t, err)
		assert.Equal(testUUID("legacy-findings"), *review.UUID, "review identity is unchanged")
		assert.Contains(review.Output, "The write is not atomic.")
	})

	t.Run("reviews without findings keep their pass verdict", func(t *testing.T) {
		for _, fixture := range []legacyFixture{clean, marker} {
			got := readStoredReview(t, env.db, fixture.jobID)
			doc, err := structuredreview.Decode(jsontext.Value(got.document))
			require.NoError(t, err)
			assert.Empty(t, doc.Findings)
			require.NotNil(t, got.verdict)
			assert.True(t, *got.verdict)
		}
		got := readStoredReview(t, env.db, clean.jobID)
		assert.JSONEq(t, `{"schema_version":1,"summary":"The change renames a helper.","findings":[]}`, got.document)
	})

	t.Run("rendered Markdown keeps the stated agent verdict", func(t *testing.T) {
		got := readStoredReview(t, env.db, rendered.jobID)
		assert.JSONEq(t, `{"schema_version":2,"summary":"The change adds a cache.","verdict":"fail","findings":[
			{"severity":"medium","problem":"The cache is never invalidated.","fix":"Invalidate on write.","location":null}]}`, got.document)
	})

	t.Run("prose stays archived with the refusal reason", func(t *testing.T) {
		_, err := env.db.GetReviewByJobID(prose.jobID)
		require.ErrorIs(t, err, ErrLegacyReviewMigration)
		records, err := env.db.UnresolvedLegacyReviews()
		require.NoError(t, err)
		require.Len(t, records, 1)
		assert.Equal(t, prose.jobID, records[0].JobID)
		assert.Equal(t, legacyProseMarkdown, records[0].Output)
		assert.Contains(t, records[0].Reason, "automatic conversion refused (unrecognized_format)")
	})

	t.Run("originals stay archived and a second upgrade changes nothing", func(t *testing.T) {
		var archived int
		require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM legacy_reviews WHERE resolved_at IS NOT NULL AND output != ''`).Scan(&archived))
		assert.Equal(t, 4, archived)
		before := readStoredReview(t, env.db, findings.jobID)
		require.NoError(t, env.db.migrateLegacyReviews())
		assert.Equal(t, before, readStoredReview(t, env.db, findings.jobID))
		var rows int
		require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM legacy_reviews`).Scan(&rows))
		assert.Equal(t, 5, rows)
	})

	t.Run("converted reviews are exported again", func(t *testing.T) {
		page, err := env.db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
		require.NoError(t, err)
		require.Len(t, page.Reviews, 4, "the prose review stays out of the export")
		var contents []string
		for _, review := range page.Reviews {
			assert.Equal(t, "2026-01-05T00:00:00Z", review.CompletedAt)
			contents = append(contents, derefString(review.Content))
		}
		assert.Contains(t, strings.Join(contents, "\n"), "**Problem:** The write is not atomic.")
	})
}

func TestConvertLegacyReviewsRestoresArchivedReviews(t *testing.T) {
	env := setupJobEnv(t, "/tmp/legacy-convert", "convert-head")
	findings := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "findings", legacyFindingsMarkdown, 0, true)
	clean := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "clean", legacyNoIssuesMarkdown, 1, false)
	prose := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "prose", legacyProseMarkdown, 0, false)
	noFix := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "no-fix",
		"## Review Findings\n\n- **Severity**: High\n- **Problem**: The write is not atomic.\n\n## Summary\n\nThe change adds a save routine.", 0, false)
	// The findings say fail, but roborev recorded a pass for this review.
	mismatch := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "mismatch", legacyFindingsMarkdown, 1, false)
	archiveAsOlderRelease(t, env.db)

	wantRefused := map[string]int{"unrecognized_format": 1, "missing_fix": 1, "verdict_mismatch": 1}

	dryRun, err := env.db.ConvertLegacyReviews(true)
	require.NoError(t, err)
	assert.Equal(t, LegacyConversionReport{Unresolved: 5, Converted: 2, Refused: wantRefused, DryRun: true}, dryRun)
	unresolved, err := env.db.UnresolvedLegacyReviews()
	require.NoError(t, err)
	assert.Len(t, unresolved, 5, "a dry run changes nothing")
	_, err = env.db.GetReviewByJobID(findings.jobID)
	require.ErrorIs(t, err, ErrLegacyReviewMigration)

	report, err := env.db.ConvertLegacyReviews(false)
	require.NoError(t, err)
	assert.Equal(t, LegacyConversionReport{Unresolved: 5, Converted: 2, Refused: wantRefused}, report)

	got := readStoredReview(t, env.db, findings.jobID)
	assert.True(t, got.closed, "closed state is unchanged")
	assert.Equal(t, findings.createdAt, got.createdAt)
	require.NotNil(t, got.verdict)
	assert.False(t, *got.verdict)
	assert.Nil(t, got.synced)
	doc, err := structuredreview.Decode(jsontext.Value(got.document))
	require.NoError(t, err)
	require.Len(t, doc.Findings, 2)
	assert.Equal(t, structuredreview.Finding{
		Severity: "high", Problem: "The write is not atomic.", Fix: "Write to a temporary file and rename it.", Location: "store/save.go:88",
	}, doc.Findings[0])
	review, err := env.db.GetReviewByJobID(clean.jobID)
	require.NoError(t, err)
	assert.Equal(t, VerdictPass, review.Verdict())
	assert.Equal(t, testUUID("legacy-clean"), *review.UUID)

	for _, fixture := range []legacyFixture{prose, noFix, mismatch} {
		_, err := env.db.GetReviewByJobID(fixture.jobID)
		require.ErrorIs(t, err, ErrLegacyReviewMigration, "refused reviews stay archived")
	}
	var original string
	require.NoError(t, env.db.QueryRow(`SELECT output FROM legacy_reviews WHERE job_id = ?`, findings.jobID).Scan(&original))
	assert.Equal(t, legacyFindingsMarkdown, original, "the original stays archived")

	again, err := env.db.ConvertLegacyReviews(false)
	require.NoError(t, err)
	assert.Equal(t, LegacyConversionReport{Unresolved: 3, Converted: 0, Refused: wantRefused}, again, "a second run has nothing new to convert")
	var active int
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM reviews WHERE job_id IN (?, ?)`, findings.jobID, clean.jobID).Scan(&active))
	assert.Equal(t, 2, active, "a second run does not duplicate restored reviews")
}

func TestConvertLegacyReviewsLeavesAJobWithAnActiveReviewAlone(t *testing.T) {
	env := setupJobEnv(t, "/tmp/legacy-active", "active-head")
	archived := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "rerun", legacyNoIssuesMarkdown, 1, false)
	archiveAsOlderRelease(t, env.db)
	_, err := env.db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, structured_output, uuid) VALUES (?, 'test', 'prompt', '', ?, ?)`,
		archived.jobID, `{"schema_version":2,"summary":"Reviewed again.","verdict":"pass","findings":[]}`, testUUID("rerun-review"))
	require.NoError(t, err)

	report, err := env.db.ConvertLegacyReviews(false)
	require.NoError(t, err)

	assert.Equal(t, LegacyConversionReport{Unresolved: 1, Refused: map[string]int{"active_review_exists": 1}}, report)
	review, err := env.db.GetReviewByJobID(archived.jobID)
	require.NoError(t, err)
	assert.Equal(t, "Reviewed again.", review.StructuredOutput["summary"])
}

// seedLegacyPanel creates a synthesis job with two finished members whose
// reviews already have documents, and returns the synthesis job ID.
func seedLegacyPanel(t *testing.T, env jobEnv, name string) int64 {
	t.Helper()
	runID := testUUID("legacy-panel-" + name)
	synthesis, err := env.db.EnqueueJob(EnqueueOpts{RepoID: env.repo.ID, GitRef: name, Agent: "test", PanelRunUUID: &runID, PanelRole: PanelRoleSynthesis})
	require.NoError(t, err)
	_, err = env.db.Exec(`UPDATE review_jobs SET job_type = 'synthesis', status = 'done' WHERE id = ?`, synthesis.ID)
	require.NoError(t, err)
	for i, reviewType := range []string{"security", "design"} {
		member, err := env.db.EnqueueJob(EnqueueOpts{
			RepoID: env.repo.ID, GitRef: name, Agent: "test", ReviewType: reviewType,
			PanelRunUUID: &runID, PanelRole: PanelRoleMember, PanelMemberIndex: i,
		})
		require.NoError(t, err)
		_, err = env.db.Exec(`UPDATE review_jobs SET status = 'done' WHERE id = ?`, member.ID)
		require.NoError(t, err)
		_, err = env.db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, structured_output, uuid) VALUES (?, 'test', 'prompt', '', ?, ?)`,
			member.ID, `{"schema_version":2,"summary":"Clean change.","verdict":"pass","findings":[]}`, testUUID(fmt.Sprintf("%s-member-%d", name, i)))
		require.NoError(t, err)
	}
	return synthesis.ID
}

func TestConvertLegacySynthesisReviews(t *testing.T) {
	env := setupJobEnv(t, "/tmp/legacy-synthesis-convert", "synthesis-convert-head")
	insert := func(jobID int64, name, markdown string, verdict int) {
		_, err := env.db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, verdict_bool, uuid) VALUES (?, 'test', 'prompt', ?, ?, ?)`,
			jobID, markdown, verdict, testUUID("synthesis-"+name))
		require.NoError(t, err)
	}
	clean := seedLegacyPanel(t, env, "clean")
	insert(clean, "clean", "No issues found.", 1)
	attributed := seedLegacyPanel(t, env, "attributed")
	insert(attributed, "attributed", structuredreview.Document{
		SchemaVersion: 2, Summary: "One reviewer found a problem.", Verdict: structuredreview.VerdictFail,
		SourceLabels: []string{"test (security)", "test (design)"},
		Findings:     []structuredreview.Finding{{Severity: "high", Problem: "The token is logged.", Fix: "Redact the token.", Sources: []int{2}}},
	}.Markdown(""), 0)
	// Older synthesis prompts asked for the ordinary findings list, which
	// never says which input review reported a finding.
	unattributed := seedLegacyPanel(t, env, "unattributed")
	insert(unattributed, "unattributed", legacyFindingsMarkdown, 0)

	require.NoError(t, env.db.migrateLegacyReviews())

	var document string
	require.NoError(t, env.db.QueryRow(`SELECT structured_output FROM reviews WHERE job_id = ?`, clean).Scan(&document))
	assert.JSONEq(t, `{"schema_version":1,"summary":"No issues found.","findings":[],"source_labels":["test (security)","test (design)"]}`, document)

	require.NoError(t, env.db.QueryRow(`SELECT structured_output FROM reviews WHERE job_id = ?`, attributed).Scan(&document))
	doc, err := structuredreview.Decode(jsontext.Value(document))
	require.NoError(t, err)
	require.Len(t, doc.Findings, 1)
	assert.Equal(t, []int{2}, doc.Findings[0].Sources, "the stated reviewer is matched to its review number")

	_, err = env.db.GetReviewByJobID(unattributed)
	require.ErrorIs(t, err, ErrLegacyReviewMigration)
	var reason string
	require.NoError(t, env.db.QueryRow(`SELECT migration_error FROM legacy_reviews WHERE job_id = ? AND resolved_at IS NULL`, unattributed).Scan(&reason))
	assert.Contains(t, reason, "automatic conversion refused (unrecoverable_sources)")
}

func TestConvertedReviewIsNotOverwrittenByAMarkdownOnlyPeer(t *testing.T) {
	env := setupJobEnv(t, "/tmp/legacy-peer", "peer-head")
	fixture := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "peer", legacyFindingsMarkdown, 0, false)
	require.NoError(t, env.db.migrateLegacyReviews())
	before := readStoredReview(t, env.db, fixture.jobID)
	job, err := env.db.GetJobByID(fixture.jobID)
	require.NoError(t, err)

	// A machine on an older release still holds the Markdown and syncs it
	// with a newer timestamp.
	require.NoError(t, env.db.UpsertPulledReview(PulledReview{
		UUID: testUUID("legacy-peer"), JobUUID: *job.UUID, Agent: "test", Prompt: "prompt",
		Output: strings.ReplaceAll(legacyFindingsMarkdown, "High", "Low"), UpdatedByMachineID: testUUID("old-peer"),
		CreatedAt: time.Now(), UpdatedAt: time.Now().Add(time.Hour),
	}))

	assert.Equal(t, before, readStoredReview(t, env.db, fixture.jobID))
	var rows int
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM reviews WHERE job_id = ?`, fixture.jobID).Scan(&rows))
	assert.Equal(t, 1, rows)
}
