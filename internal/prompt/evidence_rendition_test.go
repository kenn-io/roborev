package prompt

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

func TestEvidenceManifestIndexAndSnapshot(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	text := "First line.\r\nSecond line: café.\n"
	repo.CommitFiles(map[string]string{
		"rubric.md":           "Check supplied citations.",
		"evidence/source.txt": text,
		"manifest.toml":       manifestFixture("evidence/source.txt", text),
		"draft.md":            "Original draft.",
	}, "pin rendition")
	head := repo.CommitFiles(map[string]string{"draft.md": "Revised draft.", "other.bin": "\x00optional binary"}, "revise draft")
	repo.CommitFile("evidence/source.txt", "Unrelated current bytes.", "change checkout")
	agentRepo := testutil.NewTestRepoWithCommit(t)
	settings := config.ReviewSettings{EvidenceManifest: "manifest.toml"}
	rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{"citations": {Template: "rubric.md"}}}}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	for _, size := range []int{1, 100000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			b := NewBuilderWithConfig(nil, &config.Config{DefaultMaxPromptSize: size}).ForRepo(repo.Path(), 0).WithRepoConfig(rc, "").WithReviewSettings(settings)
			index, err := b.BuildEvidenceIndex(head, nil)
			require.NoError(t, err)
			require.Len(t, index.Entries, 1, "manifest-only settings must skip optional binary changes")
			assert.Equal(t, EvidenceEntry{
				Path: "evidence/source.txt", File: sum + ".txt", Kind: "rendition", Side: "head",
				BlobOID: repo.RevParse(head + ":evidence/source.txt"), SHA256: sum,
				Bytes: int64(len(text)), MediaHint: "text/plain; charset=utf-8",
				DocumentID: "source-a", TextPath: "evidence/source.txt",
				DeclaredSourceSHA256: strings.Repeat("a", 64),
				Provenance:           "docbank://vaults/V/documents/N/versions/C/renditions/A",
			}, index.Entries[0])
			body := "Rubric and discussion.\n" + evidenceFooterStart + "user text\n-->\n"
			stored, err := AppendEvidenceIndex(body, index)
			require.NoError(t, err)
			for range 2 {
				result, err := b.BindReviewEvidence(stored, head, "citations", nil, SnapshotTarget{RepoPath: agentRepo.Path(), ConfigRepoPath: repo.Path()})
				require.NoError(t, err)
				require.NotNil(t, result.Cleanup)
				assert := assert.New(t)
				assert.Equal(stored, result.StoredPrompt)
				assert.NotContains(result.StoredPrompt, repo.Path())
				assert.NotContains(result.StoredPrompt, agentRepo.Path())
				assert.Contains(result.Prompt, body)
				assert.Contains(result.Prompt, `declared source SHA256 (not verified): "`+strings.Repeat("a", 64)+`"`)
				assert.Contains(result.Prompt, `declared provenance (opaque, not verified): "docbank://vaults/V/documents/N/versions/C/renditions/A"`)
				files, err := filepath.Glob(filepath.Join(agentRepo.Path(), ".roborev", "roborev-snapshot-*", "*.txt"))
				require.NoError(t, err)
				require.Len(t, files, 1)
				payload, err := os.ReadFile(files[0])
				require.NoError(t, err)
				assert.Equal(text, string(payload))
				assert.Contains(result.Prompt, strconv.Quote(files[0]))
				info, err := os.Stat(files[0])
				require.NoError(t, err)
				if runtime.GOOS != "windows" {
					assert.Equal(os.FileMode(0o600), info.Mode().Perm())
				}
				prepared, err := b.Prepare(result.Prompt, SnapshotTarget{RepoPath: agentRepo.Path(), ConfigRepoPath: repo.Path()})
				require.NoError(t, err)
				if size == 1 {
					payload, err := os.ReadFile(prepared.FilePath)
					require.NoError(t, err)
					assert.Equal(result.Prompt, string(payload))
					prepared.Cleanup()
				} else {
					assert.Equal(result.Prompt, prepared.Prompt)
				}
				result.Cleanup()
				result.Cleanup()
				_, err = os.Stat(filepath.Dir(files[0]))
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			result, err := b.BuildWithSnapshotTarget(head, 0, "test", "citations", "", nil, SnapshotTarget{RepoPath: agentRepo.Path(), ConfigRepoPath: repo.Path()})
			require.NoError(t, err)
			require.NotNil(t, result.Cleanup)
			result.Cleanup()
			assert.Contains(t, result.StoredPrompt, sum)
		})
	}
}

func TestEvidenceManifestBindingRejectsMissingOrAlteredIdentities(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	head := repo.CommitFiles(map[string]string{"source.txt": "Text.", "manifest.toml": manifestFixture("source.txt", "Text.")}, "pin rendition")
	b := NewBuilder(nil).ForRepo(repo.Path(), 0).WithReviewSettings(config.ReviewSettings{EvidenceManifest: "manifest.toml"})
	index, err := b.BuildEvidenceIndex(head, nil)
	require.NoError(t, err)
	for _, field := range []string{"document", "source", "provenance", "missing"} {
		t.Run(field, func(t *testing.T) {
			changed := index
			changed.Entries = append([]EvidenceEntry(nil), index.Entries...)
			switch field {
			case "document":
				changed.Entries[0].DocumentID = "other"
			case "source":
				changed.Entries[0].DeclaredSourceSHA256 = strings.Repeat("b", 64)
			case "provenance":
				changed.Entries[0].Provenance = "different"
			case "missing":
				changed.Entries = nil
			}
			stored, err := AppendEvidenceIndex("Rubric.", changed)
			require.NoError(t, err)
			_, err = b.BindReviewEvidence(stored, head, "citations", nil, SnapshotTarget{})
			require.Error(t, err)
		})
	}
	dirs, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*"))
	require.NoError(t, err)
	assert.Empty(t, dirs)
}

func TestEvidenceManifestMixedMediaAndPartialFailure(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	text := "%PDF-1.7\nA UTF-8 rendition.\n"
	manifest := manifestFixture("source.pdf", text)
	manifest += strings.Replace(strings.TrimPrefix(manifest, "version = 1\n"), `id = "source-a"`, `id = "source-b"`, 1)
	head := repo.CommitFiles(map[string]string{
		".gitattributes": "*.pdf binary\n", "source.pdf": text,
		"manifest.toml": manifest, "invalid.txt": "\xff",
	}, "pin representations")
	b := NewBuilder(nil).ForRepo(repo.Path(), 0).WithReviewSettings(config.ReviewSettings{BinaryEvidence: true, EvidenceManifest: "manifest.toml"})
	index, err := b.BuildEvidenceIndex(head, nil)
	require.NoError(t, err)
	var renditions []EvidenceEntry
	for _, entry := range index.Entries {
		if entry.DocumentID != "" {
			renditions = append(renditions, entry)
		}
	}
	require.Len(t, renditions, 2)
	section, cleanup, err := b.MaterializeEvidence(index, SnapshotTarget{})
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	assert.Contains(t, section, `document: "source-a"`)
	assert.Contains(t, section, `document: "source-b"`)
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	files, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*", sum+".*"))
	require.NoError(t, err)
	assert.Len(t, files, 2, "the same raw bytes may have both PDF and text representations")
	cleanup()
	for _, failure := range []string{"duplicate object", "invalid UTF8", "filename", "length"} {
		t.Run(failure, func(t *testing.T) {
			changed := EvidenceIndex{Version: index.Version, Target: index.Target, Entries: append([]EvidenceEntry(nil), renditions...)}
			bad := &changed.Entries[1]
			switch failure {
			case "duplicate object":
				bad.BlobOID = repo.RevParse(head + ":invalid.txt")
			case "invalid UTF8":
				bad.BlobOID = repo.RevParse(head + ":invalid.txt")
				bad.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("\xff")))
				bad.File = bad.SHA256 + ".txt"
				bad.Bytes = 1
			case "filename":
				bad.File = "../outside.txt"
			case "length":
				bad.Bytes++
			}
			section, cleanup, err := b.MaterializeEvidence(changed, SnapshotTarget{})
			require.Error(t, err)
			if failure == "invalid UTF8" {
				require.ErrorContains(t, err, "UTF-8")
			}
			assert.Empty(t, section)
			assert.Nil(t, cleanup)
			dirs, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*"))
			require.NoError(t, err)
			assert.Empty(t, dirs, "later invalid entries must clean already written renditions")
		})
	}
}
