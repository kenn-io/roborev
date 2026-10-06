package prompt

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	return collapseRepeatedLines(s)
}

// collapseRepeatedLines folds runs of 3+ identical non-empty lines into the
// line plus a count marker. Oversized-diff fixtures are hundreds of repeated
// filler lines; the goldens pin where truncation cuts, not the filler itself.
func collapseRepeatedLines(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for i := 0; i < len(lines); {
		j := i
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		if run := j - i; run >= 3 && lines[i] != "" {
			out = append(out, lines[i],
				fmt.Sprintf("[golden: previous line repeated %d more times]", run-1))
		} else {
			out = append(out, lines[i:j]...)
		}
		i = j
	}
	return strings.Join(out, "\n")
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

func TestGoldenPrompt_SingleWithToolchain(t *testing.T) {
	r := newGoldenTestRepo(t)
	r.writeFile("go.mod", "module example.com/app\n\ngo 1.27.0\n")
	r.git("add", "go.mod")
	r.git("commit", "-m", "add go.mod")
	sha := r.commitFile("main.go", "package main\n\nfunc main() {}\n", "add main")

	b := NewBuilder(nil)
	prompt, err := b.ForRepo(r.dir, 0).Build(sha, 0, "test", "", "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "single_with_toolchain.golden")
}

// TestSinglePromptStatesGoToolchainVersion reproduces issue #1202: when the
// change touches Go files, the prompt must state the Go version declared by
// the repository's go.mod as an explicit fact, instead of relying on the
// reviewer choosing to consult the manifest and recalling recent features
// correctly.
func TestSinglePromptStatesGoToolchainVersion(t *testing.T) {
	r := newGoldenTestRepo(t)
	r.writeFile("go.mod", "module example.com/app\n\ngo 1.27.0\n")
	r.git("add", "go.mod")
	r.git("commit", "-m", "add go.mod")
	sha := r.commitFile("main.go", "package main\n\nfunc main() {}\n", "add main")

	prompt, err := NewBuilder(nil).ForRepo(r.dir, 0).Build(sha, 0, "test", "", "")
	require.NoError(t, err)

	assert.Contains(t, prompt, "This project uses Go 1.27.0")
}

// TestSinglePromptOmitsToolchainWithoutGoChanges locks the gating: a
// repository may declare a toolchain, but changes that touch no Go files
// carry no version-gated findings, so the fact stays out of the prompt.
func TestSinglePromptOmitsToolchainWithoutGoChanges(t *testing.T) {
	r := newGoldenTestRepo(t)
	r.writeFile("go.mod", "module example.com/app\n\ngo 1.27.0\n")
	r.git("add", "go.mod")
	r.git("commit", "-m", "add go.mod")
	sha := r.commitFile("notes.md", "hello world\n", "add notes")

	prompt, err := NewBuilder(nil).ForRepo(r.dir, 0).Build(sha, 0, "test", "", "")
	require.NoError(t, err)

	assert.NotContains(t, prompt, "This project uses Go")
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
		VerdictBool: testutil.ReviewFixtureVerdict("- Medium: foo.go:1 missing doc comment"),
		JobID:       42,
		Agent:       "test",
		Output:      "- Medium: foo.go:1 missing doc comment",
		Job:         &storage.ReviewJob{GitRef: sha},
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

// TestSinglePromptPreviousReviewsCarrySuppressionInstruction reproduces
// issue #1031: a single-commit review whose parent commit has stored
// findings — the roborev fix loop shape — receives those findings as
// context, but the previous_reviews section must also tell the reviewer
// to judge them against the current code, so a review of a fix commit
// neither re-raises the resolved finding nor asks to undo the fix,
// while still reporting genuine problems the fix itself introduced.
func TestSinglePromptPreviousReviewsCarrySuppressionInstruction(t *testing.T) {
	r := newGoldenTestRepo(t)
	parent := r.commitFile("handler.go",
		"func handle() error {\n\tif err := doWork(); err != nil {\n\t\t_ = err\n\t}\n\treturn nil\n}\n",
		"add handler")
	target := r.commitFile("handler.go",
		"func handle() error {\n\treturn doWork()\n}\n",
		"return the error from doWork")

	db := testutil.OpenTestDB(t)
	repo, err := db.GetOrCreateRepo(r.dir)
	require.NoError(t, err)

	testutil.CreateCompletedReview(t, db, repo.ID, parent, "test",
		"**Problem**: handle() swallows the error returned by doWork().\n\nVerdict: FAIL")

	b := NewBuilder(db)
	prompt, err := b.ForRepo(r.dir, repo.ID).Build(target, 1, "test", "", "")
	require.NoError(t, err)

	// The parent finding itself is context for the review of the fix commit.
	assert.Contains(t, prompt, "swallows the error returned by doWork()")

	// The reviewer must be told to re-raise a previous finding only when it
	// persists in the current code, and not to undo the fix that resolved it.
	assert.Contains(t, prompt, "unless it persists in the current code")
	assert.Contains(t, prompt, "do not ask to undo")
	// Suppression must not silence real problems in the fixed code.
	assert.Contains(t, prompt, "introduced by such a change")
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

func TestGoldenPrompt_AddressWithoutSeverity(t *testing.T) {
	r := newGoldenTestRepo(t)
	sha := r.commitFile("foo.go", "package foo\n", "add foo")

	b := NewBuilder(nil)
	review := &storage.Review{
		VerdictBool: testutil.ReviewFixtureVerdict("- Medium: foo.go:1 missing doc comment"),
		JobID:       99,
		Agent:       "test",
		Output:      "- Medium: foo.go:1 missing doc comment",
		Job:         &storage.ReviewJob{GitRef: sha},
	}
	responses := []storage.Response{
		{Responder: "roborev-fix", Response: "Added doc comment", CreatedAt: time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)},
	}

	prompt, err := b.ForRepo(r.dir, 0).BuildAddressPrompt(review, responses, "")
	require.NoError(t, err)

	assertGolden(t, scrubDynamic(prompt), "address_without_severity.golden")
}
