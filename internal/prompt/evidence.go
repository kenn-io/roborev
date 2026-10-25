package prompt

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

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
			// Keep the original name and extension so readers recognize the type.
			file := filepath.Join(dir, fmt.Sprintf("%d-%s", i+1, path.Base(change.Path)))
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
