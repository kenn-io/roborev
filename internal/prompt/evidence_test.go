package prompt

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/testutil"
)

func TestEvidenceIndexPinnedRawBytes(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	old := "%PDF-1.7\n\x00old bytes"
	base := repo.CommitFile("draft.pdf", old, "add evidence")
	newBytes := "%PDF-1.7\n\x00new bytes"
	head := repo.CommitFile("draft.pdf", newBytes, "update evidence")
	repo.CommitFile("draft.pdf", "unrelated current bytes", "current checkout")
	builder := NewBuilder(nil).ForRepo(repo.Path(), 0)
	index, err := builder.BuildEvidenceIndex(base+".."+head, nil)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal(1, index.Version)
	assert.Equal(git.ReviewEndpoints{Head: head, Bases: []string{base}}, index.Target)
	require.Len(t, index.Entries, 2)
	for i, want := range []string{old, newBytes} {
		entry := index.Entries[i]
		assert.Equal(fmt.Sprintf("%x", sha256.Sum256([]byte(want))), entry.SHA256)
		assert.Equal(int64(len(want)), entry.Bytes)
		assert.Equal("draft.pdf", entry.Path)
		assert.Equal("application/pdf", entry.MediaHint)
		assert.Equal(fmt.Sprintf("%x.pdf", sha256.Sum256([]byte(want))), entry.File)
		assert.Empty(entry.Omission)
	}
	assert.Equal("old parent 1", index.Entries[0].Side)
	assert.Equal("new", index.Entries[1].Side)
}

func TestEvidenceMaterializationLifecycle(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	data := "%PDF-1.7\n\x00same committed bytes"
	head := repo.CommitFiles(map[string]string{"first.pdf": data, "second.bin": data}, "duplicate evidence")
	agentRepo := testutil.NewTestRepoWithCommit(t)
	builder := NewBuilder(nil).ForRepo(repo.Path(), 0)
	index, err := builder.BuildEvidenceIndex(head, nil)
	require.NoError(t, err)
	section, cleanup, err := builder.MaterializeEvidence(index, SnapshotTarget{RepoPath: agentRepo.Path(), ConfigRepoPath: repo.Path()})
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	t.Cleanup(cleanup)
	assert := assert.New(t)
	assert.Contains(section, "first.pdf")
	assert.Contains(section, "second.bin")
	assert.Contains(section, fmt.Sprintf("%x", sha256.Sum256([]byte(data))))
	assert.NotContains(section, "agent read")
	files, err := filepath.Glob(filepath.Join(agentRepo.Path(), ".roborev", "roborev-snapshot-*", "*.pdf"))
	require.NoError(t, err)
	require.Len(t, files, 1, "identical bytes deduplicate in the attempt")
	bytes, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Equal(data, string(bytes))
	assert.Contains(section, strconv.Quote(files[0]))
	info, err := os.Stat(files[0])
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(os.FileMode(0o600), info.Mode().Perm())
	}
	ignored, err := git.CheckIgnoreNoIndex(agentRepo.Path(), files[0])
	require.NoError(t, err)
	assert.True(ignored)
	assert.True(snapshotIsActive(filepath.Dir(files[0])))
	assert.Empty(repo.Run("status", "--porcelain"))
	assert.Empty(agentRepo.Run("status", "--porcelain"))
	cleanup()
	_, err = os.Stat(filepath.Dir(files[0]))
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.False(snapshotIsActive(filepath.Dir(files[0])))
	// Retrying reconstructs the same identities in a fresh attempt directory.
	second, cleanup2, err := builder.MaterializeEvidence(index, SnapshotTarget{RepoPath: agentRepo.Path(), ConfigRepoPath: repo.Path()})
	require.NoError(t, err)
	t.Cleanup(cleanup2)
	assert.NotEqual(section, second)
	assert.Contains(second, index.Entries[0].SHA256)
}

func TestEvidenceMaterializationFailureCleansPartialAttempt(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	head := repo.CommitFiles(map[string]string{"a.bin": "\x00first", "z.bin": "\x00second"}, "evidence")
	builder := NewBuilder(nil).ForRepo(repo.Path(), 0)
	index, err := builder.BuildEvidenceIndex(head, nil)
	require.NoError(t, err)
	for _, tc := range []string{"hash", "length", "object", "cancel", "version"} {
		t.Run(tc, func(t *testing.T) {
			changed := index
			changed.Entries = append([]EvidenceEntry(nil), index.Entries...)
			current := builder
			switch tc {
			case "hash":
				changed.Entries[1].SHA256 = strings.Repeat("f", 64)
				changed.Entries[1].File = changed.Entries[1].SHA256 + ".bin"
			case "length":
				changed.Entries[1].Bytes++
			case "object":
				changed.Entries[1].BlobOID = strings.Repeat("f", 40)
			case "cancel":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				current = builder.WithContext(ctx)
			case "version":
				changed.Version++
			}
			section, cleanup, err := current.MaterializeEvidence(changed, SnapshotTarget{})
			require.Error(t, err)
			if tc == "hash" {
				require.ErrorContains(t, err, "committed evidence bytes do not match index")
			}
			assert.Empty(t, section)
			assert.Nil(t, cleanup)
			dirs, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*"))
			require.NoError(t, err)
			assert.Empty(t, dirs)
		})
	}
}

func TestEvidenceCompleteIndexParticipatesInPrepare(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("rubric.md", "Read the supplied evidence.", "rubric")
	head := repo.CommitFile("report.bin", "\x00binary", "evidence")
	rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{
		"citations": {Template: "rubric.md"},
	}}}
	baselineBuilder := NewBuilder(nil).ForRepo(repo.Path(), 0).WithRepoConfig(rc, "")
	baseline, err := baselineBuilder.BuildWithSnapshot(head, 0, "test", "citations", "", nil)
	require.NoError(t, err)
	require.Empty(t, baseline.FilePath)

	// The same review fits inline without evidence. Only the complete evidence
	// attachment should push the production build beyond this measured budget.
	budget := len(baseline.Prompt)
	builder := NewBuilderWithConfig(nil, &config.Config{DefaultMaxPromptSize: budget}).ForRepo(repo.Path(), 0).WithRepoConfig(rc, "")
	plain, err := builder.BuildWithSnapshot(head, 0, "test", "citations", "", nil)
	require.NoError(t, err)
	require.Empty(t, plain.FilePath)
	require.Equal(t, baseline.Prompt, plain.Prompt)

	prepared, err := builder.WithReviewSettings(config.ReviewSettings{BinaryEvidence: true}).BuildWithSnapshot(head, 0, "test", "citations", "", nil)
	require.NoError(t, err)
	require.NotNil(t, prepared.Cleanup)
	t.Cleanup(prepared.Cleanup)
	require.NotEmpty(t, prepared.FilePath)
	saved, err := os.ReadFile(prepared.FilePath)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Greater(len(saved), budget)
	assert.Contains(string(saved), "Read the supplied evidence.")
	assert.Contains(string(saved), "## Committed review evidence")
	assert.Contains(string(saved), "report.bin")
	files, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*", "*.bin"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Contains(string(saved), strconv.Quote(files[0]))
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Equal("\x00binary", string(data))
	prepared.Cleanup()
	dirs, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*"))
	require.NoError(t, err)
	assert.Empty(dirs)
}

func TestEvidencePortableFooterRebindsOnlyGeneratedAttachment(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	head := repo.CommitFile("document.pdf", "%PDF-1.7\n\x00evidence", "evidence")
	builder := NewBuilder(nil).ForRepo(repo.Path(), 0)
	index, err := builder.BuildEvidenceIndex(head, nil)
	require.NoError(t, err)
	// A user-supplied lookalike is not owned by assembly and must survive intact.
	body := "rubric and PR discussion\n" + evidenceFooterStart + "user text\n-->\n"
	stored, err := AppendEvidenceIndex(body, index)
	require.NoError(t, err)
	assert.NotContains(t, stored, repo.Path())
	assert.Contains(t, stored, index.Entries[0].SHA256)
	for range 2 {
		bound, cleanup, err := builder.BindEvidenceIndex(stored, index, SnapshotTarget{})
		require.NoError(t, err)
		require.NotNil(t, cleanup)
		assert.True(t, strings.HasPrefix(bound, body))
		assert.Equal(t, 1, strings.Count(bound, "## Committed review evidence"))
		assert.Equal(t, 1, strings.Count(bound, evidenceFooterStart), "only the supplied lookalike remains")
		cleanup()
	}
	for _, tc := range []string{"head", "entry", "version", "trailing", "missing"} {
		t.Run(tc, func(t *testing.T) {
			changed := index
			changed.Entries = append([]EvidenceEntry(nil), index.Entries...)
			text := stored
			switch tc {
			case "head":
				changed.Target.Head = strings.Repeat("f", 40)
			case "entry":
				changed.Entries[0].SHA256 = strings.Repeat("f", 64)
			case "version":
				changed.Version++
			case "trailing":
				text += "extra user text"
			case "missing":
				text = "no generated footer"
			}
			if tc == "head" || tc == "entry" || tc == "version" {
				payload, err := json.Marshal(changed)
				require.NoError(t, err)
				text = body + evidenceFooterStart + string(payload) + evidenceFooterEnd
			}
			// Reject a changed stored footer before attempting materialization.
			// The independent expected index remains valid in every case.
			_, err := evidencePromptBody(text, index)
			require.Error(t, err)
			_, cleanup, err := builder.BindEvidenceIndex(text, index, SnapshotTarget{})
			require.Error(t, err)
			assert.Nil(t, cleanup)
		})
	}
}

func TestEvidenceMaterializationRootConfinement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink creation privileges")
	}
	repo := testutil.NewTestRepoWithCommit(t)
	head := repo.CommitFile("image.bin", "\x89PNG\r\n\x1a\n\x00committed", "image evidence")
	builder := NewBuilder(nil).ForRepo(repo.Path(), 0)
	index, err := builder.BuildEvidenceIndex(head, nil)
	require.NoError(t, err)
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(repo.Path(), ".roborev")))
	_, cleanup, err := builder.MaterializeEvidence(index, SnapshotTarget{})
	require.Error(t, err)
	assert.Nil(t, cleanup)
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Empty(t, entries)
	require.NoError(t, os.Remove(filepath.Join(repo.Path(), ".roborev")))
	// A symlink in the checkout's own path is allowed; only snapshot-relative
	// components must be link-free. This also exercises macOS's /tmp alias.
	alias := filepath.Join(t.TempDir(), "checkout")
	require.NoError(t, os.Symlink(repo.Path(), alias))
	section, cleanup, err := builder.MaterializeEvidence(index, SnapshotTarget{RepoPath: alias})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.Contains(t, section, ".png")
	assert.Contains(t, section, alias)
}

// Path is an arbitrary identity label. Changing it must never affect the
// destination name or the bytes written in the private attempt directory.
func FuzzEvidenceMaterializationPaths(f *testing.F) {
	for _, path := range []string{
		"report.pdf", "../../escape.pdf", "/absolute/path", "C:\\outside\\file", "quote\"\t\n世界.pdf", "\x00", "\xff",
		"draft-\xff.pdf", "draft-\xfe.pdf", `draft-\xff.pdf`, "draft-\ufffd.pdf",
	} {
		f.Add(path)
	}
	repo := f.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		require.NoError(f, err, "%s", out)
	}
	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.invalid")
	data := "%PDF-1.7\n\x00pinned"
	require.NoError(f, os.WriteFile(filepath.Join(repo, "source.pdf"), []byte(data), 0o600))
	run("add", ".")
	run("commit", "-m", "synthetic evidence")
	builder := NewBuilder(nil).ForRepo(repo, 0)
	index, err := builder.BuildEvidenceIndex("HEAD", nil)
	require.NoError(f, err)
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(data)))
	f.Fuzz(func(t *testing.T, path string) {
		changed := index
		changed.Entries = append([]EvidenceEntry(nil), index.Entries...)
		changed.Entries[0].Path = path
		if !utf8.ValidString(path) {
			changed.Entries[0].Path = strconv.Quote(path)
			changed.Entries[0].PathBytes = []byte(path)
		}
		stored, err := AppendEvidenceIndex("rubric and PR discussion", changed)
		require.NoError(t, err)
		section, cleanup, err := builder.BindEvidenceIndex(stored, changed, SnapshotTarget{})
		require.NoError(t, err)
		t.Cleanup(cleanup)
		dirs, err := filepath.Glob(filepath.Join(repo, ".roborev", "roborev-snapshot-*"))
		require.NoError(t, err)
		require.Len(t, dirs, 1)
		files, err := os.ReadDir(dirs[0])
		require.NoError(t, err)
		assert.Len(t, files, 3, "marker, index, and one content-named blob")
		portable, err := os.ReadFile(filepath.Join(dirs[0], "evidence.json"))
		require.NoError(t, err)
		// Decode the actual file independently of the production identity codec.
		var wire struct {
			Entries []struct {
				Path      string `json:"path"`
				PathBytes []byte `json:"path_bytes"`
			} `json:"entries"`
		}
		require.NoError(t, json.Unmarshal(portable, &wire))
		require.Len(t, wire.Entries, 1)
		entry := wire.Entries[0]
		if utf8.ValidString(path) {
			assert.Equal(t, path, entry.Path)
			assert.Nil(t, entry.PathBytes)
		} else {
			assert.Equal(t, strconv.Quote(path), entry.Path)
			assert.Equal(t, []byte(path), entry.PathBytes)
		}
		got, err := os.ReadFile(filepath.Join(dirs[0], sum+".pdf"))
		require.NoError(t, err)
		assert.Equal(t, data, string(got))
		assert.Contains(t, section, strconv.Quote(path))
	})
}

func TestEvidenceReviewAssemblyAndTransport(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("rubric.md", "Preserve supplied rubric and discussion.", "rubric")
	head := repo.CommitFile("draft.pdf", "%PDF-1.7\n\x00committed", "draft")
	rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{"citations": {Template: "rubric.md", BinaryEvidence: true}, "plain": {Template: "rubric.md"}}}}
	for _, size := range []int{1, 100000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			b := NewBuilderWithConfig(nil, &config.Config{DefaultMaxPromptSize: size}).ForRepo(repo.Path(), 0).WithRepoConfig(rc, "")
			stored, err := b.BuildWithAdditionalContext(head, 0, "test", "citations", "", "PR discussion preserved.")
			require.NoError(t, err)
			assert.Contains(t, stored, evidenceFooterStart)
			assert.NotContains(t, stored, repo.Path())
			bound, err := b.BindReviewEvidence(stored, head, "citations", nil, SnapshotTarget{}, "\nMember instructions preserved.")
			require.NoError(t, err)
			require.NotNil(t, bound.Cleanup)
			t.Cleanup(bound.Cleanup)
			assert.Contains(t, bound.StoredPrompt, "Member instructions preserved.")
			assert.NotContains(t, bound.StoredPrompt, repo.Path())
			assert.Equal(t, 1, strings.Count(bound.Prompt, "## Committed review evidence"))
			prepared, err := b.Prepare(bound.Prompt, SnapshotTarget{})
			require.NoError(t, err)
			if prepared.Cleanup != nil {
				t.Cleanup(prepared.Cleanup)
			}
			if size == 1 {
				data, err := os.ReadFile(prepared.FilePath)
				require.NoError(t, err)
				assert.Equal(t, bound.Prompt, string(data))
			}
			result, err := b.BuildWithSnapshot(head, 0, "test", "citations", "", nil)
			require.NoError(t, err)
			require.NotNil(t, result.Cleanup)
			t.Cleanup(result.Cleanup)
			assert.Contains(t, result.StoredPrompt, evidenceFooterStart)
			assert.NotContains(t, result.StoredPrompt, repo.Path())
			// Explicit off is an override even when the live type enables evidence.
			off := b.WithReviewSettings(config.ReviewSettings{})
			disabled, err := off.BuildWithSnapshot(head, 0, "test", "citations", "", nil)
			require.NoError(t, err)
			if disabled.Cleanup != nil {
				t.Cleanup(disabled.Cleanup)
			}
			assert.Empty(t, disabled.StoredPrompt)
			plain, err := b.Build(head, 0, "test", "plain", "")
			require.NoError(t, err)
			assert.NotContains(t, plain, evidenceFooterStart)
			// A copied footer cannot turn evidence on for an unconfigured member.
			_, err = b.BindReviewEvidence(stored, head, "plain", nil, SnapshotTarget{})
			require.Error(t, err)
		})
	}
}

func TestEvidenceDirtyRefusal(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("rubric.md", "Review draft.", "rubric")
	rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{"citations": {Template: "rubric.md"}}}}
	for _, settings := range []config.ReviewSettings{{BinaryEvidence: true}, {EvidenceManifest: "evidence.toml"}} {
		b := NewBuilder(nil).ForRepo(repo.Path(), 0).WithRepoConfig(rc, "").WithReviewSettings(settings)
		_, err := b.BuildDirty("draft", 0, "test", "citations", "")
		require.ErrorContains(t, err, "committed")
	}
	b := NewBuilder(nil).ForRepo(repo.Path(), 0).WithRepoConfig(rc, "").WithReviewSettings(config.ReviewSettings{})
	_, err := b.BuildDirty("draft", 0, "test", "citations", "")
	require.NoError(t, err)
}

func TestEvidenceSnapshotExplicitExclusions(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("rubric.md", "Review committed draft.", "rubric")
	head := repo.CommitFiles(map[string]string{"included.pdf": "%PDF-1.7\n\x00include", "excluded.pdf": "%PDF-1.7\n\x00exclude"}, "drafts")
	rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{"citations": {Template: "rubric.md", BinaryEvidence: true}}}}
	b := NewBuilder(nil).ForRepo(repo.Path(), 0).WithRepoConfig(rc, "")
	result, err := b.BuildWithSnapshot(head, 0, "test", "citations", "", []string{"excluded.pdf"})
	require.NoError(t, err)
	require.NotNil(t, result.Cleanup)
	t.Cleanup(result.Cleanup)
	assert.NotContains(t, result.Prompt, "excluded.pdf")
	assert.NotContains(t, result.StoredPrompt, "excluded.pdf")
	assert.Contains(t, result.Prompt, "included.pdf")
}

func TestEvidenceRequiredManifestNotSilentlyIgnored(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	head := repo.CommitFile("rubric.md", "Review draft.", "rubric")
	rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{"citations": {Template: "rubric.md", EvidenceManifest: "missing.toml"}}}}
	b := NewBuilder(nil).ForRepo(repo.Path(), 0).WithRepoConfig(rc, "")
	_, err := b.Build(head, 0, "test", "citations", "")
	require.ErrorContains(t, err, "manifest")
	dirs, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*"))
	require.NoError(t, err)
	assert.Empty(t, dirs)
}

func TestEvidenceBuilderOriginalReplacementObjects(t *testing.T) {
	for _, ranged := range []bool{false, true} {
		for _, replacementKind := range []string{"commit", "blobs"} {
			t.Run(fmt.Sprintf("range=%t/%s", ranged, replacementKind), func(t *testing.T) {
				repo := testutil.NewTestRepoWithCommit(t)
				base := repo.CommitFile("rubric.md", "Review the supplied material.", "rubric")
				const original = "%PDF-1.7\n\x00original offered bytes"
				head := repo.CommitFiles(map[string]string{"draft.pdf": original, "draft.txt": "Original visible material\n", "go.mod": "module example.com/synthetic\n\ngo 1.24.0\n", "main.go": "package main\n"}, "Original review subject")
				originalPDF := repo.Run("rev-parse", head+":draft.pdf")
				originalText := repo.Run("rev-parse", head+":draft.txt")
				originalMod := repo.Run("rev-parse", head+":go.mod")
				originalRubric := repo.Run("rev-parse", head+":rubric.md")
				other := repo.CommitFiles(map[string]string{"draft.pdf": "%PDF-1.7\n\x00substituted bytes", "draft.txt": "Substituted visible material\n", "go.mod": "module example.com/synthetic\n\ngo 1.25.0\n", "rubric.md": "Substituted rubric"}, "replacement source")
				if replacementKind == "commit" {
					replacement := repo.Run("commit-tree", repo.Run("rev-parse", other+"^{tree}"), "-p", base, "-m", "Substituted review subject")
					repo.Run("replace", head, replacement)
				} else {
					repo.Run("replace", originalPDF, repo.Run("rev-parse", other+":draft.pdf"))
					repo.Run("replace", originalText, repo.Run("rev-parse", other+":draft.txt"))
					repo.Run("replace", originalMod, repo.Run("rev-parse", other+":go.mod"))
					repo.Run("replace", originalRubric, repo.Run("rev-parse", other+":rubric.md"))
				}
				ref := head
				if ranged {
					ref = base + ".." + head
				}
				rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{"citations": {Template: "rubric.md", BinaryEvidence: true}}}}
				b := NewBuilder(nil).ForRepo(repo.Path(), 0).WithRepoConfig(rc, head)
				result, err := b.BuildWithSnapshot(ref, 0, "test", "citations", "", nil)
				require.NoError(t, err)
				require.NotNil(t, result.Cleanup)
				t.Cleanup(result.Cleanup)
				assert := assert.New(t)
				assert.Contains(result.StoredPrompt, "Original review subject")
				assert.Contains(result.StoredPrompt, "Go 1.24.0")
				assert.NotContains(result.StoredPrompt, "Go 1.25.0")
				assert.Contains(result.StoredPrompt, "+Original visible material")
				assert.NotContains(result.StoredPrompt, "Substituted")
				index, err := b.BuildEvidenceIndex(ref, nil)
				require.NoError(t, err)
				_, err = evidencePromptBody(result.StoredPrompt, index)
				require.NoError(t, err)
				require.Len(t, index.Entries, 1)
				assert.Equal(originalPDF, index.Entries[0].BlobOID)
				assert.Equal(fmt.Sprintf("%x", sha256.Sum256([]byte(original))), index.Entries[0].SHA256)
				files, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*", index.Entries[0].File))
				require.NoError(t, err)
				require.Len(t, files, 1)
				data, err := os.ReadFile(files[0])
				require.NoError(t, err)
				assert.Equal(original, string(data))
				// The fixture exercises native replacement behavior on the ordinary path.
				ordinary, err := b.WithReviewSettings(config.ReviewSettings{}).Build(ref, 0, "test", "citations", "")
				require.NoError(t, err)
				assert.Contains(ordinary, "+Substituted visible material")
			})
		}
	}
}

func TestEvidenceBuilderDoesNotExecuteDiffHelpers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable sentinel fixture")
	}
	for _, ranged := range []bool{false, true} {
		for _, helper := range []string{"textconv", "external"} {
			t.Run(fmt.Sprintf("range=%t/%s", ranged, helper), func(t *testing.T) {
				repo := testutil.NewTestRepoWithCommit(t)
				base := repo.CommitFiles(map[string]string{"rubric.md": "Review committed bytes.", ".gitattributes": "draft.pdf diff=synthetic\n"}, "rubric and attributes")
				head := repo.CommitFile("draft.pdf", "%PDF-1.7\n\x00original bytes", "draft")
				dir := t.TempDir()
				sentinel := filepath.Join(dir, "invoked")
				script := filepath.Join(dir, "converter")
				quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
				require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf invoked >> "+quote(sentinel)+"\nprintf 'Converted helper material\\n'\n"), 0o700))
				if helper == "textconv" {
					repo.Run("config", "diff.synthetic.textconv", quote(filepath.ToSlash(script)))
				} else {
					repo.Run("config", "diff.external", quote(filepath.ToSlash(script)))
				}
				ref := head
				if ranged {
					ref = base + ".." + head
				}
				rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{"citations": {Template: "rubric.md", BinaryEvidence: true}}}}
				b := NewBuilder(nil).ForRepo(repo.Path(), 0).WithRepoConfig(rc, "")
				result, err := b.BuildWithSnapshot(ref, 0, "test", "citations", "", nil)
				require.NoError(t, err)
				if result.Cleanup != nil {
					t.Cleanup(result.Cleanup)
				}
				assert.NotContains(t, result.StoredPrompt, "Converted helper material")
				assert.Contains(t, result.StoredPrompt, "Binary files")
				_, err = os.Stat(sentinel)
				require.ErrorIs(t, err, os.ErrNotExist)
				// Demonstrate that the configured helper really executes on native
				// presentation commands (show does not enable external diff by default).
				if helper == "textconv" || ranged {
					_, err = b.WithReviewSettings(config.ReviewSettings{}).Build(ref, 0, "test", "citations", "")
					require.NoError(t, err)
					_, err = os.Stat(sentinel)
					require.NoError(t, err)
				}
			})
		}
	}
}

func TestEvidenceRootInclusiveRangeMaterialization(t *testing.T) {
	repo := testutil.NewTestRepo(t)
	const payload = "%PDF-1.7\n\x00committed root-inclusive draft"
	root := repo.CommitFiles(map[string]string{
		"rubric.md": "Review the committed draft.", "document.pdf": payload,
	}, "root evidence")
	head := repo.CommitFile("note.txt", "Later endpoint.", "endpoint")
	blobOID := repo.RevParse(root + ":document.pdf")
	repo.CommitFile("document.pdf", "unrelated working checkout bytes", "current")
	rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{
		"citations": {Template: "rubric.md", BinaryEvidence: true},
	}}}
	builder := NewBuilder(nil).ForRepo(repo.Path(), 0).WithRepoConfig(rc, "")
	// The daemon normalizes a range including the root commit to this sentinel.
	result, err := builder.BuildWithSnapshot(git.EmptyTreeSHA+".."+head, 0, "test", "citations", "", nil)
	require.NoError(t, err)
	require.NotNil(t, result.Cleanup)
	t.Cleanup(result.Cleanup)
	assert := assert.New(t)
	assert.Contains(result.StoredPrompt, git.EmptyTreeSHA)
	assert.Contains(result.StoredPrompt, head)
	assert.Contains(result.StoredPrompt, blobOID)
	assert.Contains(result.StoredPrompt, fmt.Sprintf("%x", sha256.Sum256([]byte(payload))))
	assert.Contains(result.Prompt, "## Committed review evidence")
	files, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*", "*.pdf"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Contains(result.Prompt, strconv.Quote(files[0]))
	bytes, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Equal(payload, string(bytes))
	result.Cleanup()
	dirs, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*"))
	require.NoError(t, err)
	assert.Empty(dirs)
}

// Removing lossless path identities must fail at assembly, before any agent can
// read the committed binary. Object stdin avoids host filesystem constraints;
// the fixture names must still pass native Git index validation.
func TestEvidenceNonUTF8GitPaths(t *testing.T) {
	check := assert.New(t)
	repo := testutil.NewTestRepoWithCommit(t)
	base := repo.Run("rev-parse", "HEAD")
	runInput := func(input string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo.Path()
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return strings.TrimSpace(string(out))
	}
	data := "%PDF-1.7\n\x00complete committed bytes"
	oid := runInput(data, "hash-object", "-w", "--stdin")
	rubric := runInput("Review every committed draft.", "hash-object", "-w", "--stdin")
	paths := []string{"draft-\xff.pdf", "draft-\xfe.pdf", "draft-xff.pdf", "draft-\ufffd.pdf"}
	var treeInput strings.Builder
	for _, path := range paths {
		fmt.Fprintf(&treeInput, "100644 blob %s\t%s\x00", oid, path)
	}
	fmt.Fprintf(&treeInput, "100644 blob %s\trubric.md\x00", rubric)
	tree := runInput(treeInput.String(), "mktree", "-z")
	head := repo.Run("commit-tree", tree, "-p", base, "-m", "committed byte paths")
	rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{
		"citations": {Template: "rubric.md", BinaryEvidence: true},
	}}}
	builder := NewBuilder(nil).ForRepo(repo.Path(), 0).WithRepoConfig(rc, head)
	index, err := builder.BuildEvidenceIndex(head, nil)
	require.NoError(t, err)
	require.Len(t, index.Entries, len(paths))
	stored, err := AppendEvidenceIndex("rubric and PR discussion", index)
	require.NoError(t, err)

	// Inspect the transport independently of the production path codec.
	type pathIdentity struct {
		Path      string `json:"path"`
		PathBytes []byte `json:"path_bytes"`
	}
	checkIndex := func(payload string) {
		t.Helper()
		var wire struct {
			Entries []pathIdentity `json:"entries"`
		}
		require.NoError(t, json.Unmarshal([]byte(payload), &wire))
		require.Len(t, wire.Entries, len(paths))
		got := make(map[string]bool)
		for _, entry := range wire.Entries {
			path := entry.Path
			if entry.PathBytes != nil {
				path = string(entry.PathBytes)
				check.Equal(strconv.Quote(path), entry.Path)
				check.False(utf8.Valid(entry.PathBytes))
			} else {
				check.True(utf8.ValidString(path))
			}
			got[path] = true
		}
		check.Equal(map[string]bool{paths[0]: true, paths[1]: true, paths[2]: true, paths[3]: true}, got)
	}
	payload := stored[strings.LastIndex(stored, evidenceFooterStart)+len(evidenceFooterStart) : len(stored)-len(evidenceFooterEnd)]
	checkIndex(payload)
	independent, err := builder.BuildEvidenceIndex(head, nil)
	require.NoError(t, err)
	for _, mutation := range []string{"raw", "display", "missing raw", "other canonical path"} {
		t.Run(mutation, func(t *testing.T) {
			check := assert.New(t)
			changed := independent
			changed.Entries = append([]EvidenceEntry(nil), independent.Entries...)
			var entry *EvidenceEntry
			for i := range changed.Entries {
				if changed.Entries[i].PathBytes != nil {
					entry = &changed.Entries[i]
					break
				}
			}
			require.NotNil(t, entry)
			switch mutation {
			case "raw":
				entry.PathBytes = []byte("other-\xff.pdf")
			case "display":
				entry.Path = "different label"
			case "missing raw":
				entry.PathBytes = nil
			case "other canonical path":
				entry.PathBytes = []byte("other-\xff.pdf")
				entry.Path = `"other-\xff.pdf"`
			}
			// Bypass the producer's validation to model untrusted stored text.
			payload, err := json.Marshal(changed)
			require.NoError(t, err)
			corrupt := "rubric and PR discussion" + evidenceFooterStart + string(payload) + evidenceFooterEnd
			_, cleanup, err := builder.BindEvidenceIndex(corrupt, independent, SnapshotTarget{})
			require.Error(t, err)
			check.Nil(cleanup)
			dirs, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*"))
			require.NoError(t, err)
			check.Empty(dirs)
		})
	}
	section, remove, err := builder.BindEvidenceIndex(stored, independent, SnapshotTarget{})
	require.NoError(t, err)
	require.NotNil(t, remove)
	t.Cleanup(remove)
	for _, path := range paths {
		check.Contains(section, "Path: "+strconv.Quote(path)+"; change:")
	}
	remove()

	assembled, err := builder.BuildWithSnapshotTarget(head, 0, "test", "citations", "", nil, SnapshotTarget{})
	require.NoError(t, err)
	require.NotNil(t, assembled.Cleanup)
	t.Cleanup(assembled.Cleanup)
	checkRuntime := func(prompt string) {
		t.Helper()
		check := assert.New(t)
		check.Equal(1, strings.Count(prompt, "## Committed review evidence"))
		for _, path := range paths {
			check.Equal(1, strings.Count(prompt, "\n- Path: "+strconv.Quote(path)+"; change:"))
		}
		dirs, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*"))
		require.NoError(t, err)
		require.Len(t, dirs, 1)
		portable, err := os.ReadFile(filepath.Join(dirs[0], "evidence.json"))
		require.NoError(t, err)
		checkIndex(string(portable))
		files, err := os.ReadDir(dirs[0])
		require.NoError(t, err)
		check.Len(files, 3, "marker, portable index, and one deduplicated hash-named blob")
		sum := fmt.Sprintf("%x", sha256.Sum256([]byte(data)))
		got, err := os.ReadFile(filepath.Join(dirs[0], sum+".pdf"))
		require.NoError(t, err)
		check.Equal([]byte(data), got)
		for _, entry := range index.Entries {
			check.Equal(oid, entry.BlobOID)
			check.Equal(sum, entry.SHA256)
			check.Equal(int64(len(data)), entry.Bytes)
			check.Equal(sum+".pdf", entry.File)
		}
	}
	checkRuntime(assembled.Prompt)
	assembled.Cleanup()
	for range 2 {
		retry, err := builder.BindReviewEvidence(assembled.StoredPrompt, head, "citations", nil, SnapshotTarget{})
		require.NoError(t, err)
		require.NotNil(t, retry.Cleanup)
		t.Cleanup(retry.Cleanup)
		checkRuntime(retry.Prompt)
		check.Equal(assembled.StoredPrompt, retry.StoredPrompt)
		retry.Cleanup()
	}
	dirs, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*"))
	require.NoError(t, err)
	check.Empty(dirs)
	check.Equal(base, repo.Run("rev-parse", "HEAD"), "the reviewed object was never checked out")
	check.Empty(repo.Run("status", "--porcelain"))
}

// Accepting an alternate raw/display pair would make the portable identity
// ambiguous even when the blob bytes themselves are valid.
func TestEvidencePathIdentityValidation(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	head := repo.CommitFile("draft.pdf", "%PDF-1.7\n\x00raw bytes", "binary")
	builder := NewBuilder(nil).ForRepo(repo.Path(), 0)
	index, err := builder.BuildEvidenceIndex(head, nil)
	require.NoError(t, err)
	require.Len(t, index.Entries, 1)
	for _, tc := range []struct {
		name string
		path string
		raw  []byte
	}{
		{"missing raw", "draft-\xff.pdf", nil},
		{"valid UTF8 raw", "draft.pdf", []byte("draft.pdf")},
		{"empty raw", "", []byte{}},
		{"mismatched display", `"draft-\xfe.pdf"`, []byte("draft-\xff.pdf")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := index
			changed.Entries = append([]EvidenceEntry(nil), index.Entries...)
			changed.Entries[0].Path = tc.path
			changed.Entries[0].PathBytes = tc.raw
			_, err := AppendEvidenceIndex("body", changed)
			require.ErrorContains(t, err, "evidence path")
			_, cleanup, err := builder.MaterializeEvidence(changed, SnapshotTarget{})
			require.ErrorContains(t, err, "evidence path")
			assert.Nil(t, cleanup)
			dirs, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*"))
			require.NoError(t, err)
			assert.Empty(t, dirs)
		})
	}
}

// Ordinary version-1 stored footers remain decodable without a new field,
// migration or path reinterpretation.
func TestEvidenceOrdinaryPathJSONCompatibility(t *testing.T) {
	index := EvidenceIndex{Version: 1, Target: git.ReviewEndpoints{
		Head: strings.Repeat("a", 40), Bases: []string{strings.Repeat("b", 40)},
	}, Entries: []EvidenceEntry{{
		Path: "café\t\"世界.pdf", File: strings.Repeat("c", 64) + ".pdf",
		Kind: "A", Side: "new", BlobOID: strings.Repeat("d", 40),
		SHA256: strings.Repeat("c", 64), MediaHint: "application/pdf", Bytes: 4,
	}}}
	const legacy = `{"version":1,"target":{"head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bases":["bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"]},"entries":[{"path":"café\t\"世界.pdf","file":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc.pdf","kind":"A","side":"new","blob_oid":"dddddddddddddddddddddddddddddddddddddddd","sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","media_hint":"application/pdf","omission":"","bytes":4}]}`
	body, err := evidencePromptBody("ordinary body"+evidenceFooterStart+legacy+evidenceFooterEnd, index)
	require.NoError(t, err)
	assert.Equal(t, "ordinary body", body)
}
