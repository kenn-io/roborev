package prompt

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/roborev/internal/git"
)

// ErrBinaryEvidenceDirty rejects binary evidence for uncommitted changes,
// which have no committed bytes to hand the reviewer.
var ErrBinaryEvidenceDirty = errors.New(
	"binary_evidence needs committed files; it cannot be used to review uncommitted changes",
)

// WithBinaryEvidence returns a builder that gives committed reviews the
// committed bytes of the binary files they change.
func (b *Builder) WithBinaryEvidence(enabled bool) *Builder {
	next := *b
	next.binaryEvidence = enabled
	return &next
}

// AppendBinaryEvidence writes the committed version of each binary file that
// gitRef changes into a snapshot directory of target and appends a section
// telling the reviewer where to read them. Bytes come from Git objects, never
// the working tree. Text is returned unchanged when binary evidence is off or
// no binary file changed. The caller runs cleanup after the agent finishes.
func (b *Builder) AppendBinaryEvidence(text, gitRef string, excludes []string, target SnapshotTarget) (string, func(), error) {
	if !b.binaryEvidence {
		return text, nil, nil
	}
	changes, err := git.ChangedBinaryFiles(b.context(), b.repoPath, gitRef, excludes...)
	if err != nil || len(changes) == 0 {
		return text, nil, err
	}

	var dir string
	var cleanup func()
	var lines []string
	for i, change := range changes {
		if change.OID == "" {
			lines = append(lines, fmt.Sprintf("- %q (%s)", change.Path, change.Status))
			continue
		}
		if cleanup == nil {
			repoPath, root, err := b.resolveSnapshotTarget(target)
			if err == nil {
				dir, cleanup, err = createSnapshotDir(repoPath, root)
			}
			if err != nil {
				return "", nil, fmt.Errorf("binary evidence: %w", err)
			}
		}
		data, err := git.ReadBlob(b.context(), b.repoPath, change.OID)
		if err == nil {
			file := filepath.Join(dir, evidenceFileName(i+1, change.Path))
			if err = writeSnapshotFile(file, data); err == nil {
				lines = append(lines, fmt.Sprintf("- %q (%s): read %q", change.Path, change.Status, file))
			}
		}
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("binary evidence %s: %w", change.Path, err)
		}
	}

	section := "\n\n## Committed review evidence\n\n" +
		"The diff cannot show these binary files. Each saved copy holds the bytes " +
		"committed in the reviewed changes; read it instead of the file in the " +
		"working tree, which may differ. Deleted files have no copy.\n\n" +
		strings.Join(lines, "\n") + "\n"
	return text + section, cleanup, nil
}

// evidenceFileName names the saved copy of the n-th changed binary file. It
// keeps the committed base name so readers recognize the type, but only when
// that name is a single safe path element on every OS. Git allows "\" and ":"
// in file names, and Windows would treat them as separators or drive and stream
// markers, placing the copy outside the snapshot directory. Other names fall
// back to n plus the extension when it is plain ASCII letters and digits.
func evidenceFileName(n int, repoPath string) string {
	name := path.Base(repoPath)
	if filepath.IsLocal(name) && !strings.ContainsAny(name, `\/:`) {
		return trimEvidenceFileName(fmt.Sprintf("%d-", n), name)
	}
	ext := strings.TrimPrefix(path.Ext(name), ".")
	plain := ext != "" && !strings.ContainsFunc(ext, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	})
	if !plain {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%d.%s", n, ext)
}

// maxEvidenceFileNameBytes is the file name limit on ext4 and APFS (NAME_MAX).
// A committed base name can already use all of it, and the n- prefix would
// push the copy's name past the limit and fail the write.
const maxEvidenceFileNameBytes = 255

// trimEvidenceFileName shortens the name part of prefix+name to fit the file
// name limit, keeping the extension and cutting on a UTF-8 boundary.
func trimEvidenceFileName(prefix, name string) string {
	if len(prefix)+len(name) <= maxEvidenceFileNameBytes {
		return prefix + name
	}
	ext := path.Ext(name)
	if len(prefix)+len(ext) >= maxEvidenceFileNameBytes {
		ext = ""
	}
	stem := strings.TrimSuffix(name, ext)
	keep := maxEvidenceFileNameBytes - len(prefix) - len(ext)
	for keep > 0 && !utf8.RuneStart(stem[keep]) {
		keep--
	}
	return prefix + stem[:keep] + ext
}
