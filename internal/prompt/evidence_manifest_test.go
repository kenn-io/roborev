package prompt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

func manifestFixture(textPath, text string) string {
	return fmt.Sprintf(`version = 1
[[documents]]
id = "source-a"
text_path = %q
text_sha256 = "%x"
source_sha256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
provenance = "docbank://vaults/V/documents/N/versions/C/renditions/A"
`, textPath, sha256.Sum256([]byte(text)))
}

// Reading the working tree or skipping unchanged entries would lose the pinned
// rendition when only the draft changes.
func TestEvidenceManifestPinnedUnchangedRawBytes(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	text := "First line.\r\nSecond line: café.\n"
	manifest := manifestFixture("evidence/source.txt", text)
	repo.CommitFiles(map[string]string{
		"evidence/source.txt":    text,
		".roborev/evidence.toml": manifest,
		"draft.md":               "Original draft.",
	}, "add pinned rendition")
	head := repo.CommitFile("draft.md", "Revised draft.", "revise draft")
	repo.CommitFiles(map[string]string{
		"evidence/source.txt":    "Unrelated current payload.",
		".roborev/evidence.toml": "invalid current manifest",
	}, "change current checkout")
	docs, err := loadEvidenceManifest(context.Background(), repo.Path(), head, ".roborev/evidence.toml", nil)
	require.NoError(t, err)
	require.Len(t, docs, 1)
	assert.Equal(t, EvidenceDocument{
		ID: "source-a", TextPath: "evidence/source.txt",
		TextSHA256:   fmt.Sprintf("%x", sha256.Sum256([]byte(text))),
		SourceSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Provenance:   "docbank://vaults/V/documents/N/versions/C/renditions/A",
	}, docs[0])
}

// A missing or excluded required input must reject the whole manifest instead
// of producing an incomplete evidence set.
func TestEvidenceManifestRequiredInputs(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	text := "A committed rendition.\n"
	head := repo.CommitFiles(map[string]string{
		"evidence/source.txt":    text,
		".roborev/evidence.toml": manifestFixture("evidence/source.txt", text),
	}, "add evidence")
	for _, tc := range []struct {
		name, manifest string
		excludes       []string
	}{
		{name: "missing manifest", manifest: "missing.toml"},
		{name: "excluded manifest", manifest: ".roborev/evidence.toml", excludes: []string{".roborev"}},
		{name: "excluded payload", manifest: ".roborev/evidence.toml", excludes: []string{"source.txt"}},
		{name: "absolute manifest", manifest: "/evidence.toml"},
		{name: "traversal manifest", manifest: "../evidence.toml"},
		{name: "URL manifest", manifest: "https://example.invalid/evidence.toml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs, err := loadEvidenceManifest(context.Background(), repo.Path(), head, tc.manifest, tc.excludes)
			require.Error(t, err)
			assert.Nil(t, docs, "preparation must fail atomically")
		})
	}
}

// Accepting malformed metadata or returning a partially verified list could
// allow required evidence to disappear silently before invocation.
func TestEvidenceManifestValidation(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	text := "Committed text.\r\n"
	valid := manifestFixture("evidence/source.txt", text)
	for _, tc := range []struct {
		name, manifest, payload string
	}{
		{"unsupported version", strings.Replace(valid, "version = 1", "version = 2", 1), text},
		{"missing version", strings.Replace(valid, "version = 1", "", 1), text},
		{"wrong version type", strings.Replace(valid, "version = 1", `version = "1"`, 1), text},
		{"unknown key", valid + `text_hash = "ignored"`, text},
		{"duplicate key", valid + `id = "other"`, text},
		{"duplicate ID", valid + strings.TrimPrefix(valid, "version = 1\n"), text},
		{"missing ID", strings.Replace(valid, `id = "source-a"`, "", 1), text},
		{"blank ID", strings.Replace(valid, `id = "source-a"`, `id = " "`, 1), text},
		{"missing text path", strings.Replace(valid, `text_path = "evidence/source.txt"`, "", 1), text},
		{"missing payload", strings.Replace(valid, "evidence/source.txt", "missing.txt", 1), text},
		{"missing text hash", strings.Replace(valid, fmt.Sprintf("%x", sha256.Sum256([]byte(text))), "", 1), text},
		{"uppercase text hash", strings.Replace(valid, fmt.Sprintf("%x", sha256.Sum256([]byte(text))), strings.Repeat("A", 64), 1), text},
		{"short text hash", strings.Replace(valid, fmt.Sprintf("%x", sha256.Sum256([]byte(text))), "abcd", 1), text},
		{"nonhex text hash", strings.Replace(valid, fmt.Sprintf("%x", sha256.Sum256([]byte(text))), strings.Repeat("g", 64), 1), text},
		{"invalid source hash", strings.Replace(valid, strings.Repeat("a", 64), strings.Repeat("B", 64), 1), text},
		{"hash mismatch", valid, "Different committed text."},
		{"line ending mismatch", valid, strings.ReplaceAll(text, "\r\n", "\n")},
		{"invalid UTF8", manifestFixture("evidence/source.txt", "\xff"), "\xff"},
		{"malformed TOML", "version = [", text},
		{"late invalid entry", valid + `
[[documents]]
id = "source-b"
text_path = "missing.txt"
text_sha256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`, text},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			head := repo.CommitFiles(map[string]string{
				".roborev/evidence.toml": tc.manifest,
				"evidence/source.txt":    tc.payload,
			}, "change required evidence")
			docs, err := loadEvidenceManifest(context.Background(), repo.Path(), head, ".roborev/evidence.toml", nil)
			require.Error(t, err)
			assert.Nil(docs)
			assert.NotContains(err.Error(), tc.payload, "errors must not quote payload bytes")
			switch tc.name {
			case "malformed TOML":
				var decodeErr toml.ParseError
				require.ErrorAs(t, err, &decodeErr)
				assert.Equal(1, decodeErr.Position.Line)
				require.ErrorContains(t, err, "toml: line 1")
				require.ErrorContains(t, err, "unexpected EOF")
			case "wrong version type":
				require.ErrorContains(t, err, `toml: line 1 (last key "version")`)
				require.ErrorContains(t, err, "incompatible types: TOML value has type string; destination has type integer")
			case "unknown key":
				assert.EqualError(err, "invalid evidence manifest TOML")
			}
		})
	}
}

func TestEvidenceManifestRejectsNonlocalPaths(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	for _, value := range []string{
		"", ".", "..", "../source.txt", "evidence/../source.txt", "/source.txt",
		"C:/source.txt", "C:source.txt", `C:\source.txt`, `\\server\share\source.txt`,
		"//server/share/source.txt", "file:///source.txt", "docbank://vaults/V",
		"evidence/\x00source.txt", "evidence/./source.txt", "evidence//source.txt",
	} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			var encoded bytes.Buffer
			require.NoError(t, toml.NewEncoder(&encoded).Encode(EvidenceManifest{
				Version: 1, Documents: []EvidenceDocument{{ID: "source-a", TextPath: value, TextSHA256: strings.Repeat("a", 64)}},
			}))
			head := repo.CommitFile("manifest.toml", encoded.String(), "pin invalid path")
			docs, err := loadEvidenceManifest(context.Background(), repo.Path(), head, "manifest.toml", nil)
			require.Error(t, err)
			assert.Nil(t, docs)
		})
	}
}

func TestEvidenceManifestCommittedModes(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	text := "Committed text.\n"
	head := repo.CommitFiles(map[string]string{
		"source.txt": text, "manifest.toml": manifestFixture("source.txt", text),
	}, "add rendition")
	blob := repo.RevParse(head + ":source.txt")
	manifestBlob := repo.RevParse(head + ":manifest.toml")
	for _, tc := range []struct{ name, mode, object string }{
		{"payload symlink", "120000", blob},
		{"payload submodule", "160000", head},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo.Run("update-index", "--cacheinfo", tc.mode+","+tc.object+",source.txt")
			repo.Run("commit", "-m", "pin nonregular input")
			docs, err := loadEvidenceManifest(context.Background(), repo.Path(), repo.HeadSHA(), "manifest.toml", nil)
			require.Error(t, err)
			assert.Nil(t, docs)
		})
	}
	repo.Run("update-index", "--cacheinfo", "100755,"+blob+",source.txt")
	repo.Run("commit", "-m", "pin executable regular file")
	docs, err := loadEvidenceManifest(context.Background(), repo.Path(), repo.HeadSHA(), "manifest.toml", nil)
	require.NoError(t, err)
	assert.Len(t, docs, 1)
	for _, mode := range []string{"120000", "160000"} {
		object := manifestBlob
		if mode == "160000" {
			object = head
		}
		repo.Run("update-index", "--cacheinfo", mode+","+object+",manifest.toml")
		repo.Run("commit", "-m", "pin nonregular manifest")
		_, err := loadEvidenceManifest(context.Background(), repo.Path(), repo.HeadSHA(), "manifest.toml", nil)
		require.Error(t, err)
	}
}

func TestEvidenceManifestOptionalMetadataAndCancellation(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	text := ""
	manifest := fmt.Sprintf("version = 1\n[[documents]]\nid = 'empty'\ntext_path = 'empty.txt'\ntext_sha256 = '%x'\n", sha256.Sum256([]byte(text)))
	head := repo.CommitFiles(map[string]string{"manifest.toml": manifest, "empty.txt": text}, "pin empty rendition")
	before := repo.Run("ls-files", "--stage")
	docs, err := loadEvidenceManifest(context.Background(), repo.Path(), head, "manifest.toml", nil)
	require.NoError(t, err)
	require.Len(t, docs, 1)
	assert.Empty(t, docs[0].SourceSHA256)
	assert.Empty(t, docs[0].Provenance)
	assert.Equal(t, before, repo.Run("ls-files", "--stage"))
	assert.Empty(t, repo.Run("status", "--porcelain"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = loadEvidenceManifest(ctx, repo.Path(), head, "manifest.toml", nil)
	require.ErrorIs(t, err, context.Canceled)
	_, err = loadEvidenceManifest(context.Background(), repo.Path(), "HEAD", "manifest.toml", nil)
	require.Error(t, err, "callers must pin endpoints before loading")
	// Payloads present only in the checkout cannot satisfy a committed manifest.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), "untracked.txt"), []byte("local"), 0o600))
	head = repo.CommitFile("manifest.toml", manifestFixture("untracked.txt", "local"), "pin missing rendition")
	_, err = loadEvidenceManifest(context.Background(), repo.Path(), head, "manifest.toml", nil)
	require.Error(t, err)
}

func TestEvidenceManifestExactTOMLKeys(t *testing.T) {
	valid := manifestFixture("source.txt", "Text.")
	for _, key := range []string{"version", "documents", "id", "text_path", "text_sha256", "source_sha256", "provenance"} {
		t.Run(key, func(t *testing.T) {
			_, err := decodeEvidenceManifest([]byte(strings.Replace(valid, key, strings.ToUpper(key), 1)))
			require.Error(t, err, "manifest keys are case sensitive")
		})
	}
}

func TestEvidenceManifestHashBeforeUTF8(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	head := repo.CommitFiles(map[string]string{
		"source.txt": "\xff", "manifest.toml": manifestFixture("source.txt", "different bytes"),
	}, "pin mismatched bytes")
	_, err := loadEvidenceManifest(context.Background(), repo.Path(), head, "manifest.toml", nil)
	require.ErrorContains(t, err, "SHA256 mismatch")
}

func TestEvidenceManifestLiteralPathsAndExcludes(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	// These unusual names exist only in Git objects, never in the checkout.
	// Allow Git for Windows to index them in this temporary repository.
	repo.Run("config", "core.protectNTFS", "false")
	text := "Verified rendition.\n"
	initial := repo.CommitFile("fixture.txt", text, "add fixture")
	blob := repo.RevParse(initial + ":fixture.txt")
	for _, name := range []string{"evidence/quoted \"text\".txt", "evidence/tab\tline\n雪.txt", "evidence/:(glob)*.txt"} {
		var encoded bytes.Buffer
		want := []EvidenceDocument{
			{ID: "source-a", TextPath: name, TextSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(text)))},
			{ID: "source-b", TextPath: name, TextSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(text)))},
		}
		require.NoError(t, toml.NewEncoder(&encoded).Encode(EvidenceManifest{Version: 1, Documents: want}))
		repo.WriteFile("manifest.toml", encoded.String())
		repo.Run("add", "--", "manifest.toml")
		repo.Run("update-index", "--add", "--cacheinfo", "100644,"+blob+","+name)
		repo.Run("commit", "-m", "pin literal path")
		head := repo.HeadSHA()
		docs, err := loadEvidenceManifest(context.Background(), repo.Path(), head, "manifest.toml", nil)
		require.NoError(t, err)
		assert.Equal(t, want, docs, "distinct documents may pin the same rendition")
		for _, excludes := range [][]string{{"evidence"}, {"/evidence"}, {"evidence/*.txt"}} {
			_, err := loadEvidenceManifest(context.Background(), repo.Path(), head, "manifest.toml", excludes)
			require.Error(t, err)
		}
	}
	head := repo.CommitFiles(map[string]string{
		"evidence/go.sum": text, "manifest.toml": manifestFixture("evidence/go.sum", text),
	}, "pin excluded input")
	_, err := loadEvidenceManifest(context.Background(), repo.Path(), head, "manifest.toml", nil)
	require.Error(t, err, "built-in review exclusions apply too")
}

func TestEvidenceManifestPrivateIndexCleanup(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	head := repo.CommitFiles(map[string]string{
		"source.txt": "Text.", "manifest.toml": manifestFixture("source.txt", "Text."),
	}, "pin text")
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	t.Setenv("TMP", scratch)
	t.Setenv("TEMP", scratch)
	for _, manifest := range []string{"manifest.toml", "missing.toml"} {
		_, err := loadEvidenceManifest(context.Background(), repo.Path(), head, manifest, nil)
		if manifest == "manifest.toml" {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
		entries, err := os.ReadDir(scratch)
		require.NoError(t, err)
		assert.Empty(t, entries, "attempt index must be removed after success and failure")
	}
}

// Accepted paths must remain portable, repository-relative file names. The
// constructive direction exercises arbitrary input bytes without narrowing them.
func FuzzEvidenceManifestPaths(f *testing.F) {
	for _, seed := range []string{"source.txt", "evidence/雪.txt", "\xff", "../secret", "/secret", "C:/secret", `C:\secret`, `\\server\share`, "docbank://vaults/V", "a\x00b", "a/../../b", "evidence/:(glob)*.txt"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if validateEvidenceManifestPath(value) == nil {
			assert := assert.New(t)
			assert.True(fs.ValidPath(value))
			assert.NotEqual(".", value)
			assert.False(path.IsAbs(value))
			assert.NotContains(value, "\\")
			assert.NotContains(value, "\x00")
			first, _, _ := strings.Cut(value, "/")
			assert.NotContains(first, ":")
		}
		valid := "evidence/" + hex.EncodeToString([]byte(value)) + ".txt"
		require.NoError(t, validateEvidenceManifestPath(valid))
	})
}

// Arbitrary TOML either rejects or preserves the manifest invariants. A valid
// generated document also round-trips its exact declared metadata.
func FuzzEvidenceManifestDecode(f *testing.F) {
	for _, seed := range []string{manifestFixture("evidence/source.txt", "Text."), "version = 1", "version = [", "version = 0", "version = 1\nversion = 1", "\xff", "version = 1\n[[documents]]\nid = '雪'\ntext_path = '../outside'"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		manifest, err := decodeEvidenceManifest(data)
		if err == nil {
			assert.Equal(t, 1, manifest.Version)
			ids := make(map[string]struct{})
			for _, doc := range manifest.Documents {
				assert.NotEmpty(t, strings.TrimSpace(doc.ID))
				_, duplicate := ids[doc.ID]
				assert.False(t, duplicate)
				ids[doc.ID] = struct{}{}
				require.NoError(t, validateEvidenceManifestPath(doc.TextPath))
				assert.Regexp(t, `^[0-9a-f]{64}$`, doc.TextSHA256)
				if doc.SourceSHA256 != "" {
					assert.Regexp(t, `^[0-9a-f]{64}$`, doc.SourceSHA256)
				}
			}
		}
		want := EvidenceManifest{Version: 1, Documents: []EvidenceDocument{{
			ID: "source-a", TextPath: "evidence/source.txt", TextSHA256: fmt.Sprintf("%x", sha256.Sum256(data)),
			Provenance: strings.ToValidUTF8(string(data), "�"),
		}}}
		var encoded bytes.Buffer
		require.NoError(t, toml.NewEncoder(&encoded).Encode(want))
		got, err := decodeEvidenceManifest(encoded.Bytes())
		require.NoError(t, err)
		assert.Equal(t, want, got)
		// The same otherwise valid data cannot be accepted under another version.
		invalid := strings.Replace(encoded.String(), "version = 1", "version = 0", 1)
		_, err = decodeEvidenceManifest([]byte(invalid))
		require.Error(t, err)
	})
}

// Replacements must not substitute either a required manifest or its rendition,
// even when called directly outside an evidence-scoped Builder context.
func TestEvidenceManifestOriginalReplacementObjects(t *testing.T) {
	for _, ranged := range []bool{false, true} {
		for _, kind := range []string{"commit", "blobs"} {
			t.Run(fmt.Sprintf("range=%t/%s", ranged, kind), func(t *testing.T) {
				repo := testutil.NewTestRepoWithCommit(t)
				base := repo.CommitFile("rubric.md", "Check required source text.", "rubric")
				original := "Original pinned source.\r\nCafé.\n"
				manifest := manifestFixture("source.txt", original)
				head := repo.CommitFiles(map[string]string{"source.txt": original, "manifest.toml": manifest, "draft.md": "Original draft."}, "pin source")
				manifestOID := repo.RevParse(head + ":manifest.toml")
				textOID := repo.RevParse(head + ":source.txt")
				substituted := "Substituted source text.\n"
				other := repo.CommitFiles(map[string]string{
					"source.txt":    substituted,
					"manifest.toml": strings.Replace(manifestFixture("source.txt", substituted), `id = "source-a"`, `id = "source-z"`, 1),
				}, "replacement source")
				if kind == "commit" {
					replacement := repo.Run("commit-tree", repo.RevParse(other+"^{tree}"), "-p", base, "-m", "replacement review subject")
					repo.Run("replace", head, replacement)
				} else {
					repo.Run("replace", manifestOID, repo.RevParse(other+":manifest.toml"))
					repo.Run("replace", textOID, repo.RevParse(other+":source.txt"))
				}
				assert.Equal(t, strings.TrimSpace(substituted), repo.Run("show", head+":source.txt"), "ordinary Git proves native replacements are active")
				assert.Contains(t, repo.Run("show", head+":manifest.toml"), `id = "source-z"`)
				docs, err := loadEvidenceManifest(context.Background(), repo.Path(), head, "manifest.toml", nil)
				require.NoError(t, err)
				require.Len(t, docs, 1)
				assert.Equal(t, "source-a", docs[0].ID)
				sum := fmt.Sprintf("%x", sha256.Sum256([]byte(original)))
				assert.Equal(t, sum, docs[0].TextSHA256)
				ref := head
				if ranged {
					ref = base + ".." + head
				}
				rc := &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{"citations": {Template: "rubric.md", EvidenceManifest: "manifest.toml"}}}}
				builder := NewBuilder(nil).ForRepo(repo.Path(), 0).WithRepoConfig(rc, head)
				result, err := builder.BuildWithSnapshot(ref, 0, "test", "citations", "", nil)
				require.NoError(t, err)
				require.NotNil(t, result.Cleanup)
				t.Cleanup(result.Cleanup)
				assert.Contains(t, result.StoredPrompt, sum)
				assert.NotContains(t, result.StoredPrompt, "source-z")
				assert.NotContains(t, result.StoredPrompt, substituted)
				index, err := builder.WithReviewSettings(config.ReviewSettings{EvidenceManifest: "manifest.toml"}).BuildEvidenceIndex(ref, nil)
				require.NoError(t, err)
				require.Len(t, index.Entries, 1)
				assert.Equal(t, textOID, index.Entries[0].BlobOID)
				assert.Equal(t, "source-a", index.Entries[0].DocumentID)
				files, err := filepath.Glob(filepath.Join(repo.Path(), ".roborev", "roborev-snapshot-*", sum+".txt"))
				require.NoError(t, err)
				require.Len(t, files, 1)
				data, err := os.ReadFile(files[0])
				require.NoError(t, err)
				assert.Equal(t, original, string(data))
			})
		}
	}
}

// A private index must not let repository hooks modify the working checkout.
func TestEvidenceManifestDoesNotRunIndexHooks(t *testing.T) {
	for _, location := range []string{"default", "absolute", "relative"} {
		t.Run(location, func(t *testing.T) {
			assert := assert.New(t)
			repo := testutil.NewTestRepoWithCommit(t)
			text := "Committed rendition.\n"
			head := repo.CommitFiles(map[string]string{
				"source.txt":    text,
				"manifest.toml": manifestFixture("source.txt", text),
				"draft.md":      "Committed draft.\n",
			}, "pin rendition")
			draftPath := filepath.Join(repo.Path(), "draft.md")
			draft := []byte("Uncommitted draft.\n")
			require.NoError(t, os.WriteFile(draftPath, draft, 0o644))
			indexPath := filepath.Join(repo.GitDir, "index")
			index, err := os.ReadFile(indexPath)
			require.NoError(t, err)
			hooksDir := repo.HooksDir
			if location != "default" {
				hooksDir = filepath.Join(repo.Path(), "custom-hooks")
				configured := hooksDir
				if location == "relative" {
					configured = "custom-hooks"
				}
				repo.Run("config", "core.hooksPath", configured)
			}
			require.NoError(t, os.MkdirAll(hooksDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(hooksDir, "post-index-change"),
				[]byte("#!/bin/sh\nprintf 'Hook changed draft.\\n' > draft.md\n"), 0o755))

			// Prove the installed hook runs even when Git writes a private index.
			cmd := exec.Command("git", "read-tree", head)
			cmd.Dir = repo.Path()
			cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(t.TempDir(), "index"))
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
			changed, err := os.ReadFile(draftPath)
			require.NoError(t, err)
			require.Equal(t, "Hook changed draft.\n", string(changed))
			require.NoError(t, os.WriteFile(draftPath, draft, 0o644))

			docs, err := loadEvidenceManifest(context.Background(), repo.Path(), head, "manifest.toml", nil)
			require.NoError(t, err)
			assert.Len(docs, 1)
			afterDraft, err := os.ReadFile(draftPath)
			require.NoError(t, err)
			assert.Equal(draft, afterDraft, "manifest reads must preserve uncommitted work")
			afterIndex, err := os.ReadFile(indexPath)
			require.NoError(t, err)
			assert.Equal(index, afterIndex, "manifest reads must preserve the checkout index")
		})
	}
}

// Losing captured stderr or the failed Git stage hides the actionable reason
// for a required-manifest failure. Neither error path may quote text payloads.
func TestEvidenceManifestGitFailureDiagnostics(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	const payload = "Synthetic rendition payload must not appear in Git diagnostics."
	head := repo.CommitFiles(map[string]string{
		"source.txt": payload, "manifest.toml": manifestFixture("source.txt", payload),
	}, "pin rendition")
	for _, tc := range []struct {
		stage, head string
		excludes    []string
	}{
		{stage: "read-tree", head: strings.Repeat("0", 40)},
		{stage: "ls-files", head: head, excludes: []string{"../outside"}},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			assert := assert.New(t)
			docs, err := loadEvidenceManifest(context.Background(), repo.Path(), tc.head, "manifest.toml", tc.excludes)
			require.Error(t, err)
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr)
			require.NotEmpty(t, exitErr.Stderr, "fixture must cause a real Git diagnostic")
			assert.Nil(docs, "Git failures must still reject required evidence")
			assert.Contains(err.Error(), "("+tc.stage+")", "identify the failed Git operation")
			assert.Contains(err.Error(), strings.TrimSpace(string(exitErr.Stderr)), "preserve Git's diagnostic")
			assert.NotContains(err.Error(), payload, "diagnostics must not add rendition payloads")
			t.Logf("%s diagnostic: %s", tc.stage, err)
		})
	}
}
