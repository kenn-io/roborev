package git

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvidenceEndpoints(t *testing.T) {
	r := NewTestRepo(t)
	r.CommitFile("draft.txt", "first", "root")
	root := r.Run("rev-parse", "HEAD")
	r.CommitFile("draft.txt", "second", "update")
	head := r.Run("rev-parse", "HEAD")
	for _, tc := range []struct {
		ref  string
		want ReviewEndpoints
	}{
		{root, ReviewEndpoints{Head: root}},
		{"HEAD", ReviewEndpoints{Head: head, Bases: []string{root}}},
		{root + "..HEAD", ReviewEndpoints{Head: head, Bases: []string{root}}},
		{EmptyTreeSHA + "..HEAD", ReviewEndpoints{Head: head, Bases: []string{EmptyTreeSHA}}},
		{root + "...HEAD", ReviewEndpoints{Head: head, Bases: []string{root}}},
	} {
		got, err := ResolveReviewEndpoints(context.Background(), r.Dir, tc.ref)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
	tree := r.Run("rev-parse", "HEAD^{tree}")
	for _, ref := range []string{"--all", "missing", "HEAD: draft.txt", tree + "..HEAD", root + ".." + tree, EmptyTreeSHA + ".." + EmptyTreeSHA, EmptyTreeSHA + "...HEAD"} {
		_, err := ResolveReviewEndpoints(context.Background(), r.Dir, ref)
		require.Error(t, err, ref)
	}
}

func TestEvidenceCommittedBinarySides(t *testing.T) {
	r := NewTestRepo(t)
	r.Run("config", "diff.renames", "true")
	oldPath := "old\t\"café\n.pdf"
	if runtime.GOOS == "windows" {
		// Native Windows filenames disallow control characters and quotes.
		oldPath = "old café.pdf"
	}
	r.WriteFile(oldPath, "%PDF-1.7\x00old")
	// The in-process test helper rejects control characters that native Git accepts.
	r.Run("add", "--", oldPath)
	r.Run("commit", "-m", "root binary")
	root := r.Run("rev-parse", "HEAD")
	oldOID := r.Run("rev-parse", "HEAD:"+oldPath)
	target, err := ResolveReviewEndpoints(context.Background(), r.Dir, root)
	require.NoError(t, err)
	changes, err := ChangedBinaryEvidence(context.Background(), r.Dir, target, nil)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, EvidenceChange{Kind: "A", New: &EvidenceBlob{Path: oldPath, OID: oldOID, Mode: "100644"}}, changes[0])

	r.Run("mv", "--", oldPath, "renamed.pdf")
	r.CommitAll("rename")
	target, err = ResolveReviewEndpoints(context.Background(), r.Dir, "HEAD")
	require.NoError(t, err)
	changes, err = ChangedBinaryEvidence(context.Background(), r.Dir, target, nil)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, "R100", changes[0].Kind)
	assert.Equal(t, []EvidenceBlob{{Path: oldPath, OID: oldOID, Mode: "100644", Parent: 1}}, changes[0].Old)
	assert.Equal(t, &EvidenceBlob{Path: "renamed.pdf", OID: oldOID, Mode: "100644"}, changes[0].New)

	r.CommitFile("renamed.pdf", "%PDF-1.7\x00new", "modify")
	newOID := r.Run("rev-parse", "HEAD:renamed.pdf")
	target, err = ResolveReviewEndpoints(context.Background(), r.Dir, "HEAD")
	require.NoError(t, err)
	changes, err = ChangedBinaryEvidence(context.Background(), r.Dir, target, nil)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, "M", changes[0].Kind)
	assert.Equal(t, oldOID, changes[0].Old[0].OID)
	assert.Equal(t, newOID, changes[0].New.OID)
	var data bytes.Buffer
	require.NoError(t, CopyBlob(context.Background(), r.Dir, newOID, &data))
	assert.Equal(t, "%PDF-1.7\x00new", data.String())

	r.Run("rm", "renamed.pdf")
	r.CommitAll("delete")
	target, err = ResolveReviewEndpoints(context.Background(), r.Dir, "HEAD")
	require.NoError(t, err)
	changes, err = ChangedBinaryEvidence(context.Background(), r.Dir, target, nil)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, EvidenceChange{Kind: "D", Old: []EvidenceBlob{{Path: "renamed.pdf", OID: newOID, Mode: "100644", Parent: 1}}}, changes[0])
}

func TestEvidenceAttributesAreCommittedAndPrivate(t *testing.T) {
	r := NewTestRepo(t)
	r.WriteFile(".gitattributes", "*.blob -diff\n*.pointer diff\n")
	r.WriteFile("report.blob", "opaque without NUL")
	r.WriteFile("source.pointer", "version https://git-lfs.github.com/spec/v1\noid sha256:"+strings.Repeat("a", 64)+"\nsize 100\n")
	r.WriteFile("excluded.blob", "excluded")
	r.CommitAll("committed attributes")
	target, err := ResolveReviewEndpoints(context.Background(), r.Dir, "HEAD")
	require.NoError(t, err)
	// None of these current-checkout attributes may change reviewed identities.
	r.WriteFile(".gitattributes", "* diff\n")
	require.NoError(t, os.WriteFile(filepath.Join(r.Dir, ".git", "info", "attributes"), []byte("* diff\n"), 0o600))
	global := filepath.Join(t.TempDir(), "attributes")
	require.NoError(t, os.WriteFile(global, []byte("* diff\n"), 0o600))
	r.Run("config", "core.attributesFile", global)
	indexBefore, err := os.ReadFile(filepath.Join(r.Dir, ".git", "index"))
	require.NoError(t, err)
	changes, err := ChangedBinaryEvidence(context.Background(), r.Dir, target, []string{"excluded.blob"})
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, "report.blob", changes[0].New.Path)
	indexAfter, err := os.ReadFile(filepath.Join(r.Dir, ".git", "index"))
	require.NoError(t, err)
	assert.Equal(t, indexBefore, indexAfter)
	attrs, err := os.ReadFile(filepath.Join(r.Dir, ".gitattributes"))
	require.NoError(t, err)
	assert.Equal(t, "* diff\n", string(attrs))
}

func TestEvidenceCombinedMergeSelection(t *testing.T) {
	r := NewTestRepo(t)
	r.CommitFile("both.bin", "\x00root", "root")
	root := r.Run("rev-parse", "HEAD")
	r.Run("checkout", "-b", "side")
	r.CommitFile("side-only.bin", "\x00side", "side")
	side := r.Run("rev-parse", "HEAD")
	r.Run("checkout", "-b", "main-test", root)
	r.CommitFile("main-only.bin", "\x00main", "main")
	main := r.Run("rev-parse", "HEAD")
	r.Run("merge", "--no-commit", "side")
	r.WriteFile("both.bin", "\x00merged")
	r.Run("add", ".")
	// Native Git consumes MERGE_HEAD; the in-process fixture helper does not.
	r.Run("commit", "-m", "merge")
	target, err := ResolveReviewEndpoints(context.Background(), r.Dir, "HEAD")
	require.NoError(t, err)
	assert.Equal(t, []string{main, side}, target.Bases)
	changes, err := ChangedBinaryEvidence(context.Background(), r.Dir, target, nil)
	require.NoError(t, err)
	require.Len(t, changes, 1, "combined diff excludes paths equal to either parent")
	assert.Equal(t, "both.bin", changes[0].New.Path)
	require.Len(t, changes[0].Old, 2)
	assert.Equal(t, 1, changes[0].Old[0].Parent)
	assert.Equal(t, 2, changes[0].Old[1].Parent)
	// An explicit range has endpoint semantics even when its head is a merge.
	target, err = ResolveReviewEndpoints(context.Background(), r.Dir, root+"..HEAD")
	require.NoError(t, err)
	changes, err = ChangedBinaryEvidence(context.Background(), r.Dir, target, nil)
	require.NoError(t, err)
	assert.Len(t, changes, 3)
}

type evidenceFailWriter struct{}

func (evidenceFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestEvidenceCopyFailures(t *testing.T) {
	r := NewTestRepo(t)
	r.CommitFile("data.bin", "\x00data", "binary")
	oid := r.Run("rev-parse", "HEAD:data.bin")
	require.Error(t, CopyBlob(context.Background(), r.Dir, oid, evidenceFailWriter{}))
	require.Error(t, CopyBlob(context.Background(), r.Dir, strings.Repeat("0", 40), io.Discard))
	require.Error(t, CopyBlob(context.Background(), r.Dir, "HEAD:data.bin", io.Discard), "blob identities must be immutable")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := CopyBlob(ctx, r.Dir, oid, io.Discard)
	require.ErrorIs(t, err, context.Canceled)
}

func TestEvidenceDeletedCommittedAttributes(t *testing.T) {
	r := NewTestRepo(t)
	r.WriteFile(".gitattributes", "report.dat -diff\n")
	r.WriteFile("report.dat", "binary by committed attribute")
	r.CommitAll("root")
	r.Run("rm", "report.dat", ".gitattributes")
	r.Run("commit", "-m", "delete evidence and attributes")
	target, err := ResolveReviewEndpoints(context.Background(), r.Dir, "HEAD")
	require.NoError(t, err)
	changes, err := ChangedBinaryEvidence(context.Background(), r.Dir, target, nil)
	require.NoError(t, err)
	require.Len(t, changes, 1, "old endpoint attributes still identify deleted binary evidence")
	assert.Equal(t, "report.dat", changes[0].Old[0].Path)
}

func TestEvidenceOmittedModes(t *testing.T) {
	r := NewTestRepo(t)
	r.CommitFile("data.bin", "\x00data", "root")
	oid := r.Run("rev-parse", "HEAD:data.bin")
	commit := r.Run("rev-parse", "HEAD")
	r.Run("update-index", "--chmod=+x", "data.bin")
	r.Run("update-index", "--add", "--cacheinfo", "120000,"+oid+",link")
	r.Run("update-index", "--add", "--cacheinfo", "160000,"+commit+",module")
	r.Run("commit", "-m", "unsafe modes and mode-only change")
	target, err := ResolveReviewEndpoints(context.Background(), r.Dir, "HEAD")
	require.NoError(t, err)
	changes, err := ChangedBinaryEvidence(context.Background(), r.Dir, target, nil)
	require.NoError(t, err)
	require.Len(t, changes, 3)
	for _, change := range changes {
		assert.NotEmpty(t, change.Omission, change.New.Path)
	}
	assert.Contains(t, changes[0].Omission, "mode-only")
}

func TestEvidenceCombinedRenamePaths(t *testing.T) {
	r := NewTestRepo(t)
	r.Run("config", "diff.renames", "true")
	r.CommitFile("root.pdf", "%PDF-1.7\x00data", "root")
	root := r.HeadSHA()
	r.Run("checkout", "-b", "side")
	r.Run("mv", "root.pdf", "side.pdf")
	r.Run("commit", "-m", "side rename")
	side := r.HeadSHA()
	r.Run("checkout", "-b", "main-test", root)
	r.Run("mv", "root.pdf", "main.pdf")
	r.Run("commit", "-m", "main rename")
	main := r.HeadSHA()
	r.Run("mv", "main.pdf", "merged.pdf")
	tree := r.Run("write-tree")
	merge := r.Run("commit-tree", tree, "-p", main, "-p", side, "-m", "resolved rename")
	target, err := ResolveReviewEndpoints(context.Background(), r.Dir, merge)
	require.NoError(t, err)
	changes, err := ChangedBinaryEvidence(context.Background(), r.Dir, target, nil)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, "merged.pdf", changes[0].New.Path)
	require.Len(t, changes[0].Old, 2)
	assert.Equal(t, "main.pdf", changes[0].Old[0].Path)
	assert.Equal(t, "side.pdf", changes[0].Old[1].Path)
}

func TestEvidenceRespectsReviewRenameConfig(t *testing.T) {
	r := NewTestRepo(t)
	r.CommitFile("old.bin", "\x00content", "root")
	r.Run("mv", "old.bin", "new.bin")
	r.Run("commit", "-m", "rename")
	r.Run("config", "diff.renames", "false")
	diff, err := GetDiffCtx(context.Background(), r.Dir, "HEAD")
	require.NoError(t, err)
	assert.Contains(t, diff, "deleted file mode")
	assert.NotContains(t, diff, "rename from")
	target, err := ResolveReviewEndpoints(context.Background(), r.Dir, "HEAD")
	require.NoError(t, err)
	changes, err := ChangedBinaryEvidence(context.Background(), r.Dir, target, nil)
	require.NoError(t, err)
	require.Len(t, changes, 2, "evidence preserves the review diff's rename setting")
	assert.Equal(t, "A", changes[0].Kind)
	assert.Equal(t, "D", changes[1].Kind)
}

// All policy inputs are synthetic. HOME matters because production Git reads
// persistent config after stripping inherited GIT_CONFIG_* overrides.
func evidenceConfigHome(t *testing.T, global string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(global), 0o600))
}

func TestEvidenceRenameConfigValues(t *testing.T) {
	for _, tc := range []struct {
		name, global, local string
		copies              bool
	}{
		{"copy", "", "copy", true},
		{"Copies", "", "Copies", true},
		{"copies", "", "copies", true},
		{"true", "", "true", false},
		{"global-false-local-true", "false", "true", false},
		{"global-copies-local-true", "copies", "true", false},
		{"global-false-local-copies", "false", "copies", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global := ""
			if tc.global != "" {
				global = "[diff]\nrenames = " + tc.global + "\n"
			}
			evidenceConfigHome(t, global)
			r := NewTestRepo(t)
			r.CommitFile("source.bin", "\x00original bytes", "root")
			r.WriteFile("copy.bin", "\x00original bytes")
			r.WriteFile("source.bin", "\x00modified bytes")
			r.CommitAll("copy and modify source")
			r.Run("config", "diff.renames", tc.local)
			diff, err := GetDiffCtx(context.Background(), r.Dir, "HEAD")
			require.NoError(t, err)
			if tc.copies {
				require.Contains(t, diff, "copy from source.bin")
			} else {
				require.NotContains(t, diff, "copy from")
			}
			target, err := ResolveReviewEndpoints(context.Background(), r.Dir, "HEAD")
			require.NoError(t, err)
			changes, err := ChangedBinaryEvidence(context.Background(), r.Dir, target, nil)
			require.NoError(t, err)
			require.Len(t, changes, 2)
			want := "A"
			if tc.copies {
				want = "C100"
			}
			assert.Equal(t, want, changes[0].Kind)
			assert.Equal(t, "M", changes[1].Kind)
		})
	}
}

func TestEvidenceRenameOldPathBinaryAttribute(t *testing.T) {
	evidenceConfigHome(t, "")
	r := NewTestRepo(t)
	r.Run("config", "diff.renames", "true")
	r.WriteFile(".gitattributes", "old.dat -diff\n")
	r.WriteFile("old.dat", "no NUL: binary only by old path attribute")
	r.CommitAll("old path binary")
	r.Run("mv", "old.dat", "new.dat")
	r.CommitAll("rename")
	target, err := ResolveReviewEndpoints(context.Background(), r.Dir, "HEAD")
	require.NoError(t, err)
	changes, err := ChangedBinaryEvidence(context.Background(), r.Dir, target, nil)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, "R100", changes[0].Kind)
	assert.Equal(t, "old.dat", changes[0].Old[0].Path)
	assert.Equal(t, "new.dat", changes[0].New.Path)
}
