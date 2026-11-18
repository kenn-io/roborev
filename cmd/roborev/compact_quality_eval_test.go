//go:build qualityeval

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// Only authenticated bytes enter preparation. Validation and consumption must
// use the same snapshot even if the external harness checkout later changes.
type compactQualityCorpus struct {
	files map[string][]byte
}

func compactQualityLoadFrozenCorpus(ctx context.Context, root, head string) (compactQualityCorpus, error) {
	actual, err := compactQualityCommand(ctx, root, "git", "rev-parse", "HEAD")
	if err != nil {
		return compactQualityCorpus{}, err
	}
	if head == "" || head != actual {
		return compactQualityCorpus{}, fmt.Errorf("expected corpus full SHA %q differs from checkout HEAD %s", head, actual)
	}
	corpus := compactQualityCorpus{files: make(map[string][]byte)}
	paths := []string{"rubric.json"}
	for _, mechanism := range []string{"invoice", "cursor"} {
		for _, variant := range []string{"vulnerable", "fixed", "safe"} {
			paths = append(paths, mechanism+"/"+variant+"/fixture.go")
		}
	}
	for _, path := range paths {
		relative := "internal/review/testdata/quality/" + path
		command := exec.CommandContext(ctx, "git", "show", head+":"+relative)
		command.Dir = root
		committed, err := command.Output()
		if err != nil {
			return compactQualityCorpus{}, fmt.Errorf("read frozen corpus %s: %w", path, err)
		}
		current, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return compactQualityCorpus{}, fmt.Errorf("read corpus checkout %s: %w", path, err)
		}
		if !bytes.Equal(current, committed) {
			return compactQualityCorpus{}, fmt.Errorf("corpus %s differs from frozen revision %s", path, head)
		}
		corpus.files[path] = committed
	}
	return corpus, nil
}

// This frozen harness runs unchanged on baseline and candidate. It uses the
// actual CLI prompt/enqueue function, daemon worker, canonical persistence and
// source closure. No disposition/closure is inferred from an absent finding.
// Live calls require explicit mode, auth and a local schema extension. Plan
// mode exercises preparation/enqueue without starting any worker/model.
type compactQualityRow struct {
	EffectiveSchemaHash string                   `json:"effective_schema_hash,omitempty"`
	EffectivePrompt     string                   `json:"effective_prompt,omitempty"`
	EffectiveSchema     string                   `json:"effective_schema,omitempty"`
	NormalizedPolicy    string                   `json:"normalized_policy"`
	AgentInvoked        bool                     `json:"agent_invoked"`
	RetryCount          int                      `json:"retry_count"`
	PiVersion           string                   `json:"pi_version"`
	ExtensionHash       string                   `json:"extension_hash"`
	Case                string                   `json:"case"`
	Repeat              int                      `json:"repeat"`
	ProductHead         string                   `json:"product_head"`
	HarnessHash         string                   `json:"harness_hash"`
	CorpusHead          string                   `json:"corpus_head"`
	FixtureHash         string                   `json:"fixture_hash"`
	RubricHash          string                   `json:"rubric_hash"`
	StoredPromptHash    string                   `json:"stored_prompt_hash"`
	EffectivePromptHash string                   `json:"effective_prompt_hash,omitempty"`
	Agent               string                   `json:"agent"`
	Model               string                   `json:"model"`
	Reasoning           string                   `json:"reasoning"`
	Status              string                   `json:"status"`
	Error               string                   `json:"error,omitempty"`
	SourceClosed        bool                     `json:"source_closed"`
	Disposition         string                   `json:"disposition"`
	Findings            int                      `json:"retained_findings"`
	Output              string                   `json:"output,omitempty"`
	Document            storage.StructuredOutput `json:"document,omitempty"`
	RawLog              string                   `json:"raw_log,omitempty"`
}

func compactQualityHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (corpus compactQualityCorpus) fixture(mechanism, variant string) ([]byte, error) {
	sourceVariant := variant
	if variant == "unchanged" || variant == "harmless" || variant == "vulnerable" {
		sourceVariant = "vulnerable"
	}
	data, ok := corpus.files[mechanism+"/"+sourceVariant+"/fixture.go"]
	if !ok {
		return nil, fmt.Errorf("fixture %s/%s is not in frozen corpus", mechanism, sourceVariant)
	}
	if variant == "harmless" {
		return append([]byte("// Harmless documentation edit.\n"), data...), nil
	}
	return bytes.Clone(data), nil
}

func compactQualityCommand(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2024-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2024-01-01T00:00:00Z")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("fixture command %s: %w: %s", args[0], err, output)
	}
	return strings.TrimSpace(string(output)), nil
}

func compactQualitySeed(mechanism string) structuredreview.Finding {
	f := structuredreview.Finding{Severity: "high", Kind: "defect", Confidence: 0.9, File: mechanism + ".go", StartLine: 7, EndLine: 7}
	if mechanism == "invoice" {
		f.Problem = "An existing invoice requested by another tenant returns its amount because lookup checks only ID, not ownership."
		f.Fix = "Reject cross-tenant reads; preserve own-tenant reads and rejection of missing invoices."
	} else {
		f.Problem = "When the exclusive cursor equals a value, the result repeats that boundary value instead of starting after it."
		f.Fix = "Return strictly greater values while preserving all greater records and empty/exhausted results."
	}
	return f
}

func compactQualityPrepare(t *testing.T, corpus compactQualityCorpus, work, mechanism, variant string) (string, string, []byte) {
	t.Helper()
	// t.TempDir here would embed the outcome-bearing subtest name in Pi's cwd.
	repo, err := os.MkdirTemp(work, "q-")
	require.NoError(t, err)
	for _, args := range [][]string{{"git", "init", "-q"}, {"git", "config", "user.name", "Fixture Reviewer"}, {"git", "config", "user.email", "reviewer@example.test"}, {"git", "config", "core.hooksPath", filepath.Join(repo, "no-hooks")}} {
		_, err := compactQualityCommand(t.Context(), repo, args...)
		require.NoError(t, err)
	}
	sourceVariant := "vulnerable"
	if variant == "safe" {
		sourceVariant = "safe"
	}
	initial, err := corpus.fixture(mechanism, sourceVariant)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repo, mechanism+".go"), initial, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Ledger helpers\n\nReadInvoice returns an amount only for the requesting tenant's invoice.\nMissing and other-tenant invoices are rejected; own-tenant reads succeed.\nAfter accepts ascending sorted integers and an exclusive cursor. It returns\nonly values strictly greater than the cursor, including all greater records.\nEmpty input and exhausted cursors return no records.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.test/ledger\n\ngo 1.27.3\n"), 0o600))
	_, err = compactQualityCommand(t.Context(), repo, "git", "add", ".")
	require.NoError(t, err)
	_, err = compactQualityCommand(t.Context(), repo, "git", "commit", "-q", "-m", "feat: initial helper")
	require.NoError(t, err)
	source, err := compactQualityCommand(t.Context(), repo, "git", "rev-parse", "HEAD")
	require.NoError(t, err)
	current, err := corpus.fixture(mechanism, variant)
	require.NoError(t, err)
	if variant == "fixed" || variant == "harmless" {
		require.NoError(t, os.WriteFile(filepath.Join(repo, mechanism+".go"), current, 0o600))
		_, err = compactQualityCommand(t.Context(), repo, "git", "add", ".")
		require.NoError(t, err)
		_, err = compactQualityCommand(t.Context(), repo, "git", "commit", "-q", "-m", "fix: update helper")
		require.NoError(t, err)
	}
	return repo, source, current
}

// Allowlist matches the paired quality harness. The consuming child has no
// inherited native config overrides, provider keys, extensions or user skills.
func compactQualityChildEnv(home string, inherited []string) ([]string, error) {
	var env []string
	for _, entry := range inherited {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "ROBOREV_QUALITY_") || strings.HasPrefix(key, "ROBOREV_COMPACT_") {
			env = append(env, entry)
			continue
		}
		if slices.Contains([]string{"PATH", "SystemRoot", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "TMPDIR", "TMP", "TEMP", "GOCACHE", "GOMODCACHE", "GOPATH"}, key) {
			env = append(env, entry)
		}
	}
	for _, dir := range []string{home, filepath.Join(home, ".pi/agent"), filepath.Join(home, "config"), filepath.Join(home, "cache"), filepath.Join(home, "roborev")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	return append(env, "HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_CACHE_HOME="+filepath.Join(home, "cache"), "ROBOREV_DATA_DIR="+filepath.Join(home, "roborev"), "GOMAXPROCS=2", "GOFLAGS=-p=2", "GOWORK=off", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(home, "gitconfig"), "GIT_TERMINAL_PROMPT=0"), nil
}

func compactQualityCopyAuth(path, home string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var providers map[string]jsontext.Value
	if err := json.Unmarshal(data, &providers); err != nil {
		return err
	}
	selected, ok := providers["zai"]
	if !ok {
		return fmt.Errorf("explicit Pi auth has no zai provider")
	}
	raw, err := json.Marshal(map[string]jsontext.Value{"zai": selected})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, ".pi/agent/auth.json"), raw, 0o600)
}

func compactQualityShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// The recorder forwards every argument to the selected Pi executable. It only
// copies the adapter's actual prompt file and schema argument for provenance;
// no model output or command text is executed by this harness.
func compactQualityPiRecorder(dir, realPi string) (string, error) {
	script := `#!/bin/sh
prev=""
for arg in "$@"; do
 case "$arg" in @*) cp -- "${arg#@}" PROMPT_CAPTURE ;; esac
 if [ "$prev" = "--json-schema" ]; then printf '%s' "$arg" > SCHEMA_CAPTURE; fi
 prev="$arg"
done
exec REAL_PI "$@"
`
	script = strings.ReplaceAll(script, "PROMPT_CAPTURE", compactQualityShellQuote(filepath.Join(dir, "prompt")))
	script = strings.ReplaceAll(script, "SCHEMA_CAPTURE", compactQualityShellQuote(filepath.Join(dir, "schema")))
	script = strings.ReplaceAll(script, "REAL_PI", compactQualityShellQuote(realPi))
	path := filepath.Join(dir, "capture-pi")
	return path, os.WriteFile(path, []byte(script), 0o700)
}

func TestCompactQualityEvaluation(t *testing.T) {
	mode := os.Getenv("ROBOREV_COMPACT_EVAL")
	if mode != "plan" && mode != "live" {
		t.Skip("explicit ROBOREV_COMPACT_EVAL=plan|live required")
	}
	root := os.Getenv("ROBOREV_QUALITY_HARNESS_DIR")
	require.NotEmpty(t, root, "explicit frozen fixture root required")
	corpusHead := os.Getenv("ROBOREV_QUALITY_HARNESS_HEAD")
	require.NotEmpty(t, corpusHead, "explicit expected corpus full SHA required")
	corpus, err := compactQualityLoadFrozenCorpus(t.Context(), root, corpusHead)
	require.NoError(t, err, "authenticate the recorded corpus before any model call")
	if os.Getenv("ROBOREV_COMPACT_CHILD") != "1" {
		executable, err := os.Executable()
		require.NoError(t, err)
		home := t.TempDir()
		env, err := compactQualityChildEnv(home, os.Environ())
		require.NoError(t, err)
		if mode == "live" {
			require.NoError(t, compactQualityCopyAuth(os.Getenv("ROBOREV_QUALITY_PI_AUTH"), home))
		}
		child := exec.CommandContext(t.Context(), executable, "-test.run=^TestCompactQualityEvaluation$", "-test.timeout=3h")
		child.Env = append(env, "ROBOREV_COMPACT_CHILD=1")
		output, err := child.CombinedOutput()
		require.NoError(t, err, "%s", output)
		observations, err := os.Open(os.Getenv("ROBOREV_COMPACT_REPORT"))
		require.NoError(t, err, "isolated child must produce observations, not skip")
		defer observations.Close()
		decoder := jsontext.NewDecoder(observations)
		count := 0
		for {
			var row compactQualityRow
			err := json.UnmarshalDecode(decoder, &row)
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			require.NotEmpty(t, row.StoredPromptHash, "every planned row must reach actual enqueue")
			if mode == "live" {
				require.NotEqual(t, "planned", row.Status, "live rows cannot silently skip invocation")
			}
			count++
		}
		require.Equal(t, 30, count, "2 mechanisms x 5 controls x 3 repeats, including failures")
		return
	}
	report := os.Getenv("ROBOREV_COMPACT_REPORT")
	require.NotEmpty(t, report)
	require.NoFileExists(t, report, "do not overwrite frozen observations")
	file, err := os.OpenFile(report, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer file.Close()
	head, err := compactQualityCommand(t.Context(), ".", "git", "rev-parse", "HEAD")
	require.NoError(t, err)
	harness, err := os.ReadFile("compact_quality_eval_test.go")
	require.NoError(t, err)
	rubric := corpus.files["rubric.json"]
	work, err := os.MkdirTemp("", "review-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(work)) })
	piVersion := "not invoked in plan mode"
	if version, err := compactQualityCommand(t.Context(), ".", "pi", "--version"); err == nil {
		piVersion = version
	}
	extensionHash := "unavailable"
	if extension, err := os.ReadFile(os.Getenv("ROBOREV_QUALITY_PI_SCHEMA_EXTENSION")); err == nil {
		extensionHash = compactQualityHash(extension)
	}
	for repeat := 1; repeat <= 3; repeat++ {
		for _, mechanism := range []string{"invoice", "cursor"} {
			for _, variant := range []string{"vulnerable", "fixed", "safe", "unchanged", "harmless"} {
				t.Run(fmt.Sprintf("%s/%s/%d", mechanism, variant, repeat), func(t *testing.T) {
					row := compactQualityRow{NormalizedPolicy: "fixture_only_agentic_false_existing_retry_allowance_exhausted", PiVersion: piVersion, ExtensionHash: extensionHash, Case: mechanism + "/" + variant, Repeat: repeat, ProductHead: head, HarnessHash: compactQualityHash(harness), CorpusHead: corpusHead, RubricHash: compactQualityHash(rubric), Agent: "pi", Model: "zai/glm-5.3-flash", Reasoning: "max", Status: "planned", Disposition: "unavailable"}
					defer func() {
						if t.Failed() && row.Status == "planned" {
							row.Status = "preparation_failed"
							row.Error = "preparation failed; see harness process output"
						}
						raw, encodeErr := json.Marshal(row)
						require.NoError(t, encodeErr)
						_, writeErr := fmt.Fprintln(file, string(raw))
						require.NoError(t, writeErr)
						require.NoError(t, file.Sync())
					}()
					repo, sourceSHA, current := compactQualityPrepare(t, corpus, work, mechanism, variant)
					row.FixtureHash = compactQualityHash(current)
					t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
					db := testutil.OpenTestDB(t)
					cfg := config.DefaultConfig()
					cfg.AllowUnsafeAgents = new(false)
					cfg.DefaultAgent = "pi"
					cfg.DefaultModel = row.Model
					cfg.MaxWorkers = 1
					cfg.Agent.Pi.LaunchArgs = []string{"--no-session", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files"}
					cfg.Agent.Pi.JSONSchemaExtension = os.Getenv("ROBOREV_QUALITY_PI_SCHEMA_EXTENSION")
					captureDir := t.TempDir()
					if mode == "live" {
						realPi, err := exec.LookPath("pi")
						require.NoError(t, err)
						cfg.PiCmd, err = compactQualityPiRecorder(captureDir, realPi)
						require.NoError(t, err)
					}

					if mode == "live" {
						require.FileExists(t, cfg.Agent.Pi.JSONSchemaExtension)
					}
					require.NoError(t, config.SaveGlobal(cfg))
					storedRepo, err := db.GetOrCreateRepo(repo)
					require.NoError(t, err)
					source, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: storedRepo.ID, GitRef: sourceSHA, Agent: "test"})
					require.NoError(t, err)
					claimed, err := db.ClaimJob("fixture")
					require.NoError(t, err)
					require.Equal(t, source.ID, claimed.ID)
					doc := structuredreview.Document{SchemaVersion: 3, Summary: "One actionable issue.", Verdict: "fail", Findings: []structuredreview.Finding{compactQualitySeed(mechanism)}}
					raw, err := json.Marshal(doc)
					require.NoError(t, err)
					require.NoError(t, db.CompleteJobResult(source.ID, "fixture", storage.ReviewCompletion{StructuredOutput: raw}))
					_, err = db.ExecContext(t.Context(), `UPDATE reviews SET uuid=? WHERE job_id=?`, uuid.MustParse("00000000-0000-4000-8000-000000000001"), source.ID)
					require.NoError(t, err)
					sourceReview, err := db.GetReviewByJobID(source.ID)
					require.NoError(t, err)
					source, err = db.GetJobByID(source.ID)
					require.NoError(t, err)
					server := daemon.NewServer(db, cfg, "")
					defer server.Close()
					httpServer := httptest.NewServer(server.Handler())
					defer httpServer.Close()
					previousAddr := serverAddr
					serverAddr = httpServer.URL
					defer func() { serverAddr = previousAddr }()
					command := &cobra.Command{}
					command.SetOut(io.Discard)
					command.SetErr(io.Discard)
					jobID, err := enqueueConsolidation(t.Context(), command, repo, []jobReview{{jobID: source.ID, job: source, review: sourceReview}}, "main", compactOptions{agentName: "pi", model: row.Model, reasoning: "max", quiet: true})
					require.NoError(t, err)
					job, err := db.GetJobByID(jobID)
					require.NoError(t, err)
					row.StoredPromptHash = compactQualityHash([]byte(job.Prompt))
					// Legacy CLI publishes metadata outside enqueueConsolidation. Candidate
					// already wrote its atomic manifest. This is the sole baseline adapter.
					metadata := filepath.Join(config.DataDir(), getCompactMetadataFilename(jobID))
					if _, err := os.Stat(metadata); os.IsNotExist(err) {
						require.NoError(t, writeCompactMetadata(jobID, []int64{source.ID}))
					}
					// Both arms use an explicit fixture-only read-only policy. This is
					// normalized evaluation configuration, not baseline permission evidence.
					// The existing worker permits 3 retries; exhaust that allowance before
					// its FIRST attempt so the frozen invocation budget is one call.
					_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET retry_count=3, agentic=0 WHERE id=?`, jobID)
					require.NoError(t, err)
					job, err = db.GetJobByID(jobID)
					require.NoError(t, err)
					row.RetryCount = job.RetryCount
					if mode == "plan" {
						return
					}
					pool := daemon.NewWorkerPool(db, daemon.NewStaticConfig(cfg), 1, server.Broadcaster(), nil, nil)
					pool.Start()
					ctx, cancel := context.WithTimeout(t.Context(), time.Duration(cfg.JobTimeoutMinutes)*time.Minute)
					_, waitErr := waitForJobCompletion(ctx, httpServer.URL, jobID, nil)
					cancel()
					pool.Stop()
					job, err = db.GetJobByID(jobID)
					require.NoError(t, err)
					row.Status = string(job.Status)
					row.RetryCount = job.RetryCount
					require.NoError(t, db.QueryRowContext(t.Context(), `SELECT agent_invoked FROM review_jobs WHERE id=?`, jobID).Scan(&row.AgentInvoked))
					if waitErr != nil {
						row.Error = waitErr.Error()
					}
					if captured, err := os.ReadFile(filepath.Join(captureDir, "prompt")); err == nil {
						row.EffectivePrompt = string(captured)
						row.EffectivePromptHash = compactQualityHash(captured)
					} else if row.AgentInvoked {
						row.Status = "instrumentation_failed"
						row.Error += "; actual prompt capture unavailable"
					}
					if captured, err := os.ReadFile(filepath.Join(captureDir, "schema")); err == nil {
						row.EffectiveSchema = string(captured)
						row.EffectiveSchemaHash = compactQualityHash(captured)
					} else if row.AgentInvoked {
						row.Status = "instrumentation_failed"
						row.Error += "; actual schema capture unavailable"
					}
					if log, err := daemon.ReadJobLog(jobID); err == nil {
						row.RawLog = string(log)
					}
					result, resultErr := db.GetReviewByJobID(jobID)
					if resultErr == nil {
						row.Output = result.Output
						row.Document = result.StructuredOutput
						if encoded, err := json.Marshal(result.StructuredOutput); err == nil {
							if document, err := structuredreview.Decode(encoded); err == nil {
								row.Findings = len(document.Findings)
								if strings.HasPrefix(document.Summary, "<!-- roborev:compact-audit:v1 -->\n") {
									if index := strings.LastIndex(document.Summary, "### F1 — "); index >= 0 {
										row.Disposition, _, _ = strings.Cut(document.Summary[index+len("### F1 — "):], "\n")
									}
								}
							}
						}
					}
					sourceReview, err = db.GetReviewByJobID(source.ID)
					require.NoError(t, err)
					row.SourceClosed = sourceReview.Closed
				})
			}
		}
	}
}

func TestCompactQualityIsolationUsesOnlyExplicitAuth(t *testing.T) {
	home := t.TempDir()
	env, err := compactQualityChildEnv(home, []string{"PATH=/bin", "CODEX_HOME=private", "CLAUDE_CONFIG_DIR=private", "PI_CODING_AGENT_DIR=private", "UNRELATED_SECRET=secret", "ROBOREV_COMPACT_EVAL=live"})
	require.NoError(t, err)
	a := assert.New(t)
	a.NotContains(env, "CODEX_HOME=private")
	a.NotContains(env, "CLAUDE_CONFIG_DIR=private")
	a.NotContains(env, "PI_CODING_AGENT_DIR=private")
	a.NotContains(env, "UNRELATED_SECRET=secret")
	a.Contains(env, "ROBOREV_COMPACT_EVAL=live", "the consuming child must run rather than skip")
	source := filepath.Join(t.TempDir(), "auth.json")
	require.NoError(t, os.WriteFile(source, []byte(`{"zai":{"type":"api_key","key":"synthetic"},"other":{"key":"not-copied"}}`), 0o600))
	require.NoError(t, compactQualityCopyAuth(source, home))
	data, err := os.ReadFile(filepath.Join(home, ".pi/agent/auth.json"))
	require.NoError(t, err)
	a.JSONEq(`{"zai":{"type":"api_key","key":"synthetic"}}`, string(data))
}

func TestCompactQualityRecorderPreservesActualInputs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("opt-in recorder uses POSIX forwarding")
	}
	dir := t.TempDir()
	actual := filepath.Join(dir, "fake-pi")
	require.NoError(t, os.WriteFile(actual, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	recorder, err := compactQualityPiRecorder(dir, actual)
	require.NoError(t, err)
	prompt := filepath.Join(dir, "prompt ' $(ignored).md")
	body := "Original complete prompt\nUnicode: 租户\n"
	schema := `{"type":"object","properties":{"decisions":{"type":"array"}}}`
	require.NoError(t, os.WriteFile(prompt, []byte(body), 0o600))
	command := exec.CommandContext(t.Context(), recorder, "--json-schema", schema, "@"+prompt)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	captured, err := os.ReadFile(filepath.Join(dir, "prompt"))
	require.NoError(t, err)
	assert.Equal(t, body, string(captured))
	captured, err = os.ReadFile(filepath.Join(dir, "schema"))
	require.NoError(t, err)
	assert.Equal(t, schema, string(captured))
}

func TestCompactQualityFrozenCorpusRejectsChangedInputs(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "internal/review/testdata/quality")
	for _, mechanism := range []string{"invoice", "cursor"} {
		for _, variant := range []string{"vulnerable", "fixed", "safe"} {
			path := filepath.Join(base, mechanism, variant, "fixture.go")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte("package fixture\n"), 0o600))
		}
	}
	rubric := filepath.Join(base, "rubric.json")
	require.NoError(t, os.WriteFile(rubric, []byte(`{"version":1}`), 0o600))
	for _, args := range [][]string{{"git", "init", "-q"}, {"git", "config", "user.name", "Fixture Reviewer"}, {"git", "config", "user.email", "reviewer@example.test"}, {"git", "config", "core.hooksPath", filepath.Join(root, "no-hooks")}, {"git", "add", "."}, {"git", "commit", "-qm", "test: freeze corpus"}} {
		_, err := compactQualityCommand(t.Context(), root, args...)
		require.NoError(t, err)
	}
	head, err := compactQualityCommand(t.Context(), root, "git", "rev-parse", "HEAD")
	require.NoError(t, err)
	corpus, err := compactQualityLoadFrozenCorpus(t.Context(), root, head)
	require.NoError(t, err)
	fixture := filepath.Join(base, "invoice/fixed/fixture.go")
	for _, path := range []string{fixture, rubric} {
		original, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, []byte("changed"), 0o600))
		_, err = compactQualityLoadFrozenCorpus(t.Context(), root, head)
		require.ErrorContains(t, err, "differs from frozen")
		require.NoError(t, os.Remove(path))
		_, err = compactQualityLoadFrozenCorpus(t.Context(), root, head)
		require.Error(t, err, "deleted input must fail before model invocation")
		require.NoError(t, os.WriteFile(path, original, 0o600))
	}
	_, err = compactQualityLoadFrozenCorpus(t.Context(), root, strings.Repeat("0", 40))
	require.Error(t, err, "unknown frozen revision cannot authenticate inputs")
	// The already authenticated snapshot never rereads mutable worktree bytes.
	require.NoError(t, os.WriteFile(fixture, []byte("changed again"), 0o600))
	got, err := corpus.fixture("invoice", "fixed")
	require.NoError(t, err)
	assert.Equal(t, "package fixture\n", string(got))
	got[0] = 'X'
	got, err = corpus.fixture("invoice", "fixed")
	require.NoError(t, err)
	assert.Equal(t, "package fixture\n", string(got), "consumers cannot mutate the frozen snapshot")
}

func TestCompactQualityCheckoutPathIsOpaque(t *testing.T) {
	work, err := os.MkdirTemp("", "review-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(work)) })
	corpus := compactQualityCorpus{files: map[string][]byte{
		"invoice/vulnerable/fixture.go": []byte("package fixture\n"),
		"invoice/fixed/fixture.go":      []byte("package fixture\nconst Updated = true\n"),
	}}
	t.Run("invoice/fixed/1", func(t *testing.T) {
		repo, _, _ := compactQualityPrepare(t, corpus, work, "invoice", "fixed")
		assert.Equal(t, work, filepath.Dir(repo))
		for _, label := range []string{"invoice", "vulnerable", "fixed", "safe", "unchanged", "harmless"} {
			assert.NotContains(t, repo, label, "model-visible cwd must not reveal row labels")
		}
	})
}
