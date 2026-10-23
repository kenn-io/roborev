package storage

import (
	"context"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/findingid"
	"go.kenn.io/roborev/pkg/structuredreview"
)

const snapshotDocument = `{"schema_version":2,"summary":"Two problems.","verdict":"fail","findings":[` +
	`{"severity":"high","problem":"Nil dereference.","fix":"Check first.","location":"a.go:10"},` +
	`{"severity":"low","problem":"Typo.","fix":"Spell it.","location":null}]}`

// completeWithDocument claims the next queued job and completes it with doc.
func completeWithDocument(t *testing.T, db *DB, jobID int64, doc string) {
	t.Helper()
	claimed := claimJob(t, db, "worker-1")
	require.Equal(t, jobID, claimed.ID)
	require.NoError(t, completeReviewFixture(db, jobID, "codex", "", doc))
}

func reviewUUIDOf(t *testing.T, db *DB, jobID int64) uuid.UUID {
	t.Helper()
	var raw string
	require.NoError(t, db.QueryRow(`SELECT uuid FROM reviews WHERE job_id = ?`, jobID).Scan(&raw))
	return uuid.MustParse(raw)
}

func TestAssignFindingSnapshotsPinsEveryFinding(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/snapshots-pin", "pin123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)

	got, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)

	wantHash, err := findingid.DocumentHash([]byte(snapshotDocument))
	require.NoError(t, err)
	reviewUUID := reviewUUIDOf(t, env.db, env.job.ID)
	job, err := env.db.GetJobByID(env.job.ID)
	require.NoError(t, err)
	require.NotNil(t, job.UUID)

	assert := assert.New(t)
	for i, snap := range got {
		assert.Equal(env.job.ID, snap.JobID)
		assert.Equal(*job.UUID, snap.JobUUID)
		assert.Equal(reviewUUID, snap.ReviewUUID)
		assert.Equal(wantHash, snap.DocumentHash)
		assert.Equal(i, snap.Index)
		assert.False(snap.CreatedAt.IsZero())

		id, err := findingid.Parse(snap.Selector)
		require.NoError(t, err)
		assert.Equal(reviewUUID, id.ReviewUUID)
		assert.Equal(i, id.Index)
		assert.True(id.MatchesHash(wantHash))
	}
	assert.Equal(structuredreview.Finding{
		Severity: "high", Problem: "Nil dereference.", Fix: "Check first.", Location: "a.go:10",
	}, got[0].Finding)
	assert.Equal(structuredreview.Finding{Severity: "low", Problem: "Typo.", Fix: "Spell it."}, got[1].Finding,
		"a null location is an empty location")
	assert.Empty(got[0].Provenance.PanelRole, "a standalone review has no panel provenance")
}

func TestAssignFindingSnapshotsIsIdempotent(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/snapshots-idem", "idem123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)

	first, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)
	second, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)
	assert.Equal(t, first, second, "the second call returns the stored snapshots, not new ones")

	var rows int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM finding_snapshots`).Scan(&rows))
	assert.Equal(t, 2, rows)
}

func TestDuplicateFindingTextGetsDistinctSelectors(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/snapshots-dup", "dup123")
	finding := `{"severity":"high","problem":"Same text.","fix":"Same fix.","location":"a.go:1"}`
	completeWithDocument(t, env.db, env.job.ID,
		`{"schema_version":2,"summary":"Repeats.","verdict":"fail","findings":[`+finding+`,`+finding+`,`+finding+`]}`)

	got, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)
	require.Len(t, got, 3)
	selectors := map[string]bool{}
	for _, snap := range got {
		selectors[snap.Selector] = true
	}
	assert.Len(t, selectors, 3, "identical findings are told apart by position")
}

func TestNoFindingIdentitiesForLegacyOrFreeFormReviews(t *testing.T) {
	t.Parallel()
	t.Run("legacy prose document", func(t *testing.T) {
		t.Parallel()
		env := setupJobEnv(t, "/tmp/snapshots-legacy", "legacy123")
		completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
		_, err := env.db.Exec(`UPDATE reviews SET structured_output = ? WHERE job_id = ?`,
			`{"schema_version":0,"legacy":{"markdown":"No issues found.","recorded_verdict":true}}`, env.job.ID)
		require.NoError(t, err)

		got, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
	t.Run("review without structured output", func(t *testing.T) {
		t.Parallel()
		env := setupJobEnv(t, "/tmp/snapshots-prose", "prose123")
		completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
		_, err := env.db.Exec(`UPDATE reviews SET structured_output = NULL WHERE job_id = ?`, env.job.ID)
		require.NoError(t, err)

		got, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
	t.Run("clean review", func(t *testing.T) {
		t.Parallel()
		env := setupJobEnv(t, "/tmp/snapshots-clean", "clean123")
		completeWithDocument(t, env.db, env.job.ID,
			`{"schema_version":2,"summary":"Clean.","verdict":"pass","findings":[]}`)

		got, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
	t.Run("no review yet", func(t *testing.T) {
		t.Parallel()
		env := setupJobEnv(t, "/tmp/snapshots-none", "none123")
		_, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
		require.ErrorIs(t, err, ErrNoReview)
	})
}

func TestFindingSnapshotsSurviveARerunDeletingTheReview(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/snapshots-rerun", "rerun123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	old, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)
	require.Len(t, old, 2)
	oldReview := old[0].ReviewUUID

	require.NoError(t, env.db.ReenqueueJob(env.job.ID, ReenqueueOpts{}))
	var reviews int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM reviews WHERE job_id = ?`, env.job.ID).Scan(&reviews))
	require.Zero(t, reviews, "the rerun deleted the review")

	kept, err := env.db.FindingSnapshot(t.Context(), old[1].Selector)
	require.NoError(t, err, "a selector handed out before the rerun still resolves")
	assert.Equal(t, old[1], kept)

	// The rerun's own review gets its own identities beside the old ones.
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	fresh, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)
	require.Len(t, fresh, 2)
	assert.NotEqual(t, oldReview, fresh[0].ReviewUUID)
	assert.NotEqual(t, old[0].Selector, fresh[0].Selector, "a new review is a new set of selectors")
	assert.Equal(t, old[0].DocumentHash, fresh[0].DocumentHash, "the same findings hash alike")

	again, err := env.db.FindingSnapshot(t.Context(), old[0].Selector)
	require.NoError(t, err)
	assert.Equal(t, old[0], again)
}

func TestAssignFindingSnapshotsRefusesAChangedDocument(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/snapshots-stale", "stale123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	pinned, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)

	// The same review UUID now holds different findings, as a sync of an
	// edited review would leave it.
	_, err = env.db.Exec(`UPDATE reviews SET structured_output = ? WHERE job_id = ?`,
		`{"schema_version":2,"summary":"Edited.","verdict":"fail","findings":[`+
			`{"severity":"low","problem":"Another.","fix":"Other.","location":null}]}`, env.job.ID)
	require.NoError(t, err)

	_, err = env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.ErrorIs(t, err, ErrFindingChanged)
	kept, err := env.db.FindingSnapshot(t.Context(), pinned[0].Selector)
	require.NoError(t, err)
	assert.Equal(t, pinned[0], kept, "the pinned snapshot is never rewritten")
}

func TestFindingSnapshotLookup(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/snapshots-lookup", "lookup123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	snaps, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)

	t.Run("full selector", func(t *testing.T) {
		got, err := env.db.FindingSnapshot(t.Context(), snaps[1].Selector)
		require.NoError(t, err)
		assert.Equal(t, snaps[1], got)
	})
	t.Run("capitals", func(t *testing.T) {
		got, err := env.db.FindingSnapshot(t.Context(), strings.ToUpper(snaps[0].Selector))
		require.NoError(t, err)
		assert.Equal(t, snaps[0], got)
	})
	t.Run("a shortened selector is not a selector", func(t *testing.T) {
		for _, n := range []int{len("f:"), len("f:") + 8, len(snaps[0].Selector) - 1} {
			_, err := env.db.FindingSnapshot(t.Context(), snaps[0].Selector[:n])
			require.ErrorIs(t, err, findingid.ErrInvalid, "%d characters", n)
		}
	})
	t.Run("a well-formed selector nothing was pinned under", func(t *testing.T) {
		parsed, err := findingid.Parse(snaps[0].Selector)
		require.NoError(t, err)
		// Same review and position, a document hash the review never had.
		stale, err := findingid.New(parsed.ReviewUUID, parsed.Index, strings.Repeat("ab", 32))
		require.NoError(t, err)
		_, err = env.db.FindingSnapshot(t.Context(), stale.String())
		require.ErrorIs(t, err, findingid.ErrNotFound, "a stale document hash names nothing")
	})
	t.Run("malformed text", func(t *testing.T) {
		for _, in := range []string{"", "nonsense", "f:", "f:%", "f:_", "f:*", snaps[0].Selector + "%", " " + snaps[0].Selector} {
			_, err := env.db.FindingSnapshot(t.Context(), in)
			require.ErrorIs(t, err, findingid.ErrInvalid, "%q", in)
		}
	})
}

// A selector that is not well formed is refused before the database is asked.
func TestFindingSnapshotChecksTheSelectorBeforeQuerying(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	require.NoError(t, db.Close())

	_, err := db.FindingSnapshot(t.Context(), "f:abc")
	require.ErrorIs(t, err, findingid.ErrInvalid, "a closed database was not consulted")
	well, err := findingid.New(uuid.MustParse("0b9f1c2e-4d5a-4e6f-8a7b-1c2d3e4f5a6b"), 0, strings.Repeat("ab", 32))
	require.NoError(t, err)
	_, err = db.FindingSnapshot(t.Context(), well.String())
	require.Error(t, err)
	require.NotErrorIs(t, err, findingid.ErrInvalid, "a well-formed selector does reach the database")
}

func TestFindingSnapshotRecordsPanelProvenance(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/snapshots-panel")
	runID := uuid.MustParse("5d0e8c3a-1f2b-4c6d-9e7f-8a9b0c1d2e3f")

	member, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, GitRef: "base..head", Agent: "test", JobType: JobTypeRange,
		PanelRunUUID: &runID, PanelRole: "member", PanelName: "branch_final",
		PanelMemberName: "security", PanelMemberIndex: 2,
	})
	require.NoError(t, err)
	completeWithDocument(t, db, member.ID, snapshotDocument)
	got, err := db.AssignFindingSnapshots(t.Context(), member.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, FindingProvenance{
		PanelRunUUID: &runID, PanelRole: "member", PanelName: "branch_final",
		PanelMemberName: "security", PanelMemberIndex: new(2),
	}, got[0].Provenance)

	synthesis, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, GitRef: "base..head", Agent: "test", JobType: JobTypeSynthesis,
		PanelRunUUID: &runID, PanelRole: "synthesis", PanelName: "branch_final",
	})
	require.NoError(t, err)
	completeWithDocument(t, db, synthesis.ID, snapshotDocument)
	combined, err := db.AssignFindingSnapshots(t.Context(), synthesis.ID)
	require.NoError(t, err)
	require.Len(t, combined, 2)
	assert.Equal(t, "synthesis", combined[0].Provenance.PanelRole)
	assert.Equal(t, []string{"codex"}, combined[0].Provenance.SourceLabels,
		"a combined finding keeps the labels of the reviews it cites")
	assert.Nil(t, combined[0].Provenance.PanelMemberIndex)
}

func TestDeletingARepoDeletesItsFindingSnapshots(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/snapshots-delete", "delete123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	snaps, err := env.db.AssignFindingSnapshots(t.Context(), env.job.ID)
	require.NoError(t, err)
	require.NotEmpty(t, snaps)

	require.NoError(t, env.db.DeleteRepo(env.repo.ID, true))
	_, err = env.db.FindingSnapshot(t.Context(), snaps[0].Selector)
	require.ErrorIs(t, err, findingid.ErrNotFound, "finding text does not outlive the job it came from")
}

func TestAssignFindingSnapshotsDoesNotRecreateOrphansAfterRepoDeletion(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/snapshots-delete-race", "delete-race123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)

	type result struct {
		snapshots []FindingSnapshot
		err       error
	}
	loaded := make(chan struct{})
	resume := make(chan struct{})
	done := make(chan result, 1)
	go func() {
		snapshots, err := env.db.assignFindingSnapshots(t.Context(), env.job.ID, func() {
			close(loaded)
			<-resume
		})
		done <- result{snapshots: snapshots, err: err}
	}()
	<-loaded
	deleteErr := env.db.DeleteRepo(env.repo.ID, true)
	close(resume)
	require.NoError(t, deleteErr)

	got := <-done
	require.ErrorIs(t, got.err, ErrNoReview)
	assert.Empty(t, got.snapshots)
	var rows int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM finding_snapshots`).Scan(&rows))
	assert.Zero(t, rows, "a stale source must not recreate review text after its repository is deleted")
}

func TestOpenCreatesFindingSnapshotsOnce(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	assert.Equal(t, []int{1, 2, 3, 4, 5, 6, 7}, appliedVersions(t, db))

	// The migration is safe to run again after an interruption.
	require.NoError(t, migrateFindingSnapshots(context.Background(), db))
	var tables int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'finding_snapshots'`).Scan(&tables))
	assert.Equal(t, 1, tables)
}
