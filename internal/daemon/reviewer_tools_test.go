package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/mcpserver"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

const reviewerCalcBase = "package calc\n\nfunc Avg(xs []int) int {\n\ts := 0\n\tfor _, x := range xs {\n\t\ts += x\n\t}\n\treturn s / len(xs)\n}\n"

const reviewerCalcFixed = "package calc\n\nfunc Avg(xs []int) int {\n\tif len(xs) == 0 {\n\t\treturn 0\n\t}\n\ts := 0\n\tfor _, x := range xs {\n\t\ts += x\n\t}\n\treturn s / len(xs)\n}\n"

// reviewerFixture is a repository with two branches plus a second repository,
// each with finished reviews, and a running review of the fix commit on the
// feature branch.
type reviewerFixture struct {
	server      *Server
	db          *storage.DB
	url         string
	running     *storage.ReviewJob
	featureJob  *storage.ReviewJob
	otherJob    *storage.ReviewJob
	foreignJob  *storage.ReviewJob
	featureSHA  string
	fixSHA      string
	otherSHA    string
	foreignSHA  string
	baseSHA     string
	authKeyUsed string
}

func newReviewerFixture(t *testing.T) *reviewerFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	cfg := config.DefaultConfig()
	cfg.AuthKey = "daemon-api-key"
	server := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })

	repo := testutil.InitTestRepo(t)
	f := &reviewerFixture{server: server, db: db, authKeyUsed: cfg.AuthKey}
	f.baseSHA = repo.CommitFile("calc.go", reviewerCalcBase, "add Avg")
	repo.CheckoutNewBranch("feature")
	f.featureSHA = repo.CommitFile("calc_test.go", "package calc\n", "add calc test")
	f.fixSHA = repo.CommitFile("calc.go", reviewerCalcFixed, "fix: guard Avg against empty input")
	repo.CheckoutNewBranch("other", "main")
	f.otherSHA = repo.CommitFile("other.go", "package calc\n", "other work")

	foreign := testutil.InitTestRepo(t)
	foreign.CheckoutNewBranch("feature")
	f.foreignSHA = foreign.CommitFile("secret.go", "package secret\n", "foreign work")

	f.featureJob = seedReviewerJob(t, db, repo.Root, "feature", f.featureSHA, `{"schema_version":2,"summary":"Avg panics.","verdict":"fail","findings":[`+
		`{"severity":"high","problem":"Avg divides by zero on an empty slice. Callers pass empty input.","fix":"Guard it.","location":"calc.go:8"},`+
		`{"severity":"low","problem":"Sum can overflow.","fix":"Use int64.","location":"calc.go:6"}]}`)
	_, err := db.AddCommentToJob(f.featureJob.ID, "roborev-fix",
		"Fix: guarded empty input in "+f.fixSHA[:9]+". Decline: overflow is not reachable here.")
	require.NoError(t, err)
	f.otherJob = seedReviewerJob(t, db, repo.Root, "other", f.otherSHA,
		`{"schema_version":2,"summary":"other branch secret","verdict":"pass","findings":[]}`)
	f.foreignJob = seedReviewerJob(t, db, foreign.Root, "feature", f.foreignSHA,
		`{"schema_version":2,"summary":"foreign repo secret","verdict":"pass","findings":[]}`)

	repoRow, err := db.GetOrCreateRepo(repo.Root)
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repoRow.ID, f.fixSHA, "Author", "fix", time.Now())
	require.NoError(t, err)
	f.running, err = db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repoRow.ID, CommitID: commit.ID, GitRef: f.fixSHA, Branch: "feature", Agent: "claude-code",
	})
	require.NoError(t, err)
	f.running.RepoPath = repo.Root

	httpSrv := httptest.NewServer(server.httpServer.Handler)
	t.Cleanup(httpSrv.Close)
	f.url = httpSrv.URL
	// The server never starts its own listener here; point agents at httptest.
	server.workerPool.reviewerURL = func() string { return httpSrv.URL + mcpserver.ReviewerHTTPPath }
	return f
}

func agentForName(t *testing.T, name string) agent.Agent {
	t.Helper()
	a, err := agent.Get(name)
	require.NoError(t, err)
	return a
}

func seedReviewerJob(t *testing.T, db *storage.DB, repoPath, branch, sha, document string) *storage.ReviewJob {
	t.Helper()
	repo, err := db.GetOrCreateRepo(repoPath)
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, sha, "Author", "subject "+sha[:7], time.Now())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: sha, Branch: branch, Agent: "test",
	})
	require.NoError(t, err)
	_, err = db.ClaimJob("reviewer-test")
	require.NoError(t, err)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "", document))
	return job
}

func (f *reviewerFixture) issue(t *testing.T) (string, func()) {
	t.Helper()
	tools, revoke, ok := f.server.workerPool.reviewerToolsFor(
		f.running, agentForName(t, "claude-code"), reviewerToolsConfig(), time.Now().Add(time.Hour),
	)
	require.True(t, ok)
	return tools.Token, revoke
}

func reviewerToolsConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.MCP.ReviewerTools = true
	return cfg
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(req)
}

func connectReviewer(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "reviewer-test", Version: "0"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             url + mcpserver.ReviewerHTTPPath,
		HTTPClient:           &http.Client{Transport: bearerTransport{token: token}},
		DisableStandaloneSSE: true,
	}, nil)
	if err == nil {
		t.Cleanup(func() { _ = session.Close() })
	}
	return session, err
}

func callReviewerTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &out))
	return out, result.IsError
}

func TestReviewerToolsReturnOnlyThisBranchOfThisRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReviewerFixture(t)
	token, _ := f.issue(t)
	session, err := connectReviewer(t, f.url, token)
	require.NoError(err)

	tools, err := session.ListTools(t.Context(), nil)
	require.NoError(err)
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(mcpserver.ReviewerToolNames, names)

	findings, isErr := callReviewerTool(t, session, mcpserver.ReviewerToolBranchFindings, nil)
	require.False(isErr, "%v", findings)
	reviews := findings["reviews"].([]any)
	require.Len(reviews, 1, "only the feature branch review of this repo is in scope")
	review := reviews[0].(map[string]any)
	assert.EqualValues(f.featureJob.ID, review["job_id"])
	assert.Equal("default", review["review_type"])
	assert.Equal(false, review["closed"])
	assert.Equal([]any{
		map[string]any{"severity": "high", "location": "calc.go:8", "claim": "Avg divides by zero on an empty slice."},
		map[string]any{"severity": "low", "location": "calc.go:6", "claim": "Sum can overflow."},
	}, review["findings"])
	comments := review["comments"].([]any)
	require.Len(comments, 1)
	assert.Contains(comments[0].(map[string]any)["text"], "Decline: overflow")

	for _, job := range []*storage.ReviewJob{f.otherJob, f.foreignJob, f.running} {
		out, isErr := callReviewerTool(t, session, mcpserver.ReviewerToolPriorReview, map[string]any{"job_id": job.ID})
		assert.True(isErr, "job %d must be out of scope", job.ID)
		assert.Equal(mcpserver.ErrorCodeNotFound, out["error"].(map[string]any)["code"])
	}
	for _, sha := range []string{f.otherSHA, f.foreignSHA} {
		_, isErr := callReviewerTool(t, session, mcpserver.ReviewerToolPriorReview, map[string]any{"commit": sha[:10]})
		assert.True(isErr, "commit %s must be out of scope", sha)
	}
	prior, isErr := callReviewerTool(t, session, mcpserver.ReviewerToolPriorReview, map[string]any{"commit": f.featureSHA[:8]})
	require.False(isErr, "%v", prior)
	assert.Contains(prior["reviews"].([]any)[0].(map[string]any)["output"], "Avg divides by zero")
}

func TestReviewerGitToolsLinkFixCommits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newReviewerFixture(t)
	token, _ := f.issue(t)
	session, err := connectReviewer(t, f.url, token)
	require.NoError(err)

	blame, isErr := callReviewerTool(t, session, mcpserver.ReviewerToolGitBlame,
		map[string]any{"path": "calc.go", "start_line": 3, "end_line": 7})
	require.False(isErr, "%v", blame)
	hunks := blame["hunks"].([]any)
	require.Len(hunks, 3)
	signature, guard, body := hunks[0].(map[string]any), hunks[1].(map[string]any), hunks[2].(map[string]any)
	assert.Equal("3", signature["lines"])
	assert.Equal(f.baseSHA[:12], signature["commit"])
	assert.Equal(false, signature["on_branch"], "main history is not part of the feature branch")
	assert.Equal("4-6", guard["lines"])
	assert.Equal(f.fixSHA[:12], guard["commit"])
	assert.Equal(true, guard["on_branch"])
	assert.Equal([]any{float64(f.featureJob.ID)}, guard["fix_for_jobs"])
	assert.Equal("7", body["lines"])

	log, isErr := callReviewerTool(t, session, mcpserver.ReviewerToolGitLog, map[string]any{"path": "calc.go"})
	require.False(isErr, "%v", log)
	commits := log["commits"].([]any)
	require.Len(commits, 2)
	assert.Equal("fix: guard Avg against empty input", commits[0].(map[string]any)["subject"])
	assert.Equal("add Avg", commits[1].(map[string]any)["subject"])

	for _, path := range []string{"../outside.go", "/etc/passwd", "", "a/../../b"} {
		out, isErr := callReviewerTool(t, session, mcpserver.ReviewerToolGitBlame, map[string]any{"path": path, "start_line": 1})
		assert.True(isErr, "path %q must be rejected", path)
		assert.Equal(mcpserver.ErrorCodeInvalidArgument, out["error"].(map[string]any)["code"])
	}
	// other.go exists only on the other branch, so the reviewed commit has no such file.
	_, isErr = callReviewerTool(t, session, mcpserver.ReviewerToolGitLog, map[string]any{"path": "other.go", "start_line": 1})
	assert.True(isErr)
}

func TestReviewerEndpointRejectsMissingAndRevokedTokens(t *testing.T) {
	f := newReviewerFixture(t)
	_, err := connectReviewer(t, f.url, "not-a-token")
	require.Error(t, err)
	_, err = connectReviewer(t, f.url, f.authKeyUsed)
	require.Error(t, err, "the daemon API key is not a reviewer token")

	token, revoke := f.issue(t)
	_, err = connectReviewer(t, f.url, token)
	require.NoError(t, err)
	revoke()
	_, err = connectReviewer(t, f.url, token)
	require.Error(t, err)
}

func TestReviewerTokenDoesNotOpenTheDaemonAPI(t *testing.T) {
	f := newReviewerFixture(t)
	token, _ := f.issue(t)
	for _, path := range []string{"/api/jobs", "/mcp"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.url+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, path)
	}
}

func TestReviewerTokenExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		grants := newReviewerGrants()
		scope := mcpserver.ReviewerScope{JobID: 1, RepoID: 1, RepoPath: "/repo", Branch: "feature", SHA: "abc"}
		token, _ := grants.issue(scope, time.Now().Add(time.Minute))

		got, _, ok := grants.verify(token)
		require.True(t, ok)
		assert.Equal(t, scope, got)

		time.Sleep(time.Minute)
		_, _, ok = grants.verify(token)
		assert.False(t, ok)
	})
}

func TestReviewerToolsIssuedOnlyWhenEnabledAndEligible(t *testing.T) {
	f := newReviewerFixture(t)
	claude := agentForName(t, "claude-code")
	expires := time.Now().Add(time.Hour)

	_, _, ok := f.server.workerPool.reviewerToolsFor(f.running, claude, config.DefaultConfig(), expires)
	assert.False(t, ok, "off by default")
	_, _, ok = f.server.workerPool.reviewerToolsFor(f.running, agentForName(t, "test"), reviewerToolsConfig(), expires)
	assert.False(t, ok, "agents without reviewer tool wiring get nothing")

	ci := *f.running
	ci.PromptPrebuilt = true
	ci.Branch = ""
	_, _, ok = f.server.workerPool.reviewerToolsFor(&ci, claude, reviewerToolsConfig(), expires)
	assert.False(t, ok, "CI reviews carry untrusted context")

	tools, _, ok := f.server.workerPool.reviewerToolsFor(f.running, claude, reviewerToolsConfig(), expires)
	require.True(t, ok)
	assert.True(t, strings.HasSuffix(tools.URL, mcpserver.ReviewerHTTPPath))
}
