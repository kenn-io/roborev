package prompt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	gitcmd "go.kenn.io/kit/git/cmd"

	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/procutil"
)

// EvidenceManifest pins local renditions at the reviewed commit.
type EvidenceManifest struct {
	Version   int                `toml:"version"`
	Documents []EvidenceDocument `toml:"documents"`
}

// EvidenceDocument identifies exported text. SourceSHA256 and Provenance are
// declared metadata, not proof of source identity or rendition fidelity.
type EvidenceDocument struct {
	ID           string `toml:"id"`
	TextPath     string `toml:"text_path"`
	TextSHA256   string `toml:"text_sha256"`
	SourceSHA256 string `toml:"source_sha256"`
	Provenance   string `toml:"provenance"`
}

// loadEvidenceManifest reads only committed objects. A private index allows
// native Git exclusions to apply to unchanged required files as well as changes.
func loadEvidenceManifest(ctx context.Context, repoPath, head, manifestPath string, excludes []string) ([]EvidenceDocument, error) {
	loaded, err := loadEvidenceManifestWithEntries(ctx, repoPath, head, manifestPath, excludes)
	return loaded.documents, err
}

type loadedEvidenceManifest struct {
	documents []EvidenceDocument
	entries   []EvidenceEntry
}

func loadEvidenceManifestWithEntries(ctx context.Context, repoPath, head, manifestPath string, excludes []string) (loadedEvidenceManifest, error) {
	// Direct callers share the Builder's immutable evidence read policy.
	ctx = git.WithEvidenceReadPolicy(ctx)
	if (len(head) != 40 && len(head) != 64) || !evidenceManifestLowerHex(head) {
		return loadedEvidenceManifest{}, fmt.Errorf("evidence manifest requires a pinned commit identity")
	}
	if err := validateEvidenceManifestPath(manifestPath); err != nil {
		return loadedEvidenceManifest{}, err
	}
	dir, err := os.MkdirTemp("", "roborev-manifest-*")
	if err != nil {
		return loadedEvidenceManifest{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// read-tree writes only the private index, but Git still runs hooks from
	// the source repository unless hooksPath points at our private directory.
	runner := gitcmd.Runner{StripEnv: true, DisableSafeDirectoryForward: true}.
		WithConfig("core.hooksPath", dir)
	run := func(operation string, args ...string) ([]byte, error) {
		cmd := runner.Command(ctx, repoPath, args...)
		cmd.Env = append(cmd.Environ(), "GIT_INDEX_FILE="+filepath.Join(dir, "index"), "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1")
		out, err := cmd.Output()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("read committed evidence tree (%s): %w", operation, procutil.WithStderr(err))
		}
		return out, nil
	}
	if _, err := run("read-tree", "-c", "core.sparseCheckout=false", "read-tree", head+"^{commit}"); err != nil {
		return loadedEvidenceManifest{}, err
	}
	// Listing from the root keeps Git's exclusion matching active. Some Git
	// versions skip exclusions when ls-files receives an exact positive filename.
	args := append([]string{"ls-files", "--stage", "-z", "--"}, git.ReviewPathspecArgs(excludes...)...)
	indexEntries, err := run("ls-files", args...)
	if err != nil {
		return loadedEvidenceManifest{}, err
	}
	metadataByPath := make(map[string][]byte)
	for entry := range bytes.SplitSeq(indexEntries, []byte{0}) {
		metadata, name, ok := bytes.Cut(entry, []byte{'\t'})
		if ok {
			metadataByPath[string(name)] = metadata
		}
	}

	type committedFile struct {
		data []byte
		oid  string
	}
	read := func(filePath string) (committedFile, error) {
		if err := validateEvidenceManifestPath(filePath); err != nil {
			return committedFile{}, err
		}
		fields := bytes.Fields(metadataByPath[filePath])
		if len(fields) != 3 || string(fields[2]) != "0" ||
			(string(fields[0]) != "100644" && string(fields[0]) != "100755") {
			return committedFile{}, fmt.Errorf("required evidence path %q is missing, excluded or not a regular Git file", filePath)
		}
		var data bytes.Buffer
		if err := git.CopyBlob(ctx, repoPath, string(fields[1]), &data); err != nil {
			return committedFile{}, err
		}
		return committedFile{data.Bytes(), string(fields[1])}, nil
	}
	data, err := read(manifestPath)
	if err != nil {
		return loadedEvidenceManifest{}, err
	}
	manifest, err := decodeEvidenceManifest(data.data)
	if err != nil {
		return loadedEvidenceManifest{}, err
	}
	loaded := loadedEvidenceManifest{documents: manifest.Documents}
	for _, document := range manifest.Documents {
		payload, err := read(document.TextPath)
		if err != nil {
			return loadedEvidenceManifest{}, err
		}
		if fmt.Sprintf("%x", sha256.Sum256(payload.data)) != document.TextSHA256 {
			return loadedEvidenceManifest{}, fmt.Errorf("evidence text SHA256 mismatch for %q", document.TextPath)
		}
		if !utf8.Valid(payload.data) {
			return loadedEvidenceManifest{}, fmt.Errorf("evidence text %q is not valid UTF-8", document.TextPath)
		}
		loaded.entries = append(loaded.entries, EvidenceEntry{
			Path: document.TextPath, File: document.TextSHA256 + ".txt", Kind: "rendition", Side: "head",
			BlobOID: payload.oid, SHA256: document.TextSHA256, Bytes: int64(len(payload.data)), MediaHint: evidenceTextMedia,
			DocumentID: document.ID, TextPath: document.TextPath,
			DeclaredSourceSHA256: document.SourceSHA256, Provenance: document.Provenance,
		})
	}
	return loaded, nil
}

func decodeEvidenceManifest(data []byte) (EvidenceManifest, error) {
	var manifest EvidenceManifest
	metadata, err := toml.Decode(string(data), &manifest)
	if err != nil {
		return EvidenceManifest{}, fmt.Errorf("invalid evidence manifest TOML: %w", err)
	}
	if len(metadata.Undecoded()) != 0 {
		return EvidenceManifest{}, fmt.Errorf("invalid evidence manifest TOML")
	}
	// The TOML decoder otherwise matches struct fields case-insensitively.
	for _, key := range metadata.Keys() {
		switch key.String() {
		case "version", "documents", "documents.id", "documents.text_path", "documents.text_sha256", "documents.source_sha256", "documents.provenance":
		default:
			return EvidenceManifest{}, fmt.Errorf("invalid evidence manifest TOML key")
		}
	}
	if manifest.Version != 1 {
		return EvidenceManifest{}, fmt.Errorf("unsupported evidence manifest version %d", manifest.Version)
	}
	ids := make(map[string]bool)
	for _, document := range manifest.Documents {
		if strings.TrimSpace(document.ID) == "" || ids[document.ID] {
			return EvidenceManifest{}, fmt.Errorf("evidence manifest requires nonempty unique document IDs")
		}
		ids[document.ID] = true
		if err := validateEvidenceManifestPath(document.TextPath); err != nil {
			return EvidenceManifest{}, err
		}
		if len(document.TextSHA256) != 64 || !evidenceManifestLowerHex(document.TextSHA256) ||
			(document.SourceSHA256 != "" && (len(document.SourceSHA256) != 64 || !evidenceManifestLowerHex(document.SourceSHA256))) {
			return EvidenceManifest{}, fmt.Errorf("evidence manifest hashes must be 64 lowercase hexadecimal characters")
		}
	}
	return manifest, nil
}

func evidenceManifestLowerHex(value string) bool {
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validateEvidenceManifestPath(value string) error {
	first, _, _ := strings.Cut(value, "/")
	if value == "" || value == "." || !utf8.ValidString(value) || path.IsAbs(value) || path.Clean(value) != value ||
		strings.ContainsAny(value, "\\\x00") || strings.Contains(first, ":") {
		return fmt.Errorf("evidence path %q must be a repository-relative file path", value)
	}
	for part := range strings.SplitSeq(value, "/") {
		if part == ".." {
			return fmt.Errorf("evidence path %q must not traverse directories", value)
		}
	}
	return nil
}
