//go:build integration

package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

const domainSource = "Pinned source: the shipment arrived on Monday.\r\nCafé.\n"

// This test breaks if hook batching skips/duplicates a commit, custom members
// lose frozen settings, evidence comes from the checkout, or synthesis takes
// an all-clean shortcut despite two voters reporting findings.
func TestDomainReviewHookPanel(t *testing.T) {
	cli := buildDomainCLI(t)
	citationFinding := structuredreview.Finding{Severity: "high", Problem: "The shipment date contradicts the pinned source.", Fix: "Use Monday as the arrival date."}
	quoteFinding := structuredreview.Finding{Severity: "high", Problem: "The quoted wording differs from the pinned source.", Fix: "Quote the pinned text exactly."}
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprintf("oversized=%t", oversized), func(t *testing.T) {
			var calls []string
			var attemptPaths []string
			var fixture *domainHookFixture
			a := &domainProofAgent{invoke: func(_ context.Context, a *domainProofAgent, checkout, text string, schema jsontext.Value) (jsontext.Value, error) {
				full, paths := inspectDomainPrompt(t, checkout, text, oversized)
				attemptPaths = append(attemptPaths, paths...)
				require.NotNil(t, a.policy, "all members and synthesis must be strict")
				assert.Empty(t, a.policy.MCP)
				native, err := os.ReadFile(a.configPath)
				require.NoError(t, err)
				assert.JSONEq(t, `{"mcpServers":{}}`, string(native))
				attemptPaths = append(attemptPaths, a.configPath)
				assert.NotContains(t, full, "You are a code reviewer")
				assert.NotContains(t, full, "multiple code review outputs")
				assert.NotContains(t, full, "current codebase")
				if bytes.Equal(schema, review.SynthesisSchema) {
					calls = append(calls, "synthesis")
					assert.Contains(t, full, "reviewed material")
					inputs := strings.Split(full, "---\n### Review ")
					require.Len(t, inputs, 3, "synthesis must receive both member contributions")
					assert.True(t, strings.HasPrefix(inputs[1], "1\nRubric: citations\n"))
					assert.Contains(t, inputs[1], citationFinding.Problem)
					assert.Contains(t, inputs[1], citationFinding.Fix)
					assert.NotContains(t, inputs[1], quoteFinding.Problem)
					assert.True(t, strings.HasPrefix(inputs[2], "2\nRubric: quotes\n"))
					assert.Contains(t, inputs[2], quoteFinding.Problem)
					assert.Contains(t, inputs[2], quoteFinding.Fix)
					assert.NotContains(t, inputs[2], citationFinding.Problem)
					citation, quote := citationFinding, quoteFinding
					citation.Sources = []int{1}
					quote.Sources = []int{2}
					return domainFindings(t, "Combined findings", citation, quote), nil
				}
				var finding structuredreview.Finding
				if strings.Contains(full, "CITATIONS-RUBRIC") {
					calls = append(calls, "citations")
					finding = citationFinding
				} else {
					require.Contains(t, full, "QUOTES-RUBRIC")
					calls = append(calls, "quotes")
					finding = quoteFinding
				}
				assertDomainEvidence(t, fixture, checkout, full)
				return domainFindings(t, "Member finding", finding), nil
			}}
			registerDomainAgent(t, a)
			fixture = newDomainHookFixture(t, cli, a.Name(), oversized, false, nil)
			fixture.commitBatch(t)
			// A changed working copy cannot replace committed PDF/text evidence.
			fixture.repo.WriteFile("evidence/source-a.txt", "Uncommitted replacement")
			fixture.repo.WriteFile("exhibit.pdf", "Uncommitted replacement")
			fixture.finishPanel(t)
			assert.Equal(t, []string{"citations", "quotes", "synthesis"}, calls)
			for _, path := range attemptPaths {
				assert.NoFileExists(t, path, "attempt artifact must be cleaned")
			}
			fixture.assertStored(t)
		})
	}
}

func TestDomainReviewHookRetryAndCancel(t *testing.T) {
	cli := buildDomainCLI(t)
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancel), func(t *testing.T) {
			var fixture *domainHookFixture
			var attempts [][]string
			a := &domainProofAgent{invoke: func(ctx context.Context, a *domainProofAgent, checkout, text string, _ jsontext.Value) (jsontext.Value, error) {
				full, paths := inspectDomainPrompt(t, checkout, text, true)
				assertDomainEvidence(t, fixture, checkout, full)
				require.NotNil(t, a.policy)
				assert.Empty(t, a.policy.MCP)
				attempts = append(attempts, append(paths, a.configPath))
				if cancel {
					require.NoError(t, fixture.db.CancelJob(fixture.response.MemberJobIDs[0]))
					require.True(t, fixture.server.workerPool.CancelJob(fixture.response.MemberJobIDs[0]))
					require.ErrorIs(t, ctx.Err(), context.Canceled)
					return nil, ctx.Err()
				}
				if len(attempts) == 1 {
					return nil, errors.New("synthetic transient provider failure")
				}
				return domainFindings(t, "Retry member finding", structuredreview.Finding{
					Severity: "high", Problem: "The shipment date contradicts the pinned source.", Fix: "Use Monday as the arrival date.",
				}), nil
			}}
			registerDomainAgent(t, a)
			fixture = newDomainHookFixture(t, cli, a.Name(), true, false, nil)
			fixture.commitBatch(t)
			// Leave just the first member claimable so the next claim is its retry.
			require.NoError(t, fixture.db.CancelJob(fixture.response.MemberJobIDs[1]))
			count := 2
			if cancel {
				count = 1
			}
			for i := range count {
				job, err := fixture.db.ClaimJob(testWorkerID)
				require.NoError(t, err)
				require.NotNil(t, job)
				require.Equal(t, fixture.response.MemberJobIDs[0], job.ID)
				require.Equal(t, i, job.RetryCount)
				fixture.server.workerPool.processJob(testWorkerID, job)
				for _, paths := range attempts {
					for _, path := range paths {
						assert.NoFileExists(t, path)
					}
				}
			}
			require.Len(t, attempts, count)
			job, err := fixture.db.GetJobByID(fixture.response.MemberJobIDs[0])
			require.NoError(t, err)
			if cancel {
				assert.Equal(t, storage.JobStatusCanceled, job.Status)
				assert.Zero(t, job.RetryCount)
				_, err = fixture.db.GetReviewByJobID(job.ID)
				assert.Error(t, err)
			} else {
				assert.Equal(t, storage.JobStatusDone, job.Status)
				assert.Equal(t, 1, job.RetryCount)
				assert.NotEqual(t, attempts[0][0], attempts[1][0], "retry must bind fresh attempt paths")
			}
		})
	}
}

// The observation channel must not block a duplicate HTTP enqueue before the
// real-hook test can fail its independent atomic request-count assertion.
func TestDomainReviewHookDuplicateEnqueue(t *testing.T) {
	a := &domainProofAgent{}
	registerDomainAgent(t, a)
	fixture := newDomainHookFixture(t, "unused-cli", a.Name(), false, false, nil)
	// Drain before httptest.Server.Close so a regressed blocking send is
	// released even when the duplicate request times out and require stops us.
	t.Cleanup(func() {
		select {
		case <-fixture.responses:
		default:
		}
	})
	body, err := json.Marshal(EnqueueRequest{
		RepoPath: fixture.repo.Root,
		GitRef:   fixture.base,
		Agent:    a.Name(),
		Panel:    "drafts",
	})
	require.NoError(t, err)
	// These real loopback sockets cannot run on synctest's virtual clock.
	// The deadline bounds a deadlocked observer; it is not a latency assertion.
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	var replies []PanelEnqueueResponse
	for i := range 2 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fixture.enqueueURL, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		assert.Equal(t, int32(i+1), fixture.requests.Load())
		require.NoError(t, err, "duplicate HTTP enqueue must return without draining the observation channel")
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(data))
		var reply PanelEnqueueResponse
		require.NoError(t, json.Unmarshal(data, &reply))
		require.NotNil(t, reply.ReviewJob)
		require.Len(t, reply.MemberJobIDs, 2)
		replies = append(replies, reply)
	}
	assert.NotEqual(t, replies[0].PanelRunUUID, replies[1].PanelRunUUID)
	require.Len(t, fixture.responses, 1)
	assert.Equal(t, replies[0].ID, (<-fixture.responses).ID)
	assert.Equal(t, int32(2), fixture.requests.Load(), "the observer must still count duplicate requests")
}

func registerDomainAgent(t *testing.T, a agent.Agent) {
	t.Helper()
	original, err := agent.Get(a.Name())
	require.NoError(t, err)
	agent.Register(a)
	t.Cleanup(func() { agent.Register(original) })
}

type domainHookFixture struct {
	repo       *testutil.TestRepo
	server     *Server
	db         *storage.DB
	requests   atomic.Int32
	responses  chan PanelEnqueueResponse
	enqueueURL string
	base       string
	head       string
	response   PanelEnqueueResponse
	pdf        []byte
}

func buildDomainCLI(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("installed Git hook fixture uses a POSIX launcher")
	}
	dir, err := os.MkdirTemp("/tmp", "rdr-domain-cli-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	path := filepath.Join(dir, "roborev")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", path, "../../cmd/roborev")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "build disposable CLI: %s", output)
	return path
}

func newDomainHookFixture(t *testing.T, cli, agentName string, oversized, granted bool, configure func(*config.Config)) *domainHookFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.IsolateReviews = true
	cfg.Sync.Enabled = false
	if configure != nil {
		configure(cfg)
	}
	db, _ := testutil.OpenTestDBWithDir(t)
	server := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
	server.workerPool.retryBackoff = 0
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	f := &domainHookFixture{repo: testutil.NewGitRepo(t), server: server, db: db, pdf: domainPDF(), responses: make(chan PanelEnqueueResponse, 1)}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/enqueue" {
			f.requests.Add(1)
			// Forward the real request to the real handler. Never fake acceptance.
			recorder := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(recorder, r)
			assert.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
			var response PanelEnqueueResponse
			assert.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			select {
			case f.responses <- response:
			default:
			}
			maps.Copy(w.Header(), recorder.Header())
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		server.httpServer.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	f.enqueueURL = httpServer.URL + "/api/enqueue"
	launcher := filepath.Join(t.TempDir(), "hook-cli")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	require.NoError(t, os.WriteFile(launcher, []byte("#!/bin/sh\nexec "+quote(cli)+" --server "+quote(httpServer.URL)+" \"$@\"\n"), 0o700))
	policy := "mcp = {}"
	if granted {
		policy = "mcp = { archive = ['lookup'] }"
	}
	transport := ""
	if oversized {
		transport = "max_prompt_size = 1\n" // Existing transport selector, not a payload cap.
	}
	f.repo.CommitFiles(map[string]string{
		".gitignore":     ".roborev/\n",
		".gitattributes": "*.pdf binary\n",
		".roborev.toml": fmt.Sprintf(`post_commit_batch_size = 5
post_commit_review = "commit"
%s
[review]
hook_review_panel = "drafts"
[review.types.citations]
template = "rubrics/citations.md"
binary_evidence = true
evidence_manifest = "evidence/manifest.toml"
[review.types.citations.tools]
mode = "read-only"
%s
[review.types.quotes]
template = "rubrics/quotes.md"
binary_evidence = true
evidence_manifest = "evidence/manifest.toml"
[review.types.quotes.tools]
mode = "read-only"
mcp = { archive = ["lookup"] }
[review.subagents.citations]
agent = %q
review_type = "citations"
[review.subagents.quotes]
agent = %q
review_type = "quotes"
[review.subagents.quotes.tools]
mode = "read-only"
mcp = {}
[review.panels.drafts]
members = ["citations", "quotes"]
synthesis_agent = %q
`, transport, policy, agentName, agentName, agentName),
		"rubrics/citations.md":   "CITATIONS-RUBRIC\nReview the shipment date against the pinned source. Report the contradiction as a high finding.\n" + domainReadInstructions,
		"rubrics/quotes.md":      "QUOTES-RUBRIC\nReview the quoted wording against the pinned source. Report the contradiction as a high finding.\n" + domainReadInstructions,
		"evidence/source-a.txt":  domainSource,
		"evidence/manifest.toml": fmt.Sprintf("version = 1\n[[documents]]\nid = 'source-a'\ntext_path = 'evidence/source-a.txt'\ntext_sha256 = '%x'\nsource_sha256 = '%x'\nprovenance = 'docbank://vaults/V/documents/N/versions/C/renditions/A'\n", sha256.Sum256([]byte(domainSource)), sha256.Sum256([]byte("declared source"))),
		"draft.md":               "# Shipment report\nThe shipment arrived on Friday.\nThe source says: ‘arrived on Sunday’.\n",
		"unchanged.txt":          "synthetic sentinel\n",
	}, "prepare synthetic review")
	f.base = f.repo.HeadSHA()
	require.NoError(t, githook.InstallWithOptions(f.repo.HooksDir, "post-commit", githook.InstallOptions{BinaryPath: launcher}))
	return f
}

const domainReadInstructions = `Read both the offered PDF and verified text snapshots using native Read.
Use the full prompt if it is in a file. Preserve the end marker DOMAIN-PROMPT-END.
DOMAIN-PROMPT-END
`

func (f *domainHookFixture) commitBatch(t *testing.T) {
	t.Helper()
	var commits []string
	for i := range 5 {
		if i == 0 {
			f.repo.WriteFile("exhibit.pdf", string(f.pdf))
		}
		f.repo.WriteFile(fmt.Sprintf("draft-%d.md", i), fmt.Sprintf("Revision %d: the shipment arrived on Friday.\nThe source says: ‘arrived on Sunday’.\nDOMAIN-DRAFT-END-%d\n", i, i))
		// CommitFile uses go-git and bypasses hooks. These must be actual Git commits.
		f.repo.Run("add", ".")
		f.repo.Run("commit", "-m", fmt.Sprintf("revise draft %d", i))
		commits = append(commits, f.repo.HeadSHA())
		if i < 4 {
			require.Zero(t, f.requests.Load(), "a partial batch must not enqueue")
		}
	}
	f.head = f.repo.HeadSHA()
	log, _ := os.ReadFile(filepath.Join(config.DataDir(), "post-commit.log"))
	require.Equal(t, int32(1), f.requests.Load(), "five actual hook commits enqueue one panel: %s", log)
	f.response = <-f.responses
	require.NotNil(t, f.response.ReviewJob)
	require.Len(t, f.response.MemberJobIDs, 2)
	require.NotEmpty(t, f.response.PanelRunUUID)
	wantRef := f.base + ".." + f.head
	assert.Equal(t, strings.Join(commits, "\n"), f.repo.Run("rev-list", "--reverse", "--first-parent", wantRef))
	members, err := f.db.GetPanelMembers(f.response.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 2)
	for i, member := range members {
		assert.Equal(t, wantRef, member.GitRef)
		assert.Equal(t, storage.JobSourcePostCommit, member.Source)
		assert.Equal(t, i, member.PanelMemberIndex)
		snapshot, err := decodeReviewExecutionSnapshot(member.PanelMemberConfigJSON)
		require.NoError(t, err)
		require.NotNil(t, snapshot.Tools)
		if member.PanelMemberName == "quotes" {
			assert.Empty(t, snapshot.Tools.MCP, "present empty member policy replaces the type's grant")
		}
		assert.True(t, snapshot.BinaryEvidence)
		assert.Equal(t, "evidence/manifest.toml", snapshot.EvidenceManifest)
	}
	parent, err := f.db.GetJobByID(f.response.ID)
	require.NoError(t, err)
	assert.Equal(t, wantRef, parent.GitRef)
	assert.True(t, parent.ClaimBlocked)
}

func (f *domainHookFixture) finishPanel(t *testing.T) {
	t.Helper()
	for _, id := range f.response.MemberJobIDs {
		job, err := f.db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		require.NotNil(t, job)
		require.Equal(t, id, job.ID)
		f.server.workerPool.processJob(testWorkerID, job)
		stored, err := f.db.GetJobByID(id)
		require.NoError(t, err)
		require.Equal(t, storage.JobStatusDone, stored.Status, "%s", stored.Error)
	}
	require.NoError(t, f.db.MaybeReleasePanelSynthesis(f.response.PanelRunUUID))
	job, err := f.db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, f.response.ID, job.ID)
	f.server.workerPool.processSynthesisJob(t.Context(), testWorkerID, job)
	stored, err := f.db.GetJobByID(job.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, stored.Status, "%s", stored.Error)
}

func (f *domainHookFixture) assertStored(t *testing.T) {
	t.Helper()
	for _, id := range f.response.MemberJobIDs {
		job, err := f.db.GetJobByID(id)
		require.NoError(t, err)
		assert.Contains(t, job.Prompt, "roborev-evidence:v1")
		assert.NotContains(t, job.Prompt, f.repo.Root)
		assert.Contains(t, job.Prompt, fmt.Sprintf("%x", sha256.Sum256(f.pdf)))
		assert.Contains(t, job.Prompt, fmt.Sprintf("%x", sha256.Sum256([]byte(domainSource))))
		assert.Contains(t, job.Prompt, f.repo.Run("rev-parse", f.head+":evidence/source-a.txt"))
		isolated, err := f.db.JobSessionIsolated(t.Context(), job.ID)
		require.NoError(t, err)
		assert.True(t, isolated)
	}
	result, err := f.db.GetReviewByJobID(f.response.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.VerdictFail, storage.ParseVerdict(result.Output))
	assert.Contains(t, result.Output, "citations")
	assert.Contains(t, result.Output, "quotes")
}

func inspectDomainPrompt(t *testing.T, checkout, text string, oversized bool) (string, []string) {
	t.Helper()
	var paths []string
	if oversized {
		path := promptSnapshotPath(t, checkout, text)
		full, err := os.ReadFile(path)
		require.NoError(t, err)
		text = string(full)
		paths = append(paths, path)
	}
	files, err := filepath.Glob(filepath.Join(checkout, ".roborev", "roborev-snapshot-*", "*"))
	require.NoError(t, err)
	return text, append(paths, files...)
}

func assertDomainEvidence(t *testing.T, f *domainHookFixture, checkout, text string) {
	t.Helper()
	assert.NotEqual(t, f.repo.Root, checkout, "snapshot belongs in actual isolated agent checkout")
	assert.Equal(t, 1, strings.Count(text, "## Committed review evidence"))
	assert.Contains(t, text, "declared provenance (opaque, not verified)")
	assert.Contains(t, text, "DOMAIN-PROMPT-END")
	for extension, payload := range map[string][]byte{"pdf": f.pdf, "txt": []byte(domainSource)} {
		filename := fmt.Sprintf("%x.%s", sha256.Sum256(payload), extension)
		files, err := filepath.Glob(filepath.Join(checkout, ".roborev", "roborev-snapshot-*", filename))
		require.NoError(t, err)
		require.Len(t, files, 1)
		data, err := os.ReadFile(files[0])
		require.NoError(t, err)
		assert.Equal(t, payload, data)
		assert.Contains(t, text, files[0])
		info, err := os.Stat(files[0])
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func domainPDF() []byte {
	var doc bytes.Buffer
	doc.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	content := "BT /F1 18 Tf 72 720 Td (Synthetic shipment exhibit: Monday.) Tj ET\n"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
	}
	var offsets []int
	for i, object := range objects {
		offsets = append(offsets, doc.Len())
		fmt.Fprintf(&doc, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := doc.Len()
	fmt.Fprintf(&doc, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&doc, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&doc, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return doc.Bytes()
}

type domainProofAgent struct {
	policy     *config.ReviewToolPolicy
	configPath string
	invoke     func(context.Context, *domainProofAgent, string, string, jsontext.Value) (jsontext.Value, error)
}

// The hook CLI validates this known adapter name in its own process. Only
// this test's daemon registry replaces the implementation.
func (*domainProofAgent) Name() string                                     { return "claude-code" }
func (a *domainProofAgent) CommandLine() string                            { return a.Name() }
func (a *domainProofAgent) WithModel(string) agent.Agent                   { return a }
func (a *domainProofAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }
func (a *domainProofAgent) WithAgentic(bool) agent.Agent                   { return a }
func (a *domainProofAgent) WithStrictReview(p config.ReviewToolPolicy, path string) agent.Agent {
	clone := *a
	clone.policy = p.Clone()
	clone.configPath = path
	return &clone
}

func (*domainProofAgent) Review(context.Context, string, string, string, io.Writer) (string, error) {
	return "", errors.New("domain proof requires schema review")
}

func (a *domainProofAgent) ReviewWithSchema(ctx context.Context, checkout, _, text string, schema jsontext.Value, _ io.Writer) (jsontext.Value, error) {
	return a.invoke(ctx, a, checkout, text, schema)
}

func domainFindings(t *testing.T, summary string, findings ...structuredreview.Finding) jsontext.Value {
	t.Helper()
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: summary, Verdict: structuredreview.VerdictFail, Findings: findings}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	return raw
}

var (
	_ agent.StructuredReviewAgent = (*domainProofAgent)(nil)
	_ agent.StrictReviewAgent     = (*domainProofAgent)(nil)
)
