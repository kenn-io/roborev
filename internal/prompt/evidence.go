package prompt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/kit/atomicfile"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
)

// EvidenceEntry describes the raw bytes offered for one committed side.
// Path is an identity label, never a destination filename. For non-UTF-8 Git
// names it is the quoted display label, and PathBytes holds the original bytes.
type EvidenceEntry struct {
	Path      string `json:"path"`
	PathBytes []byte `json:"path_bytes,omitzero"`
	File      string `json:"file,omitzero"`
	Kind      string `json:"kind"`
	Side      string `json:"side"`
	BlobOID   string `json:"blob_oid"`
	SHA256    string `json:"sha256"`
	MediaHint string `json:"media_hint"`
	Omission  string `json:"omission"`
	Bytes     int64  `json:"bytes"`
}

// EvidenceIndex is portable: its identities contain no attempt-local paths.
type EvidenceIndex struct {
	Version int                 `json:"version"`
	Target  git.ReviewEndpoints `json:"target"`
	Entries []EvidenceEntry     `json:"entries"`
}

const (
	evidenceFooterStart = "\n\n<!-- roborev-evidence:v1\n"
	evidenceFooterEnd   = "\n-->\n"
)

// originalPath accepts exactly one representation for each byte-oriented Git
// pathname. Valid UTF-8 keeps its existing wire format; only other names carry
// raw bytes plus a canonical escaped label.
func (e EvidenceEntry) originalPath() (string, error) {
	if e.PathBytes == nil {
		if !utf8.ValidString(e.Path) {
			return "", fmt.Errorf("evidence path requires lossless byte identity")
		}
		return e.Path, nil
	}
	path := string(e.PathBytes)
	if utf8.ValidString(path) || e.Path != strconv.Quote(path) {
		return "", fmt.Errorf("noncanonical evidence path identity")
	}
	return path, nil
}

func (index EvidenceIndex) validatePaths() error {
	for _, entry := range index.Entries {
		if _, err := entry.originalPath(); err != nil {
			return err
		}
	}
	return nil
}

// AppendEvidenceIndex attaches portable identities to assembled prompt text.
// Callers retain this text for storage; attempt paths are bound only at execution.
func AppendEvidenceIndex(text string, index EvidenceIndex) (string, error) {
	if index.Version != 1 {
		return "", fmt.Errorf("unsupported evidence index version %d", index.Version)
	}
	if err := index.validatePaths(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(index)
	if err != nil {
		return "", fmt.Errorf("encode evidence identities: %w", err)
	}
	return text + evidenceFooterStart + string(encoded) + evidenceFooterEnd, nil
}

// BindEvidenceIndex replaces only the final assembly-owned footer. expected must
// be independently built for the resolved settings and pinned reviewed target;
// copied prompt text is never authority for selecting blobs or tools.
func (b *Builder) BindEvidenceIndex(text string, expected EvidenceIndex, target SnapshotTarget) (string, func(), error) {
	body, err := evidencePromptBody(text, expected)
	if err != nil {
		return "", nil, err
	}
	section, cleanup, err := b.MaterializeEvidence(expected, target)
	if err != nil {
		return "", nil, err
	}
	return body + section, cleanup, nil
}

func evidencePromptBody(text string, expected EvidenceIndex) (string, error) {
	start := strings.LastIndex(text, evidenceFooterStart)
	if start < 0 || !strings.HasSuffix(text, evidenceFooterEnd) {
		return "", fmt.Errorf("missing generated evidence footer")
	}
	payload := text[start+len(evidenceFooterStart) : len(text)-len(evidenceFooterEnd)]
	var stored EvidenceIndex
	if err := json.Unmarshal([]byte(payload), &stored, json.RejectUnknownMembers(true)); err != nil {
		return "", fmt.Errorf("invalid generated evidence footer: %w", err)
	}
	actual, err := json.Marshal(stored)
	if err != nil {
		return "", err
	}
	want, err := json.Marshal(expected)
	if err != nil {
		return "", err
	}
	if expected.Version != 1 || !bytes.Equal(actual, want) {
		return "", fmt.Errorf("evidence footer does not match resolved review identities")
	}
	return text[:start], nil
}

// BuildEvidenceIndex hashes complete blobs selected at immutable Git endpoints.
func (b *Builder) BuildEvidenceIndex(gitRef string, excludes []string) (EvidenceIndex, error) {
	// Required manifests must not silently degrade to optional binary evidence.
	// The pinned-rendition loader extends this boundary in the following change.
	if b.reviewSettings != nil && b.reviewSettings.EvidenceManifest != "" {
		return EvidenceIndex{}, fmt.Errorf("required evidence manifests are not supported by this build")
	}
	target, err := git.ResolveReviewEndpoints(b.context(), b.repoPath, gitRef)
	if err != nil {
		return EvidenceIndex{}, err
	}
	changes, err := git.ChangedBinaryEvidence(b.context(), b.repoPath, target, excludes)
	if err != nil {
		return EvidenceIndex{}, err
	}
	index := EvidenceIndex{Version: 1, Target: target}
	for _, change := range changes {
		blobs := append([]git.EvidenceBlob{}, change.Old...)
		if change.New != nil {
			blobs = append(blobs, *change.New)
		}
		for _, blob := range blobs {
			entry := EvidenceEntry{Path: blob.Path, Kind: change.Kind, Side: "new", BlobOID: blob.OID}
			if !utf8.ValidString(blob.Path) {
				entry.Path = strconv.Quote(blob.Path)
				entry.PathBytes = []byte(blob.Path)
			}
			if blob.Parent != 0 {
				entry.Side = fmt.Sprintf("old parent %d", blob.Parent)
			}
			if blob.Mode != "100644" && blob.Mode != "100755" {
				entry.Omission = "symlink or submodule; not copied as an ordinary file"
			} else if strings.HasPrefix(change.Omission, "mode-only") {
				entry.Omission = change.Omission
			} else {
				digest := newEvidenceDigest()
				if err := git.CopyBlob(b.context(), b.repoPath, blob.OID, digest); err != nil {
					return EvidenceIndex{}, err
				}
				entry.SHA256, entry.Bytes = digest.sum(), digest.bytes
				entry.MediaHint = evidenceMedia(digest.prefix)
				entry.File, err = evidenceFilename(entry.SHA256, entry.MediaHint)
				if err != nil {
					return EvidenceIndex{}, err
				}
			}
			index.Entries = append(index.Entries, entry)
		}
	}
	return index, nil
}

type evidenceDigest struct {
	hash   hash.Hash
	bytes  int64
	prefix []byte
}

func newEvidenceDigest() *evidenceDigest { return &evidenceDigest{hash: sha256.New()} }

func (d *evidenceDigest) Write(data []byte) (int, error) {
	// DetectContentType inspects at most 512 bytes; the hash includes all bytes.
	if len(d.prefix) < 512 {
		d.prefix = append(d.prefix, data[:min(len(data), 512-len(d.prefix))]...)
	}
	n, err := d.hash.Write(data)
	d.bytes += int64(n)
	return n, err
}

func (d *evidenceDigest) sum() string { return hex.EncodeToString(d.hash.Sum(nil)) }

func evidenceMedia(prefix []byte) string {
	media := http.DetectContentType(prefix)
	switch media {
	case "application/pdf", "image/png", "image/jpeg", "image/gif", "image/webp":
		return media
	default:
		return media + "; text rendition required for unsupported native reading"
	}
}

func evidenceFilename(sum, media string) (string, error) {
	decoded, err := hex.DecodeString(sum)
	if err != nil || len(decoded) != sha256.Size || sum != strings.ToLower(sum) {
		return "", fmt.Errorf("invalid evidence SHA256")
	}
	extension := ".bin"
	switch media {
	case "application/pdf":
		extension = ".pdf"
	case "image/png":
		extension = ".png"
	case "image/jpeg":
		extension = ".jpg"
	case "image/gif":
		extension = ".gif"
	case "image/webp":
		extension = ".webp"
	}
	return sum + extension, nil
}

// MaterializeEvidence streams and verifies pinned bytes under the actual agent
// checkout, including when the surrounding prompt is small enough for stdin.
// The returned cleanup owns the whole attempt and is safe to call repeatedly.
func (b *Builder) MaterializeEvidence(index EvidenceIndex, target SnapshotTarget) (section string, cleanup func(), err error) {
	if index.Version != 1 {
		return "", nil, fmt.Errorf("unsupported evidence index version %d", index.Version)
	}
	if err := b.context().Err(); err != nil {
		return "", nil, err
	}
	if err := index.validatePaths(); err != nil {
		return "", nil, err
	}
	portable, err := json.Marshal(index)
	if err != nil {
		return "", nil, fmt.Errorf("encode evidence identities: %w", err)
	}
	repo, root, err := b.resolveSnapshotTarget(target)
	if err != nil {
		return "", nil, err
	}
	// Reuse root confinement, ignored-root fallback, private directory, marker,
	// active registration and stale cleanup from ordinary prompt snapshots.
	anchor, remove, err := writeExternalSnapshot(repo, root, "evidence.json", string(portable))
	if err != nil {
		return "", nil, err
	}
	defer func() {
		if err != nil {
			remove()
		}
	}()
	dir := filepath.Dir(anchor)
	var text strings.Builder
	text.WriteString("\n\n## Committed review evidence\n\nFiles below are available for native reading. Hashes identify offered raw bytes; they do not establish that a file was read. Unsupported media needs a supplied text rendition.\n")
	fmt.Fprintf(&text, "\nIndex version: %d; head: %s; bases: %q\n", index.Version, index.Target.Head, index.Target.Bases)
	files := make(map[string]string)
	for _, entry := range index.Entries {
		if err := b.context().Err(); err != nil {
			return "", nil, err
		}
		path, err := entry.originalPath()
		if err != nil {
			return "", nil, err
		}
		fmt.Fprintf(&text, "\n- Path: %q; change: %q; side: %q; blob: %q", path, entry.Kind, entry.Side, entry.BlobOID)
		if entry.Omission != "" {
			fmt.Fprintf(&text, "; omission: %q\n", entry.Omission)
			continue
		}
		name, err := evidenceFilename(entry.SHA256, entry.MediaHint)
		if err != nil {
			return "", nil, err
		}
		if entry.File != name {
			return "", nil, fmt.Errorf("evidence filename does not match raw byte identity")
		}

		// Always rehash, even when an earlier entry claims the same content hash.
		// Unverified metadata must not cause deduplication to skip a blob read.
		f, err := os.CreateTemp(dir, "blob-*")
		if err != nil {
			return "", nil, fmt.Errorf("create evidence file: %w", err)
		}
		digest := newEvidenceDigest()
		copyErr := git.CopyBlob(b.context(), b.repoPath, entry.BlobOID, io.MultiWriter(f, digest))
		closeErr := f.Close()
		if copyErr != nil {
			return "", nil, copyErr
		}
		if closeErr != nil {
			return "", nil, closeErr
		}
		if digest.sum() != entry.SHA256 || digest.bytes != entry.Bytes || evidenceMedia(digest.prefix) != entry.MediaHint {
			return "", nil, fmt.Errorf("committed evidence bytes do not match index for %q", path)
		}
		file := files[entry.SHA256]
		if file == "" {
			file = filepath.Join(dir, name)
			if err := atomicfile.Replace(f.Name(), file); err != nil {
				return "", nil, err
			}
			files[entry.SHA256] = file
		} else if err := os.Remove(f.Name()); err != nil {
			return "", nil, err
		}
		fmt.Fprintf(&text, "; SHA256: %s; bytes: %d; media: %q; file: %q\n", entry.SHA256, entry.Bytes, entry.MediaHint, file)
	}
	return text.String(), remove, nil
}

// WithReviewSettings overrides resolved settings, including explicit off values.
// It is used for frozen panel and experiment snapshots.
func (b *Builder) WithReviewSettings(settings config.ReviewSettings) *Builder {
	next := *b
	settings.Tools = settings.Tools.Clone()
	next.reviewSettings = &settings
	return &next
}

func (b *Builder) resolveReviewSettings(reviewType string) (config.ReviewSettings, error) {
	if b.reviewSettings != nil {
		return *b.reviewSettings, nil
	}
	if config.IsBuiltInReviewType(reviewType) {
		return config.ReviewSettings{}, nil
	}
	repoCfg, err := b.resolveRepoConfig()
	if err != nil {
		return config.ReviewSettings{}, err
	}
	return config.ResolveReviewSettings(reviewType, nil, repoCfg, b.globalCfg)
}

// BindReviewEvidence validates portable identities against independently resolved
// settings and Git endpoints, then materializes files for this attempt. Additional
// instructions go before the generated footer so persisted prompts remain reusable.
// Call Prepare on Prompt only after all other runtime attachments have been bound.
func (b *Builder) BindReviewEvidence(text, gitRef, reviewType string, excludes []string, target SnapshotTarget, additions ...string) (SnapshotResult, error) {
	settings, err := b.resolveReviewSettings(reviewType)
	if err != nil {
		return SnapshotResult{}, err
	}
	if !settings.BinaryEvidence && settings.EvidenceManifest == "" {
		if strings.HasSuffix(text, evidenceFooterEnd) && strings.Contains(text, evidenceFooterStart) {
			return SnapshotResult{}, fmt.Errorf("evidence footer present for evidence-disabled review")
		}
		return SnapshotResult{Prompt: text + strings.Join(additions, "")}, nil
	}
	if excludes == nil {
		excludes = b.resolveExcludes(reviewType)
	}
	b = b.WithReviewSettings(settings)
	expected, err := b.BuildEvidenceIndex(gitRef, excludes)
	if err != nil {
		return SnapshotResult{}, err
	}
	body, err := evidencePromptBody(text, expected)
	if err != nil {
		return SnapshotResult{}, err
	}
	suffix := strings.Join(additions, "")
	stored, err := AppendEvidenceIndex(strings.TrimSuffix(body, suffix)+suffix, expected)
	if err != nil {
		return SnapshotResult{}, err
	}
	runtime, cleanup, err := b.BindEvidenceIndex(stored, expected, target)
	if err != nil {
		return SnapshotResult{}, err
	}
	return SnapshotResult{Prompt: runtime, StoredPrompt: stored, Cleanup: cleanup}, nil
}
