package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestEvidenceWorkerPortableRetryAndFrozenMembers(t *testing.T) {
	for _, prebuilt := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			name := "rebuilt"
			if prebuilt {
				name = "prebuilt"
			}
			if enabled {
				name += "-enabled"
			} else {
				name += "-disabled"
			}
			t.Run(name, func(t *testing.T) {
				tc := newWorkerTestContext(t, 1)
				cfg := config.DefaultConfig()
				cfg.IsolateReviews = true
				if prebuilt {
					cfg.DefaultMaxPromptSize = 1
				}
				tc.reconfigurePool(cfg)
				tc.GitRepo.CommitFile("rubric.md", "Supplied rubric preserved.", "rubric")
				// The live type disagrees with the frozen member to exercise both directions.
				rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{"citations": {Template: "rubric.md", BinaryEvidence: !enabled}}}}
				require.NoError(t, config.SaveRepoConfigTo(filepath.Join(tc.TmpDir, ".roborev.toml"), rc))
				const payload = "%PDF-1.7\n\x00committed draft"
				sha := tc.GitRepo.CommitFile("draft.pdf", payload, "draft")
				settings := config.ReviewSettings{BinaryEvidence: enabled}
				var stored string
				var err error
				if prebuilt {
					stored, err = prompt.NewBuilder(nil).ForRepo(tc.TmpDir, tc.Repo.ID).WithReviewSettings(settings).BuildWithAdditionalContext(sha, 0, "test", "citations", "", "PR discussion preserved.")
					require.NoError(t, err)
				}
				raw, err := json.Marshal(config.ResolvedMember{Name: "citations", Instructions: "Member instruction preserved.", ReviewSettings: settings})
				require.NoError(t, err)
				calls := 0
				agent.RegisterForTest(t, &evidenceTestAgent{FakeAgent: &agent.FakeAgent{NameStr: "evidence-capture", ReviewFn: func(_ context.Context, repoPath, _, text string, _ io.Writer) (string, error) {
					calls++
					assert.NotEqual(t, tc.TmpDir, repoPath, "evidence belongs in the agent checkout")
					if prebuilt {
						promptFiles, err := filepath.Glob(filepath.Join(repoPath, ".roborev", "roborev-snapshot-*", "prompt.md"))
						require.NoError(t, err)
						require.Len(t, promptFiles, 1)
						require.Contains(t, text, strconv.Quote(promptFiles[0]))
						complete, err := os.ReadFile(promptFiles[0])
						require.NoError(t, err)
						text = string(complete)
					}
					files, err := filepath.Glob(filepath.Join(repoPath, ".roborev", "roborev-snapshot-*", "*.pdf"))
					require.NoError(t, err)
					if enabled {
						require.Contains(t, text, "## Committed review evidence")
						require.Contains(t, text, "draft.pdf")
						require.Len(t, files, 1)
						data, err := os.ReadFile(files[0])
						require.NoError(t, err)
						assert.Equal(t, payload, string(data))
						assert.Contains(t, text, strconv.Quote(files[0]))
						assert.Equal(t, 1, strings.Count(text, "## Committed review evidence"))
					} else {
						assert.Empty(t, files)
						assert.NotContains(t, text, "## Committed review evidence")
					}
					assert.Equal(t, 1, strings.Count(text, "Member instruction preserved."))
					assert.Contains(t, text, "Supplied rubric preserved.")
					if prebuilt {
						assert.Contains(t, text, "PR discussion preserved.")
					}
					if calls == 1 {
						return "", errors.New("transient provider failure")
					}
					return string(testutil.ReviewFixtureJSON("No issues found.")), nil
				}}})
				job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: sha, JobType: storage.JobTypeReview, Agent: "evidence-capture", ReviewType: "citations", PanelRole: storage.PanelRoleMember, PanelName: "citations", PanelMemberName: "citations", PanelMemberConfigJSON: string(raw), Prompt: stored, PromptPrebuilt: prebuilt})
				require.NoError(t, err)
				for range 2 {
					claimed, err := tc.DB.ClaimJob(testWorkerID)
					require.NoError(t, err)
					require.NotNil(t, claimed)
					tc.Pool.processJob(testWorkerID, claimed)
					dirs, err := filepath.Glob(filepath.Join(tc.TmpDir, ".roborev", "roborev-snapshot-*"))
					require.NoError(t, err)
					assert.Empty(t, dirs)
				}
				tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
				require.Equal(t, 2, calls)
				got, err := tc.DB.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.NotContains(t, got.Prompt, tc.TmpDir)
				if enabled {
					assert.Contains(t, got.Prompt, "roborev-evidence:v1")
					assert.Contains(t, got.Prompt, "blob_oid")
				}
			})
		}
	}
}

func TestEvidenceDirtyWorkerRejectsBeforeLaunch(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	called := false
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "evidence-dirty", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		called = true
		return "", nil
	}})
	diff := "draft diff"
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: "dirty", JobType: storage.JobTypeDirty, DiffContent: diff, Agent: "evidence-dirty", PanelMemberConfigJSON: `{"binary_evidence":true}`})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	tc.Pool.processJob(testWorkerID, claimed)
	got, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.NotEqual(t, storage.JobStatusDone, got.Status)
	assert.Equal(t, 1, got.RetryCount)
	assert.False(t, called)
}

// Preserve schema capability through the worker's normal agent clone calls.
type evidenceTestAgent struct{ *agent.FakeAgent }

func (a *evidenceTestAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }
func (a *evidenceTestAgent) WithModel(string) agent.Agent                   { return a }
func (a *evidenceTestAgent) WithAgentic(bool) agent.Agent                   { return a }
func (a *evidenceTestAgent) ReviewWithSchema(ctx context.Context, repo, ref, text string, _ jsontext.Value, out io.Writer) (jsontext.Value, error) {
	raw, err := a.Review(ctx, repo, ref, text, out)
	return jsontext.Value(raw), err
}

func TestEvidenceCIPrebuiltPortableTrustedSettings(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	base := repo.CommitFile("rubric.md", "Trusted rubric preserved.", "rubric")
	head := repo.CommitFile("draft.pdf", "%PDF-1.7\n\x00draft", "draft")
	// Current checkout settings are deliberately disabled. CI uses its trusted
	// configuration and reads the rubric at the trusted ref, but evidence at head.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[review.types.citations]\ntemplate=\"rubric.md\"\nbinary_evidence=false\n"), 0o600))
	rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{"citations": {Template: "rubric.md", BinaryEvidence: true}}}}
	cfg := config.DefaultConfig()
	p := NewCIPoller(nil, NewStaticConfig(cfg), nil)
	stored, err := p.callBuildReviewPrompt(context.Background(), repo.Path(), base+".."+head, 0, 0, "test", "citations", "", "PR discussion preserved.", rc, base, cfg)
	require.NoError(t, err)
	assert.Contains(t, stored, "Trusted rubric preserved.")
	assert.Contains(t, stored, "PR discussion preserved.")
	assert.Contains(t, stored, "roborev-evidence:v1")
	assert.Contains(t, stored, "draft.pdf")
	assert.NotContains(t, stored, repo.Path())
	dirs, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*"))
	require.NoError(t, err)
	assert.Empty(t, dirs)
}

func TestEvidenceWorkerPrebuiltDiffRebindingSafe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable sentinel fixture")
	}
	for _, ranged := range []bool{false, true} {
		t.Run(fmt.Sprint("range=", ranged), func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			cfg := config.DefaultConfig()
			cfg.IsolateReviews = false
			tc.reconfigurePool(cfg)
			base := tc.GitRepo.CommitFiles(map[string]string{"rubric.md": "Review supplied bytes.", ".gitattributes": "draft.pdf diff=synthetic\n"}, "rubric")
			head := tc.GitRepo.CommitFiles(map[string]string{"draft.pdf": "%PDF-1.7\n\x00original", "draft.txt": "Original visible material\n"}, "Original review subject")
			rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{"citations": {Template: "rubric.md", BinaryEvidence: true}}}}
			require.NoError(t, config.SaveRepoConfigTo(filepath.Join(tc.TmpDir, ".roborev.toml"), rc))
			ref := head
			if ranged {
				ref = base + ".." + head
			}
			stored, err := prompt.NewBuilder(nil).ForRepo(tc.TmpDir, tc.Repo.ID).WithRepoConfig(rc, "").BuildWithAdditionalContext(ref, 0, "test", "citations", "", "Diff attachment: "+prompt.DiffFilePathPlaceholder)
			require.NoError(t, err)
			other := tc.GitRepo.CommitFiles(map[string]string{"draft.pdf": "%PDF-1.7\n\x00substituted", "draft.txt": "Substituted visible material\n"}, "replacement source")
			replacement := tc.GitRepo.Run("commit-tree", tc.GitRepo.Run("rev-parse", other+"^{tree}"), "-p", base, "-m", "Substituted review subject")
			tc.GitRepo.Run("replace", head, replacement)
			dir := t.TempDir()
			sentinel := filepath.Join(dir, "invoked")
			script := filepath.Join(dir, "converter")
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
			require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf invoked >> "+quote(sentinel)+"\nprintf 'Converted helper material\\n'\n"), 0o700))
			tc.GitRepo.Run("config", "diff.synthetic.textconv", quote(filepath.ToSlash(script)))
			tc.GitRepo.Run("config", "diff.external", quote(filepath.ToSlash(script)))
			called := false
			agent.RegisterForTest(t, &evidenceTestAgent{FakeAgent: &agent.FakeAgent{NameStr: "evidence-prebuilt-safe", ReviewFn: func(_ context.Context, repo, _, text string, _ io.Writer) (string, error) {
				called = true
				files, err := filepath.Glob(filepath.Join(repo, ".roborev", "roborev-snapshot-*", "*.diff"))
				require.NoError(t, err)
				require.Len(t, files, 1)
				require.Contains(t, text, files[0])
				data, err := os.ReadFile(files[0])
				require.NoError(t, err)
				assert.Contains(t, string(data), "+Original visible material")
				assert.NotContains(t, string(data), "Substituted")
				assert.NotContains(t, string(data), "Converted helper material")
				_, err = os.Stat(sentinel)
				require.ErrorIs(t, err, os.ErrNotExist)
				return string(testutil.ReviewFixtureJSON("No issues found.")), nil
			}}})
			job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: ref, JobType: storage.JobTypeReview, ReviewType: "citations", Agent: "evidence-prebuilt-safe", Prompt: stored, PromptPrebuilt: true})
			require.NoError(t, err)
			claimed, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			tc.Pool.processJob(testWorkerID, claimed)
			tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
			assert.True(t, called)
		})
	}
}
