package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

func stallDocument(locations ...string) map[string]any {
	findings := make([]any, 0, len(locations))
	for _, location := range locations {
		findings = append(findings, map[string]any{"severity": "low", "problem": "Ordering is still wrong", "fix": "Preserve cancellation ordering", "location": location})
	}
	return map[string]any{"schema_version": 2, "summary": "Cancellation requires another change", "verdict": "fail", "findings": findings}
}

func TestRefineStallLoop(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		severityOnly, bothSignals bool
		otherFailed               bool
		pendingFixMode            string
		hookMode                  string
		rounds                    int
		stop, commentFailure      bool
		recovery                  string
		retry                     bool
		reviewFailure             bool
		recoveredReset            string
		resetJob                  int64
		wantEnqueues              int
		wantCalls, wantSignals    int
		wantError                 string
	}{
		{name: "severity only final recovered completed", severityOnly: true, rounds: 3, stop: true, recovery: "completed", wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "severity only final recovered pending", severityOnly: true, rounds: 3, stop: true, recovery: "pending", wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "severity only final recovered branch", severityOnly: true, rounds: 3, stop: true, recovery: "branch", wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "deferred third review before other failed commit", severityOnly: true, otherFailed: true, rounds: 3, stop: true, wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "severity only continues", severityOnly: true, rounds: 4, wantCalls: 4, wantSignals: 1, wantError: "max iterations"},
		{name: "both signals one advisory", bothSignals: true, rounds: 3, stop: true, wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "severity review error resets", severityOnly: true, rounds: 4, reviewFailure: true, wantCalls: 4, wantError: "max iterations"},
		{name: "severity pending review error resets", severityOnly: true, rounds: 4, reviewFailure: true, recovery: "pending", wantCalls: 4, wantError: "max iterations"},
		{name: "recovered passing review resets before branch fallback", rounds: 4, stop: true, recovery: "completed", recoveredReset: "pass", wantEnqueues: 1, wantCalls: 4, wantError: "max iterations"},
		{name: "recovered closed review resets before branch fallback", rounds: 4, stop: true, recovery: "completed", recoveredReset: "closed", wantEnqueues: 1, wantCalls: 4, wantError: "max iterations"},
		{name: "final recovered completed review stops", rounds: 3, stop: true, recovery: "completed", wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "final recovered completed review advises", rounds: 3, recovery: "completed", wantCalls: 3, wantSignals: 1, wantError: "max iterations"},
		{name: "final recovered pending review stops", rounds: 3, stop: true, recovery: "pending", wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "final recovered pending review advises", rounds: 3, recovery: "pending", wantCalls: 3, wantSignals: 1, wantError: "max iterations"},
		{name: "final recovered branch review stops", rounds: 3, stop: true, recovery: "branch", wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "final recovered branch review advises", rounds: 3, recovery: "branch", wantCalls: 3, wantSignals: 1, wantError: "max iterations"},
		{name: "final recovered completed branch review stops", rounds: 3, stop: true, recovery: "completed branch", wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "final recovered completed branch review advises", rounds: 3, recovery: "completed branch", wantCalls: 3, wantSignals: 1, wantError: "max iterations"},
		{name: "recovered closed passing branch review completes", rounds: 3, stop: true, recovery: "completed branch", recoveredReset: "closed pass", resetJob: 10, wantCalls: 3},
		{name: "pending fix review precedes other failed commit", severityOnly: true, otherFailed: true, pendingFixMode: "pending", rounds: 3, stop: true, wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "pending fix review completes between lookups", severityOnly: true, pendingFixMode: "race", rounds: 3, stop: true, wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "pending branch review precedes other failed commit", severityOnly: true, otherFailed: true, pendingFixMode: "branch", rounds: 3, stop: true, wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "pending synthesis review precedes other failed commit", severityOnly: true, otherFailed: true, pendingFixMode: "synthesis", rounds: 3, stop: true, wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "final recovered passing branch review resets", rounds: 3, stop: true, recovery: "completed branch", recoveredReset: "pass", resetJob: 10, wantCalls: 3},
		{name: "recovered closed branch review resets before fallback", rounds: 4, stop: true, recovery: "completed branch", recoveredReset: "closed", wantEnqueues: 1, wantCalls: 4, wantError: "max iterations"},
		{name: "final branch hook range passes with distinct since base", hookMode: "branch", rounds: 3, stop: true, recovery: "branch", recoveredReset: "pass", resetJob: 10, wantCalls: 3},
		{name: "final branch hook range stalls with distinct since base", hookMode: "branch", severityOnly: true, rounds: 3, stop: true, recovery: "branch", wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "final batch hook range passes with distinct since base", hookMode: "batch", rounds: 3, stop: true, recovery: "branch", recoveredReset: "pass", resetJob: 10, wantCalls: 3},
		{name: "final batch hook range stalls with distinct since base", hookMode: "batch", severityOnly: true, rounds: 3, stop: true, recovery: "branch", wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "failed re-review resets", rounds: 4, reviewFailure: true, wantCalls: 4, wantError: "max iterations"},
		{name: "provider retries do not count", rounds: 3, retry: true, wantCalls: 3, wantError: "max iterations"},
		{name: "final iteration advises", rounds: 3, wantCalls: 3, wantSignals: 1, wantError: "max iterations"},
		{name: "default continues once", rounds: 4, wantCalls: 4, wantSignals: 1, wantError: "max iterations"},
		{name: "stop before next fix", rounds: 5, stop: true, wantCalls: 3, wantSignals: 1, wantError: "stalled"},
		{name: "comment failure continues", rounds: 4, commentFailure: true, wantCalls: 4, wantSignals: 1, wantError: "max iterations"},
		{name: "comment failure still stops", rounds: 5, stop: true, commentFailure: true, wantCalls: 3, wantSignals: 1, wantError: "stalled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
			base := repo.HeadSHA()
			hookBase := base
			if tc.hookMode != "" {
				repo.Run("checkout", "-b", "feature")
				config := map[string]string{
					"branch": "post_commit_review = \"branch\"\n",
					"batch":  "post_commit_batch_size = 2\n",
				}[tc.hookMode]
				writeRoborevConfig(t, repo, config)
				repo.Run("add", ".roborev.toml")
				base = repo.CommitFile("since.txt", "package source\n", "Advance refine since base")
			}
			initial := repo.CommitFile("next.go", "package source\n", "Add next source")
			var otherRef string
			if tc.otherFailed {
				otherRef = repo.CommitFile("other.go", "package source\n", "Add other source")
			}
			d := newRefineHistoryDaemon(t, initial)
			resetJob := tc.resetJob
			if resetJob == 0 {
				resetJob = 9
			}
			d.onReview = func(r *storage.Review) {
				r.StructuredOutput = stallDocument("next.go:1-2", "source.go", "")
				if tc.severityOnly || tc.bothSignals {
					for _, item := range r.StructuredOutput["findings"].([]any) {
						finding := item.(map[string]any)
						finding["severity"] = "medium"
						if tc.severityOnly {
							finding["location"] = ""
						}
					}
				}
				if r.JobID == resetJob {
					switch tc.recoveredReset {
					case "pass":
						r.VerdictBool = new(1)
						r.StructuredOutput = stallDocument()
						r.StructuredOutput["verdict"] = "pass"
					case "closed pass":
						r.VerdictBool = new(1)
						r.StructuredOutput = stallDocument()
						r.StructuredOutput["verdict"] = "pass"
						r.Closed = true
					case "closed":
						r.Closed = true
					}
				}
			}
			d.onReview(d.reviews[7])
			if tc.otherFailed {
				d.addReview(100, otherRef, "Medium: other finding")
			}
			d.rejectAdvisory = tc.commentFailure
			d.recovery = tc.recovery
			if tc.recovery == "completed branch" {
				d.recovery = "branch"
				d.completedBranchReview = 10
				if tc.recoveredReset == "closed" {
					d.completedBranchReview = resetJob
				}
			}
			d.rangeBase = base
			if tc.hookMode != "" {
				d.rangeBase = hookBase
				d.branchName = repo.Run("branch", "--show-current")
			}
			d.allowBranchReview = tc.recoveredReset != ""
			if tc.reviewFailure {
				d.failReview = 9
			}
			calls := 0
			fake := &agent.FakeAgent{NameStr: "refine-stall-test", ReviewFn: func(_ context.Context, path, _, _ string, _ io.Writer) (string, error) {
				calls++
				if tc.otherFailed && calls == 3 {
					d.recovery = "completed"
				}
				if tc.pendingFixMode != "" && calls == 3 {
					d.recovery = "pending"
					if tc.pendingFixMode == "branch" {
						d.recovery = "branch"
					}
					if tc.pendingFixMode == "synthesis" {
						d.pendingFixJobType = storage.JobTypeSynthesis
					}
					d.completePendingFixReviewOnLookup = tc.pendingFixMode == "race"
				}
				if tc.retry && calls == 2 {
					return "", fmt.Errorf("provider unavailable")
				}
				require.NoError(t, os.WriteFile(filepath.Join(path, "next.go"), fmt.Appendf(nil, "package source\n// fix %d\n", calls), 0o600))
				return "Changed cancellation ordering", nil
			}}
			agent.Register(&refineModeGuardAgent{t: t, Agent: fake})
			t.Cleanup(func() { agent.Unregister(fake.Name()) })
			unsafe := agent.AllowUnsafeAgents()
			t.Cleanup(func() { agent.SetAllowUnsafeAgents(unsafe) })
			opts := refineOptions{since: base, agentName: fake.Name(), maxIterations: tc.rounds, quiet: true, unsafeFlagChanged: true}
			run := func() error {
				return runRefine(RunContext{Context: t.Context(), WorkingDir: repo.Dir, PostCommitDelay: time.Millisecond}, opts)
			}
			if tc.stop {
				chdir(t, repo.Dir)
				originalDelay := postCommitWaitDelay
				postCommitWaitDelay = time.Millisecond
				t.Cleanup(func() { postCommitWaitDelay = originalDelay })
				cmd := refineCmd()
				cmd.SetContext(t.Context())
				cmd.SetArgs([]string{"--stop-on-stall", "--since", base, "--agent", fake.Name(), "--max-iterations", strconv.Itoa(tc.rounds), "--quiet", "--allow-unsafe-agents=false"})
				run = cmd.Execute
			}
			var runErr error
			output := captureOutput(t, func() error {
				runErr = run()
				return nil
			})
			if tc.wantError == "" {
				require.NoError(t, runErr)
			} else {
				require.ErrorContains(t, runErr, tc.wantError,
					"captured output:\n%s\npost-commit range queries=%d matches=%d branch=%q",
					output, d.postCommitRangeQueries, d.postCommitRangeMatches, d.branchName,
				)
			}
			a := assert.New(t)
			a.Equal(tc.wantCalls, calls)
			if tc.otherFailed {
				a.Equal("4", repo.Run("rev-list", "--count", initial+"..HEAD"), "one other commit and exactly three fixes")
			}
			if tc.rounds == 3 && (tc.recovery != "" || tc.pendingFixMode != "" || tc.hookMode != "") {
				wantCommits := "3"
				if tc.otherFailed {
					wantCommits = "4"
				}
				a.Equal(wantCommits, repo.Run("rev-list", "--count", initial+"..HEAD"), "recovery cannot create a fourth fix commit")
			}
			a.Equal(tc.wantEnqueues, d.enqueues, "stopping must not enqueue a whole-branch fallback")
			if tc.hookMode != "" {
				a.NotEmpty(d.branchName)
				a.Positive(d.postCommitRangeQueries, "configured post-commit range recovery must query jobs")
				a.Positive(d.postCommitRangeMatches, "the final post-commit range job must be returned")
			}
			if tc.pendingFixMode != "" {
				a.Positive(d.jobReviewFetches[d.pendingFixReviewJobID], "the pending fix review must be recovered by job ID")
			}
			if tc.pendingFixMode == "race" {
				a.True(d.completedPendingFixReviewOnLookup, "the review must complete between the SHA and job lookups")
			}
			a.Equal(tc.wantSignals, strings.Count(output, "Refine advisory:"))
			var advisories []storage.Response
			for _, responses := range d.posted {
				for _, response := range responses {
					if strings.Contains(response.Response, "Refine advisory:") {
						advisories = append(advisories, response)
					}
				}
			}
			if tc.wantSignals > 0 {
				if !tc.severityOnly {
					a.Contains(output, "next.go, source.go")
				}
				if tc.severityOnly || tc.bothSignals {
					a.Contains(output, "High plus Medium")
				}
				a.Contains(output, "Change the plan, or split the change into smaller reviewable pieces.")
				if tc.stop || tc.rounds == 3 {
					a.False(d.reviews[10].Closed, "the newly failed review stays open at the signal")
				}
				if tc.commentFailure {
					a.Empty(advisories)
					a.Contains(output, "Warning: failed to save refine advisory")
				} else {
					require.Len(t, advisories, 1)
					a.Equal("roborev-refine", advisories[0].Responder)
					a.Contains(d.posted[10], advisories[0], "advisory belongs to the third fix review")
				}
			} else {
				a.Empty(advisories)
			}
			a.Empty(repo.Run("status", "--porcelain"))
		})
	}
}

func TestRefineFindingPath(t *testing.T) {
	tracked := map[string]struct{}{"main.go": {}, "dir/with spaces.go": {}, "Root": {}, "δοκιμή/文件.go": {}}
	for _, tc := range []struct{ name, input, want string }{
		{"plain", "main.go", "main.go"},
		{"line", "main.go:12", "main.go"},
		{"range", "main.go:2-12", "main.go"},
		{"relative range", "./main.go:1-2", "main.go"},
		{"leading zero", "main.go:0002-003", "main.go"},
		{"large line", "main.go:999999999999999999999999999999999999", "main.go"},
		{"outer whitespace and backticks", " `./dir/with spaces.go:1-2` ", "dir/with spaces.go"},
		{"root file", "Root", "Root"},
		{"unicode", "δοκιμή/文件.go:1", "δοκιμή/文件.go"},
		{"traversal", "../main.go", ""},
		{"internal traversal", "dir/../main.go:1", ""},
		{"absolute", "/main.go", ""},
		{"drive", "C:/main.go:1", ""},
		{"UNC", `\\server\main.go`, ""},
		{"url", "https://example.test/main.go:1", ""},
		{"prose", "see main.go:1", ""},
		{"multiple", "main.go:1, Root:2", ""},
		{"control", "main.go\n:1", ""},
		{"zero", "main.go:0", ""},
		{"reverse", "main.go:3-2", ""},
		{"empty end", "main.go:1-", ""},
		{"column", "main.go:1:2", ""},
		{"negative", "main.go:-1", ""},
		{"unmatched tick", "`main.go:1", ""},
		{"case", "Main.go:1", ""},
		{"unknown", "deleted.go:1", ""},
		{"dot component", "dir/./main.go", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := refineFindingPath(tc.input, tracked)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want != "", ok)
		})
	}
}

func TestRefineStallTracker(t *testing.T) {
	a := map[string]struct{}{"a.go": {}, "b.go": {}}
	b := map[string]struct{}{"b.go": {}}
	c := map[string]struct{}{"c.go": {}}
	for _, tc := range []struct {
		name    string
		sets    []map[string]struct{}
		signals []bool
	}{
		{"three transitions", []map[string]struct{}{a, a, a, a, a}, []bool{false, false, false, true, false}},
		{"shrink counts", []map[string]struct{}{a, a, b, b}, []bool{false, false, false, true}},
		{"change resets", []map[string]struct{}{a, a, a, c, c, c, c}, []bool{false, false, false, false, false, false, true}},
		{"alternating singletons reset", []map[string]struct{}{b, c, b, c, b, c}, []bool{false, false, false, false, false, false}},
		{"unknown resets", []map[string]struct{}{a, a, a, nil, a, a, a, a}, []bool{false, false, false, false, false, false, false, true}},
		{"empty resets", []map[string]struct{}{a, a, a, {}, a, a, a, a}, []bool{false, false, false, false, false, false, false, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tracker refineStallTracker
			for i, files := range tc.sets {
				assert.Equal(t, tc.signals[i], tracker.observe(files), "observation %d", i)
			}
			tracker.reset()
			assert.False(t, tracker.observe(a))
		})
	}
}

func TestRefineFindingFiles(t *testing.T) {
	repo := newPlanTestRepo(t, map[string]string{"keep.go": "package source\n", "gone.go": "package source\n", "medium.go": "package source\n", "low.go": "package source\n"})
	before := repo.HeadSHA()
	allSeverities := stallDocument("keep.go:2", "gone.go", "medium.go:1", "low.go:1", "")
	for i, severity := range []string{"critical", "high", "medium", "low"} {
		allSeverities["findings"].([]any)[i].(map[string]any)["severity"] = severity
	}
	repo.Run("rm", "gone.go")
	after := repo.CommitFile("later.go", "package source\n", "Replace source")
	for _, tc := range []struct {
		name, ref string
		document  map[string]any
		want      map[string]struct{}
	}{
		{name: "all severities and located subset", ref: before, document: allSeverities, want: map[string]struct{}{"keep.go": {}, "gone.go": {}, "medium.go": {}, "low.go": {}}},
		{name: "range endpoint excludes deleted", ref: before + ".." + after, document: stallDocument("gone.go", "later.go:1"), want: map[string]struct{}{"later.go": {}}},
		{name: "three dot endpoint", ref: before + "..." + after, document: stallDocument("gone.go", "later.go:1"), want: map[string]struct{}{"later.go": {}}},
		{name: "unresolved ref", ref: "missing-ref", document: stallDocument("keep.go")},
		{name: "legacy markdown ignored", ref: before},
		{name: "no locations", ref: before, document: stallDocument("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &storage.Review{Output: "High: keep.go:1 is wrong", StructuredOutput: tc.document, Job: &storage.ReviewJob{GitRef: tc.ref}}
			assert.Equal(t, tc.want, refineFindingFiles(t.Context(), repo.Dir, r))
		})
	}
}

func FuzzRefineFindingPath(f *testing.F) {
	for _, seed := range []string{"main.go", "./main.go:1-2", "../main.go", "see main.go:2", "https://example.test/main.go:1", "δοκιμή/文件.go:1", "Root", "main.go:3-2"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		filename := fmt.Sprintf("generated-%x.go", sha256.Sum256([]byte(input)))
		tracked := map[string]struct{}{"main.go": {}, "Root": {}, "δοκιμή/文件.go": {}, "dir/with spaces.go": {}, filename: {}}
		location := fmt.Sprintf(" `./%s:%d-%d` ", filename, len(input)+1, len(input)+2)
		got, ok := refineFindingPath(location, tracked)
		assert.True(t, ok, "a constructed tracked location must be accepted")
		assert.Equal(t, filename, got)
		got, ok = refineFindingPath(input, tracked)
		if ok {
			assert.Contains(t, tracked, got)
			again, valid := refineFindingPath(got, tracked)
			assert.True(t, valid)
			assert.Equal(t, got, again)
		} else {
			assert.Empty(t, got)
		}
		assert.Equal(t, map[string]struct{}{"main.go": {}, "Root": {}, "δοκιμή/文件.go": {}, "dir/with spaces.go": {}, filename: {}}, tracked)
	})
}

func FuzzRefineStallTracker(f *testing.F) {
	for _, seed := range [][]byte{{7, 7, 3, 1, 1}, {1, 1, 3, 3, 3, 3, 3}, {7, 7, 0, 7, 7, 7, 7}, {1, 2, 1, 2, 1, 2}} {
		f.Add(string(seed))
	}
	f.Fuzz(func(t *testing.T, input string) {
		var tracker refineStallTracker
		previous := byte(0)
		streakStart := 0
		for i, step := range []byte(input) {
			mask := step & 7
			var files map[string]struct{}
			if mask != 0 {
				files = make(map[string]struct{})
				for bit, name := range []string{"a.go", "b.go", "c.go"} {
					if mask&(1<<bit) != 0 {
						files[name] = struct{}{}
					}
				}
			}
			// A new bit or an empty endpoint starts a new chain. Count its
			// length from the start index, independently of the map tracker.
			if mask == 0 || previous == 0 || mask&previous != mask {
				streakStart = i
			}
			previous = mask
			assert.Equal(t, mask != 0 && i-streakStart == 3, tracker.observe(files), "observation %d, mask %03b", i, mask)
		}
		tracker.reset()
		assert.False(t, tracker.observe(map[string]struct{}{"a": {}}))
	})
}

func TestRefineStallReviewLifecycle(t *testing.T) {
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	initial := repo.HeadSHA()
	refs := []string{initial}
	for i := 1; i <= 4; i++ {
		refs = append(refs, repo.CommitFile("source.go", fmt.Sprintf("package source\n// change %d\n", i), "Change source"))
	}
	review := func(id int, ref string) *storage.Review {
		return &storage.Review{ID: int64(id), JobID: int64(id), Job: &storage.ReviewJob{GitRef: ref, Status: storage.JobStatusDone}, VerdictBool: new(0), StructuredOutput: stallDocument("source.go:1")}
	}
	for _, mode := range []string{"commit", "range", "three dot range", "unrelated", "unlocated", "unable", "closed", "pass", "new chain", "pending"} {
		t.Run(mode, func(t *testing.T) {
			var tracker refineStallTracker
			assert.False(t, tracker.observeReview(t.Context(), repo.Dir, review(1, initial)))
			for round := 1; round <= 3; round++ {
				tracker.pendingFixSHA = refs[round]
				ref := refs[round]
				switch mode {
				case "range":
					ref = initial + ".." + ref
				case "three dot range":
					ref = initial + "..." + ref
				}
				r := review(round+1, ref)
				if round == 2 {
					switch mode {
					case "unrelated":
						r.Job.GitRef = initial
					case "unlocated":
						r.StructuredOutput = stallDocument("")
					case "unable":
						r.StructuredOutput["verdict"] = "unable_to_review"
					case "closed":
						r.Closed = true
					case "pass":
						r.VerdictBool = new(1)
					case "pending":
						r.Job.Status = storage.JobStatusRunning
					case "new chain":
						tracker.pendingFixSHA = ""
					}
				}
				want := round == 3 && (mode == "commit" || mode == "range" || mode == "three dot range")
				assert.Equal(t, want, tracker.observeReview(t.Context(), repo.Dir, r), "round %d", round)
				assert.Empty(t, tracker.pendingFixSHA)
				assert.False(t, tracker.observeReview(t.Context(), repo.Dir, r), "retries/reselection cannot count a fix twice")
			}
		})
	}
}

func TestRefineStopOnStallListNoOp(t *testing.T) {
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	chdir(t, repo.Dir)
	_ = newMockDaemonBuilder(t).WithJobs([]storage.ReviewJob{}).Build()
	cmd := refineCmd()
	cmd.SetContext(t.Context())
	cmd.SetArgs([]string{"--list", "--stop-on-stall"})
	var output strings.Builder
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, output.String(), "No failed reviews")
	assert.Empty(t, repo.Run("status", "--porcelain"))
}

func TestRefineSeverityStall(t *testing.T) {
	repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
	refs := []string{repo.HeadSHA()}
	for i := 1; i <= 7; i++ {
		refs = append(refs, repo.CommitFile("source.go", fmt.Sprintf("package source\n// %d\n", i), "Change source"))
	}
	for _, tc := range []struct {
		name         string
		counts       []int
		want         []int
		nullLocation bool
		locatedFrom  int
		invalidAt    int
	}{
		{name: "unlocated flat", counts: []int{2, 2, 2, 2, 2}, want: []int{3}},
		{name: "unlocated increasing", counts: []int{1, 2, 3, 4}, want: []int{3}},
		{name: "null locations", counts: []int{1, 1, 1, 1}, want: []int{3}, nullLocation: true},
		{name: "decline resets only severity", counts: []int{3, 3, 2, 2, 2, 2}, want: []int{5}},
		{name: "decline preserves file streak", counts: []int{3, 3, 2, 2}, locatedFrom: 1, want: []int{3}},
		{name: "zero restarts severity baseline", counts: []int{2, 0, 2, 2, 2, 2}, want: []int{5}},
		{name: "zero does not advance", counts: []int{0, 0, 0, 0, 0}},
		{name: "invalid clears baseline", counts: []int{1, 1, 1, 1, 1, 1, 1}, invalidAt: 2, want: []int{6}},
		{name: "simultaneous signals", counts: []int{1, 1, 1, 1, 1}, locatedFrom: 1, want: []int{3}},
		{name: "later independent file signal", counts: []int{1, 1, 1, 1, 1, 1}, locatedFrom: 3, want: []int{3, 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tracker refineStallTracker
			var got []int
			for round, count := range tc.counts {
				locations := make([]string, count)
				if tc.locatedFrom > 0 && round >= tc.locatedFrom-1 {
					for i := range locations {
						locations[i] = "source.go:1"
					}
				}
				document := stallDocument(locations...)
				for i, item := range document["findings"].([]any) {
					finding := item.(map[string]any)
					finding["severity"] = []string{"high", "medium"}[i%2]
					if tc.nullLocation {
						finding["location"] = nil
					}
				}
				if round == tc.invalidAt && tc.invalidAt > 0 {
					document = nil
				}
				review := &storage.Review{ID: int64(round + 1), JobID: int64(round + 1), Job: &storage.ReviewJob{GitRef: refs[round], Status: storage.JobStatusDone}, VerdictBool: new(0), StructuredOutput: document}
				if round > 0 {
					tracker.pendingFixSHA = refs[round]
				}
				if tracker.observeReview(t.Context(), repo.Dir, review) {
					got = append(got, round)
				}
				assert.False(t, tracker.observeReview(t.Context(), repo.Dir, review), "duplicate round %d", round)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func FuzzRefineSeverityStall(f *testing.F) {
	for _, seed := range []string{"\x01\x01\x01\x01", "\x03\x02\x02\x02\x02", "\x00\x00\x00\x00", "\x01\x02\x03\x04\x05"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		var tracker refineStallTracker
		runStart := 0
		for i, count := range []byte(input) {
			if i == 0 || count == 0 || input[i-1] == 0 || count < input[i-1] {
				runStart = i
			}
			assert.Equal(t, i-runStart == 3, tracker.observeSeverity(int(count)), "observation %d", i)
		}
		tracker.resetSignals()
		assert.False(t, tracker.observeSeverity(1), "reset requires a new baseline")
	})
}

func TestRefineHighMediumCount(t *testing.T) {
	for _, tc := range []struct {
		name       string
		severities []string
		location   any
		unable     bool
		want       int
	}{
		{name: "all severities", severities: []string{"critical", "high", "medium", "low"}, location: "", want: 2},
		{name: "critical is excluded", severities: []string{"critical"}, location: "", want: 0},
		{name: "null location", severities: []string{"medium"}, want: 1},
		{name: "normalized severity", severities: []string{" HIGH ", "Medium"}, location: "", want: 2},
		{name: "unable to review", severities: []string{"medium"}, location: "", unable: true},
		{name: "empty findings", location: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := stallDocument(make([]string, len(tc.severities))...)
			for i, item := range document["findings"].([]any) {
				finding := item.(map[string]any)
				finding["severity"] = tc.severities[i]
				finding["location"] = tc.location
			}
			if tc.unable {
				document["verdict"] = "unable_to_review"
			}
			assert.Equal(t, tc.want, refineHighMediumCount(&storage.Review{StructuredOutput: document}))
		})
	}
	assert.Zero(t, refineHighMediumCount(nil))
	assert.Zero(t, refineHighMediumCount(&storage.Review{}))
}

func FuzzRefineHighMediumCount(f *testing.F) {
	f.Add("high,medium,critical,low", "")
	f.Add("medium,medium", "source.go:1")
	f.Fuzz(func(t *testing.T, input, location string) {
		findings := make([]any, 0, len(input))
		want := 0
		for _, item := range []byte(input) {
			severity := []string{"critical", "high", "medium", "low"}[item%4]
			if severity == "high" || severity == "medium" {
				want++
			}
			findings = append(findings, map[string]any{"severity": severity, "problem": "Ordering needs a change", "fix": "Preserve ordering", "location": location})
		}
		review := &storage.Review{StructuredOutput: map[string]any{"schema_version": 2, "summary": "Review", "verdict": "fail", "findings": findings}}
		assert.Equal(t, want, refineHighMediumCount(review), "location text cannot alter severity totals")
	})
}
