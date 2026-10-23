package main

import (
	"encoding/json"
	"encoding/json/jsontext"
	"net/http"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/findingid"
	"go.kenn.io/roborev/internal/storage"
)

const showFindingsDocument = `{"schema_version":2,"summary":"Two problems.","verdict":"fail","findings":[` +
	`{"severity":"high","problem":"Nil dereference.","fix":"Check first.","location":"a.go:10"},` +
	`{"severity":"low","problem":"Typo.","fix":"Spell it.","location":null}]}`

const showFindingsReviewUUID = "0b9f1c2e-4d5a-4e6f-8a7b-1c2d3e4f5a6b"

func structuredReviewFixture(t *testing.T, document string) storage.Review {
	t.Helper()
	var structured storage.StructuredOutput
	if document != "" {
		require.NoError(t, json.Unmarshal([]byte(document), &structured))
	}
	reviewUUID := uuid.MustParse(showFindingsReviewUUID)
	return storage.Review{
		ID: 1, JobID: 42, Agent: "test", Output: "## Findings\n\n### 1. High", StructuredOutput: structured,
		UUID:        &reviewUUID,
		VerdictBool: new(0),
	}
}

// sampleFindings provides reusable selector values for rebuttal tests.
func sampleFindings() daemon.ReviewFindingsBody {
	reviewUUID := uuid.MustParse(showFindingsReviewUUID)
	selector := func(index int) string {
		id, err := findingid.New(reviewUUID, index, strings.Repeat("ab", 32))
		if err != nil {
			panic(err)
		}
		return id.String()
	}
	return daemon.ReviewFindingsBody{JobID: 42, Findings: []daemon.ReviewFinding{
		{ID: selector(0), Index: 0, Severity: "high", Problem: "Nil dereference.", Fix: "Check first.", Location: "a.go:10"},
		{ID: selector(1), Index: 1, Severity: "low", Problem: "Typo.", Fix: "Spell it."},
	}}
}

// showSampleFindings lists selectors tied to the displayed review.
func showSampleFindings(t *testing.T, review storage.Review) daemon.ReviewFindingsBody {
	t.Helper()
	require.NotNil(t, review.UUID)
	data, err := json.Marshal(review.StructuredOutput)
	require.NoError(t, err)
	documentHash, err := findingid.DocumentHash(jsontext.Value(data))
	require.NoError(t, err)
	selector := func(index int) string {
		id, err := findingid.New(*review.UUID, index, documentHash)
		require.NoError(t, err)
		return id.String()
	}
	return daemon.ReviewFindingsBody{JobID: review.JobID, ReviewUUID: review.UUID, DocumentHash: documentHash, Findings: []daemon.ReviewFinding{
		{ID: selector(0), Index: 0, Severity: "high", Problem: "Nil dereference.", Fix: "Check first.", Location: "a.go:10"},
		{ID: selector(1), Index: 1, Severity: "low", Problem: "Typo.", Fix: "Spell it."},
	}}
}

// showDaemon serves one review and answers the findings route with respond.
// It returns the paths of the findings requests it saw.
func showDaemon(t *testing.T, review storage.Review, respond func(w http.ResponseWriter)) func() []string {
	t.Helper()
	var (
		mu       sync.Mutex
		requests []string
	)
	NewMockDaemon(t, MockRefineHooks{
		OnReview: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
			_ = json.NewEncoder(w).Encode(review)
			return true
		},
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			if r.URL.Path != "/api/review/findings" {
				return false
			}
			mu.Lock()
			requests = append(requests, r.URL.RequestURI())
			mu.Unlock()
			respond(w)
			return true
		},
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), requests...)
	}
}

func respondWith(body any) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { _ = json.NewEncoder(w).Encode(body) }
}

func TestShowListsFindingSelectors(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial commit")
	review := structuredReviewFixture(t, showFindingsDocument)
	findings := showSampleFindings(t, review)
	requests := showDaemon(t, review, respondWith(findings))
	chdir(t, repo.Dir)

	output := runShowCmd(t, "--job", "42")

	assert := assert.New(t)
	assert.Contains(output, "--- Findings ---")
	assert.Contains(output, "1. "+findings.Findings[0].ID+"  high  a.go:10")
	assert.Contains(output, "2. "+findings.Findings[1].ID+"  low\n")
	assert.Equal([]string{"/api/review/findings?job_id=42"}, requests())
	assert.Greater(strings.Index(output, "--- Findings ---"), strings.Index(output, "### 1. High"),
		"the selectors follow the review text")
}

func TestShowJSONGainsFindingIDs(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial commit")
	review := structuredReviewFixture(t, showFindingsDocument)
	findings := showSampleFindings(t, review)
	requests := showDaemon(t, review, respondWith(findings))
	chdir(t, repo.Dir)

	output := runShowCmd(t, "--job", "42", "--json")
	assert.Equal(t, []string{"/api/review/findings?job_id=42"}, requests(), "one request for the selectors, as in the text view")

	var parsed struct {
		JobID    int64 `json:"job_id"`
		Findings []struct {
			ID       string `json:"id"`
			Index    int    `json:"index"`
			Severity string `json:"severity"`
			Problem  string `json:"problem"`
			Fix      string `json:"fix"`
			Location string `json:"location"`
		} `json:"findings"`
		StructuredOutput map[string]any `json:"structured_output"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &parsed))
	assert.EqualValues(t, 42, parsed.JobID, "the review fields are unchanged")
	assert.NotEmpty(t, parsed.StructuredOutput)
	require.Len(t, parsed.Findings, 2)
	assert.Equal(t, findings.Findings[0].ID, parsed.Findings[0].ID)
	assert.Equal(t, "Nil dereference.", parsed.Findings[0].Problem)
	assert.Equal(t, "a.go:10", parsed.Findings[0].Location)
	assert.Equal(t, 1, parsed.Findings[1].Index)
}

func TestShowSkipsFindingsForReviewsWithoutStructuredOutput(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial commit")
	requests := showDaemon(t, structuredReviewFixture(t, ""), respondWith(daemon.ReviewFindingsBody{}))
	chdir(t, repo.Dir)

	output := runShowCmd(t, "--job", "42")

	assert.NotContains(t, output, "--- Findings ---")
	assert.Empty(t, requests(), "a legacy prose review has no identities to ask for")
}

func TestShowOmitsSelectorsWhenTheCurrentReviewChanged(t *testing.T) {
	for name, mutate := range map[string]func(*daemon.ReviewFindingsBody){
		"different review": func(body *daemon.ReviewFindingsBody) {
			other := uuid.MustParse("0b9f1c2e-4d5a-4e6f-8a7b-1c2d3e4f5a6c")
			body.ReviewUUID = &other
		},
		"different document": func(body *daemon.ReviewFindingsBody) {
			body.DocumentHash = strings.Repeat("cd", 32)
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newTestGitRepo(t)
			repo.CommitFile("file.txt", "content", "initial commit")
			review := structuredReviewFixture(t, showFindingsDocument)
			findings := showSampleFindings(t, review)
			mutate(&findings)
			showDaemon(t, review, respondWith(findings))
			chdir(t, repo.Dir)

			output := runShowCmd(t, "--job", "42")
			assert.Contains(t, output, "Review for job 42")
			assert.NotContains(t, output, "--- Findings ---")

			var asJSON map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(runShowCmd(t, "--job", "42", "--json")), &asJSON))
			assert.NotContains(t, asJSON, "findings")
		})
	}
}

func TestShowWithoutSelectorsWhenTheDaemonCannotListThem(t *testing.T) {
	for name, respond := range map[string]func(http.ResponseWriter){
		"older daemon":       func(w http.ResponseWriter) { http.NotFound(w, nil) },
		"remote read grant":  func(w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) },
		"empty answer":       func(w http.ResponseWriter) {},
		"clean review":       respondWith(daemon.ReviewFindingsBody{JobID: 42, Findings: []daemon.ReviewFinding{}}),
		"unusable answer":    func(w http.ResponseWriter) { _, _ = w.Write([]byte("<html>")) },
		"daemon error":       func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
		"changed after pins": func(w http.ResponseWriter) { w.WriteHeader(http.StatusConflict) },
	} {
		t.Run(name, func(t *testing.T) {
			repo := newTestGitRepo(t)
			repo.CommitFile("file.txt", "content", "initial commit")
			showDaemon(t, structuredReviewFixture(t, showFindingsDocument), respond)
			chdir(t, repo.Dir)

			output := runShowCmd(t, "--job", "42")
			assert.Contains(t, output, "Review for job 42", "the review itself is still shown")
			assert.NotContains(t, output, "--- Findings ---")

			var asJSON map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(runShowCmd(t, "--job", "42", "--json")), &asJSON))
			assert.NotContains(t, asJSON, "findings", "the top-level list is absent, not empty")
			assert.Contains(t, asJSON, "structured_output")
		})
	}
}
