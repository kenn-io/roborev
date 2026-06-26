package prompt

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

var updateGolden = flag.Bool("update-golden", false, "regenerate golden files under internal/prompt/testdata/golden")

// goldenCommitDate is applied to every commit in golden-test repos so SHAs
// are stable across machines and re-runs.
const goldenCommitDate = "2026-04-01T12:00:00Z"

var (
	dateScrubber = regexp.MustCompile(`Current date: \d{4}-\d{2}-\d{2} \(UTC\)`)
	tsScrubber   = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}`)
)

// scrubDynamic normalizes values that vary across runs (the current calendar
// day the test happens to run, CreatedAt timestamps) so goldens only encode
// structural differences.
func scrubDynamic(s string) string {
	s = dateScrubber.ReplaceAllString(s, "Current date: GOLDEN_DATE (UTC)")
	s = tsScrubber.ReplaceAllString(s, "GOLDEN_TIMESTAMP")
	return s
}

// assertGolden compares got against testdata/golden/<name>, or rewrites the
// golden when -update-golden is passed.
func assertGolden(t *testing.T, got, name string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *updateGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s (run: go test -update-golden ./internal/prompt/)", path)
	assert.Equal(t, string(want), got, "golden %s drifted; review and re-run with -update-golden if intended", path)
}

// newGoldenTestRepo builds a test repo with fixed author/committer dates so
// commit SHAs are deterministic across runs.
func newGoldenTestRepo(t *testing.T) *testRepo {
	t.Helper()
	t.Setenv("GIT_AUTHOR_DATE", goldenCommitDate)
	t.Setenv("GIT_COMMITTER_DATE", goldenCommitDate)
	return newTestRepo(t)
}

// writeFile is a small helper for golden scenarios.
func (r *testRepo) writeFile(name, content string) {
	r.t.Helper()
	require.NoError(r.t, os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644))
}

// commitFile stages a single file and commits with the supplied message,
// returning the resulting SHA.
func (r *testRepo) commitFile(name, content, message string) string {
	r.t.Helper()
	r.writeFile(name, content)
	r.git("add", name)
	r.git("commit", "-m", message)
	return r.git("rev-parse", "HEAD")
}

func TestGoldenPrompt_SingleReviewDefault(t *testing.T) {
	r := newGoldenTestRepo(t)
	sha := r.commitFile("hello.txt", "hello world\n", "add greeting")

	b := NewBuilder(nil)
	prompt, err := b.ForRepo(r.dir, 0).Build(sha, 0, "test", "", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "single_review_default.golden")
}

func TestGoldenPrompt_SingleReviewCodex(t *testing.T) {
	r := newGoldenTestRepo(t)
	sha := r.commitFile("hello.txt", "hello world\n", "add greeting")

	b := NewBuilder(nil)
	prompt, err := b.ForRepo(r.dir, 0).Build(sha, 0, "codex", "", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "single_review_codex.golden")
}

func TestGoldenPrompt_RangeWithInRangeReviews(t *testing.T) {
	r := newGoldenTestRepo(t)
	baseSHA := r.commitFile("base.txt", "base\n", "initial")
	commit1 := r.commitFile("base.txt", "change1\n", "first feature commit")
	commit2 := r.commitFile("base.txt", "change2\n", "second feature commit")

	db := testutil.OpenTestDB(t)
	repo, err := db.GetOrCreateRepo(r.dir)
	require.NoError(t, err)

	testutil.CreateCompletedReview(t, db, repo.ID, commit1, "test",
		"Found bug: missing null check in handler\n\nVerdict: FAIL")
	testutil.CreateCompletedReview(t, db, repo.ID, commit2, "test",
		"No issues found.\n\nVerdict: PASS")

	b := NewBuilder(db)
	prompt, err := b.ForRepo(r.dir, repo.ID).Build(baseSHA+".."+commit2, 0, "test", "", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "range_with_in_range_reviews.golden")
}

func TestGoldenPrompt_DirtyReview(t *testing.T) {
	r := newGoldenTestRepo(t)
	r.commitFile("base.txt", "base\n", "initial")

	diff := "diff --git a/base.txt b/base.txt\n" +
		"index 0000000..1111111 100644\n" +
		"--- a/base.txt\n" +
		"+++ b/base.txt\n" +
		"@@ -1 +1,2 @@\n" +
		" base\n" +
		"+added line\n"

	b := NewBuilder(nil)
	prompt, err := b.ForRepo(r.dir, 0).BuildDirty(diff, 0, "test", "", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "dirty_review.golden")
}

func TestGoldenPrompt_AddressWithSplitResponses(t *testing.T) {
	r := newGoldenTestRepo(t)
	sha := r.commitFile("foo.go", "package foo\n", "add foo")

	b := NewBuilder(nil)
	review := &storage.Review{
		JobID:  42,
		Agent:  "test",
		Output: "- Medium: foo.go:1 missing doc comment",
		Job:    &storage.ReviewJob{GitRef: sha},
	}
	responses := []storage.Response{
		{Responder: "roborev-fix", Response: "Added doc comment", CreatedAt: time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)},
		{Responder: "alice", Response: "Doc comments are optional here", CreatedAt: time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)},
	}

	prompt, err := b.ForRepo(r.dir, 0).BuildAddressPrompt(review, responses, "medium")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "address_with_split_responses.golden")
}

func TestGoldenPrompt_SecurityReview(t *testing.T) {
	r := newGoldenTestRepo(t)
	sha := r.commitFile("hello.txt", "hello world\n", "add greeting")

	b := NewBuilder(nil)
	prompt, err := b.ForRepo(r.dir, 0).Build(sha, 0, "test", "security", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "security_review.golden")
}

func TestGoldenPrompt_DesignReview(t *testing.T) {
	r := newGoldenTestRepo(t)
	sha := r.commitFile("hello.txt", "hello world\n", "add greeting")

	b := NewBuilder(nil)
	prompt, err := b.ForRepo(r.dir, 0).Build(sha, 0, "test", "design", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "design_review.golden")
}

func TestGoldenPrompt_LookaheadReview(t *testing.T) {
	r := newGoldenTestRepo(t)
	sha := r.commitFile("hello.txt", "hello world\n", "add greeting")

	b := NewBuilder(nil)
	prompt, err := b.ForRepo(r.dir, 0).Build(sha, 0, "test", "lookahead", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "lookahead_review.golden")
}

func TestGoldenPrompt_SingleReviewClaudeCode(t *testing.T) {
	r := newGoldenTestRepo(t)
	sha := r.commitFile("hello.txt", "hello world\n", "add greeting")

	b := NewBuilder(nil)
	prompt, err := b.ForRepo(r.dir, 0).Build(sha, 0, "claude-code", "", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "single_review_claude_code.golden")
}

func TestGoldenPrompt_SingleReviewGemini(t *testing.T) {
	r := newGoldenTestRepo(t)
	sha := r.commitFile("hello.txt", "hello world\n", "add greeting")

	b := NewBuilder(nil)
	prompt, err := b.ForRepo(r.dir, 0).Build(sha, 0, "gemini", "", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "single_review_gemini.golden")
}

func TestGoldenPrompt_SingleWithPreviousReviews(t *testing.T) {
	r := newGoldenTestRepo(t)
	parent1 := r.commitFile("a.txt", "a1\n", "alpha 1")
	parent2 := r.commitFile("a.txt", "a2\n", "alpha 2")
	target := r.commitFile("a.txt", "a3\n", "alpha 3")

	db := testutil.OpenTestDB(t)
	repo, err := db.GetOrCreateRepo(r.dir)
	require.NoError(t, err)

	testutil.CreateCompletedReview(t, db, repo.ID, parent1, "test",
		"No issues found.\n\nVerdict: PASS")
	testutil.CreateCompletedReview(t, db, repo.ID, parent2, "test",
		"Found unused variable in a.txt\n\nVerdict: FAIL")

	b := NewBuilder(db)
	prompt, err := b.ForRepo(r.dir, repo.ID).Build(target, 2, "test", "", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "single_with_previous_reviews.golden")
}

// TestGoldenPrompt_PreviousReviewsWithComments exercises the review_comments
// rendering path. Prior versions trimmed the separator after a comment block,
// causing the next `--- Review ... ---` header to butt against the last
// comment line. This snapshot locks in the expected blank line between items
// when any entry has comments.
func TestGoldenPrompt_PreviousReviewsWithComments(t *testing.T) {
	r := newGoldenTestRepo(t)
	parent1 := r.commitFile("a.txt", "a1\n", "alpha 1")
	parent2 := r.commitFile("a.txt", "a2\n", "alpha 2")
	target := r.commitFile("a.txt", "a3\n", "alpha 3")

	db := testutil.OpenTestDB(t)
	repo, err := db.GetOrCreateRepo(r.dir)
	require.NoError(t, err)

	testutil.CreateReviewWithComments(t, db, repo.ID, parent1,
		"Found unused variable in a.txt\n\nVerdict: FAIL",
		[]testutil.ReviewComment{
			{User: "alice", Text: "False positive; we use this field via reflection."},
			{User: "bob", Text: "Agree with alice."},
		})
	testutil.CreateCompletedReview(t, db, repo.ID, parent2, "test",
		"No issues found.\n\nVerdict: PASS")

	b := NewBuilder(db)
	prompt, err := b.ForRepo(r.dir, repo.ID).Build(target, 2, "test", "", "")
	require.NoError(t, err)

	// The separator between comment-bearing entries and the next entry
	// must still contain a blank line; otherwise the `--- Review ... ---`
	// header butts against the last comment line.
	assert.Contains(t, prompt, "Comments on this review:\n- alice: ")
	assert.Contains(t, prompt, `"Agree with alice."`+"\n\n--- Review for commit ")

	assertGolden(t, scrubDynamic(prompt), "previous_reviews_with_comments.golden")
}

func TestGoldenPrompt_SingleWithGuidelines(t *testing.T) {
	r := newGoldenTestRepo(t)
	guidelines := `review_guidelines = """
- Always prefer table-driven tests for Go.
- No new dependencies without justification.
"""
`
	r.writeFile(".roborev.toml", guidelines)
	r.git("add", ".roborev.toml")
	r.git("commit", "-m", "add review guidelines")
	sha := r.commitFile("hello.txt", "hello world\n", "add greeting")

	b := NewBuilder(nil)
	prompt, err := b.ForRepo(r.dir, 0).Build(sha, 0, "test", "", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "single_with_guidelines.golden")
}

func TestGoldenPrompt_SingleWithAdditionalContext(t *testing.T) {
	r := newGoldenTestRepo(t)
	sha := r.commitFile("hello.txt", "hello world\n", "add greeting")

	additional := "## Pull Request Discussion\n\nReviewer noted the greeting should support i18n in a later PR.\n"
	b := NewBuilder(nil)
	prompt, err := b.ForRepo(r.dir, 0).BuildWithAdditionalContext(sha, 0, "test", "", "", additional)
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "single_with_additional_context.golden")
}

func TestGoldenPrompt_SingleWithSeverityFilter(t *testing.T) {
	r := newGoldenTestRepo(t)
	sha := r.commitFile("hello.txt", "hello world\n", "add greeting")

	b := NewBuilder(nil)
	prompt, err := b.ForRepo(r.dir, 0).Build(sha, 0, "test", "", "medium")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "single_with_severity_filter.golden")
}

func TestGoldenPrompt_SingleTruncatedDiff(t *testing.T) {
	r := newGoldenTestRepo(t)
	r.commitFile("base.txt", "base\n", "initial")

	// A large change that exceeds the prompt cap and triggers the commit
	// fallback rendering. The cap is kept above the system-prompt size so the
	// truncated prompt still carries commit metadata and a diff view command,
	// rather than truncating the system prompt itself.
	large := strings.Repeat("line of content added\n", 800)
	sha := r.commitFile("big.txt", large, "huge change")

	cfg := &config.Config{DefaultMaxPromptSize: 5500}
	b := NewBuilderWithConfig(nil, cfg)
	prompt, err := b.ForRepo(r.dir, 0).Build(sha, 0, "test", "", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "single_truncated_diff.golden")
}

func TestGoldenPrompt_SingleTruncatedDiffCodex(t *testing.T) {
	r := newGoldenTestRepo(t)
	r.commitFile("base.txt", "base\n", "initial")

	large := strings.Repeat("line of content added\n", 800)
	sha := r.commitFile("big.txt", large, "huge change")

	cfg := &config.Config{DefaultMaxPromptSize: 4000}
	b := NewBuilderWithConfig(nil, cfg)
	prompt, err := b.ForRepo(r.dir, 0).Build(sha, 0, "codex", "", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "single_truncated_diff_codex.golden")
}

func TestGoldenPrompt_RangeTruncated(t *testing.T) {
	r := newGoldenTestRepo(t)
	baseSHA := r.commitFile("base.txt", "base\n", "initial")

	large := strings.Repeat("a content line\n", 500)
	r.commitFile("big1.txt", large, "first large addition")
	headSHA := r.commitFile("big2.txt", large, "second large addition")

	cfg := &config.Config{DefaultMaxPromptSize: 5000}
	b := NewBuilderWithConfig(nil, cfg)
	prompt, err := b.ForRepo(r.dir, 0).Build(baseSHA+".."+headSHA, 0, "test", "", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "range_truncated.golden")
}

func TestGoldenPrompt_DirtyTruncated(t *testing.T) {
	r := newGoldenTestRepo(t)
	r.commitFile("base.txt", "base\n", "initial")

	diff := "diff --git a/big.txt b/big.txt\nnew file mode 100644\n" +
		"index 0000000..1111111\n--- /dev/null\n+++ b/big.txt\n@@ -0,0 +1,500 @@\n" +
		strings.Repeat("+a line of content\n", 500)

	cfg := &config.Config{DefaultMaxPromptSize: 4000}
	b := NewBuilderWithConfig(nil, cfg)
	prompt, err := b.ForRepo(r.dir, 0).BuildDirty(diff, 0, "test", "", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "dirty_truncated.golden")
}

func TestGoldenPrompt_RangeTruncatedCodexPreservesInRangeReviews(t *testing.T) {
	r := newGoldenTestRepo(t)
	baseSHA := r.commitFile("base.txt", "base\n", "initial")

	large := strings.Repeat("a content line\n", 500)
	commit1 := r.commitFile("big1.txt", large, "first large addition")
	commit2 := r.commitFile("big2.txt", large, "second large addition")

	db := testutil.OpenTestDB(t)
	repo, err := db.GetOrCreateRepo(r.dir)
	require.NoError(t, err)

	testutil.CreateCompletedReview(t, db, repo.ID, commit1, "test",
		"Found null-deref in big1.txt\n\nVerdict: FAIL")
	testutil.CreateCompletedReview(t, db, repo.ID, commit2, "test",
		"No issues found.\n\nVerdict: PASS")

	cfg := &config.Config{DefaultMaxPromptSize: 6000}
	b := NewBuilderWithConfig(db, cfg)
	prompt, err := b.ForRepo(r.dir, repo.ID).Build(baseSHA+".."+commit2, 0, "codex", "", "")
	require.NoError(t, err)

	// Narrow invariant checks first — these are the regression we just
	// fixed: the truncated codex range path must not drop InRangeReviews,
	// and it must still select the codex-specific inspection fallback.
	assert.Contains(t, prompt, "Per-Commit Reviews in This Range")
	assert.Contains(t, prompt, "Found null-deref in big1.txt")
	assert.Contains(t, prompt, "For Codex in read-only review mode, inspect the commit range locally")

	assertGolden(t, scrubDynamic(prompt), "range_truncated_codex_in_range.golden")
}

func TestGoldenPrompt_AddressWithoutSeverity(t *testing.T) {
	r := newGoldenTestRepo(t)
	sha := r.commitFile("foo.go", "package foo\n", "add foo")

	b := NewBuilder(nil)
	review := &storage.Review{
		JobID:  99,
		Agent:  "test",
		Output: "- Medium: foo.go:1 missing doc comment",
		Job:    &storage.ReviewJob{GitRef: sha},
	}
	responses := []storage.Response{
		{Responder: "roborev-fix", Response: "Added doc comment", CreatedAt: time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)},
	}

	prompt, err := b.ForRepo(r.dir, 0).BuildAddressPrompt(review, responses, "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "address_without_severity.golden")
}
