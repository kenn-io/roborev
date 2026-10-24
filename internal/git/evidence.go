package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ReviewEndpoints pins the exact objects selected by a committed review.
// Bases is empty for a root, the selected start for a range, or all parents
// for a single commit. A merge retains combined-diff selection semantics.
type ReviewEndpoints struct {
	Head  string   `json:"head"`
	Bases []string `json:"bases"`
}

// EvidenceBlob identifies a tree entry. Parent is one-based for old sides.
type EvidenceBlob struct {
	Path, OID, Mode string
	Parent          int
}

// EvidenceChange describes a changed binary or an explicitly omitted tree entry.
type EvidenceChange struct {
	Kind     string
	Old      []EvidenceBlob
	New      *EvidenceBlob
	Omission string
}

// ResolveReviewEndpoints resolves symbolic refs before any evidence reads.
func ResolveReviewEndpoints(ctx context.Context, repoPath, gitRef string) (ReviewEndpoints, error) {
	resolve := func(ref string) (string, error) {
		if ref == "" {
			ref = "HEAD"
		}
		cmd := evidenceGitCommand(ctx, repoPath, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
		out, err := gitOutput(cmd)
		if err != nil {
			return "", fmt.Errorf("resolve evidence commit: %w", err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	separator := ".."
	if strings.Contains(gitRef, "...") {
		separator = "..."
	}
	if start, end, ranged := strings.Cut(gitRef, separator); ranged {
		base := start
		// The daemon uses the empty tree as the base of a root-inclusive range.
		// Other bases, including triple-dot merge-base inputs, must be commits.
		if separator != ".." || start != EmptyTreeSHA {
			var err error
			base, err = resolve(start)
			if err != nil {
				return ReviewEndpoints{}, err
			}
		}
		head, err := resolve(end)
		if err != nil {
			return ReviewEndpoints{}, err
		}
		if separator == "..." {
			out, err := gitOutput(evidenceGitCommand(ctx, repoPath, "merge-base", base, head))
			if err != nil {
				return ReviewEndpoints{}, fmt.Errorf("resolve evidence merge base: %w", err)
			}
			base = strings.TrimSpace(string(out))
		}
		return ReviewEndpoints{Head: head, Bases: []string{base}}, nil
	}
	head, err := resolve(gitRef)
	if err != nil {
		return ReviewEndpoints{}, err
	}
	out, err := gitOutput(evidenceGitCommand(ctx, repoPath, "show", "-s", "--format=%P", head))
	if err != nil {
		return ReviewEndpoints{}, fmt.Errorf("resolve evidence parents: %w", err)
	}
	var bases []string
	bases = append(bases, strings.Fields(string(out))...)
	return ReviewEndpoints{Head: head, Bases: bases}, nil
}

// WithEvidenceReadPolicy scopes immutable, non-executing Git reads to a
// committed evidence review. Ordinary review contexts retain their behavior.
func WithEvidenceReadPolicy(ctx context.Context) context.Context {
	if evidenceReadPolicy(ctx) {
		return ctx
	}
	return context.WithValue(ctx, evidenceReadPolicyKey{}, true)
}

type evidenceReadPolicyKey struct{}

func evidenceReadPolicy(ctx context.Context) bool {
	enabled, _ := ctx.Value(evidenceReadPolicyKey{}).(bool)
	return enabled
}

func evidenceGitCommand(ctx context.Context, repoPath string, args ...string) *exec.Cmd {
	cmd := newGitCmdContext(WithEvidenceReadPolicy(ctx), args...)
	cmd.Dir = repoPath
	return cmd
}

func evidenceOID(oid string) bool {
	// Git's supported object formats are SHA-1 and SHA-256.
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, c := range oid {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return strings.Trim(oid, "0") != ""
}

// CopyBlob streams raw committed bytes, never a filtered working-tree file.
func CopyBlob(ctx context.Context, repoPath, oid string, dst io.Writer) error {
	if !evidenceOID(oid) {
		return fmt.Errorf("invalid evidence blob identity")
	}
	cmd := evidenceGitCommand(ctx, repoPath, "cat-file", "blob", oid)
	var stderr bytes.Buffer
	cmd.Stdout = dst
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read evidence blob %s: %w: %s", oid, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ChangedBinaryEvidence uses native Git classification with the selected
// endpoints' committed attributes. A private index provides fallback, without
// consulting the current worktree, info attributes, or global attributes.
func ChangedBinaryEvidence(ctx context.Context, repoPath string, target ReviewEndpoints, excludes []string) ([]EvidenceChange, error) {
	if !evidenceOID(target.Head) {
		return nil, fmt.Errorf("invalid evidence head identity")
	}
	for _, base := range target.Bases {
		if !evidenceOID(base) {
			return nil, fmt.Errorf("invalid evidence base identity")
		}
	}
	command, cleanup, err := evidenceAttributeContext(ctx, repoPath, target.Head)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	args := []string{"diff-tree", "--no-commit-id", "--root", "-r", "--no-ext-diff", "--no-textconv", "--no-abbrev"}
	parents := 1
	if len(target.Bases) > 1 {
		parents = len(target.Bases)
		args = append(args, "--cc", "--combined-all-paths", target.Head)
	} else {
		args = append(args, target.Bases...)
		args = append(args, target.Head)
	}
	run := func(format string) ([]byte, error) {
		argv := append(append([]string{}, args...), format, "-z", "--")
		argv = append(argv, ReviewPathspecArgs(excludes...)...)
		return gitOutput(command(argv...))
	}
	raw, err := run("--raw")
	if err != nil {
		return nil, fmt.Errorf("enumerate evidence changes: %w", err)
	}
	changes, err := parseEvidenceRaw(raw, parents)
	if err != nil {
		return nil, err
	}
	numstat, err := run("--numstat")
	if err != nil {
		return nil, fmt.Errorf("classify evidence changes: %w", err)
	}
	binary, err := evidenceBinaryPaths(numstat)
	if err != nil {
		return nil, err
	}
	// Deleted attributes, and changes to a path's classification, must not hide
	// evidence identified by an old endpoint. Reuse the same private index.
	for _, base := range target.Bases {
		if _, err := gitOutput(command("read-tree", base)); err != nil {
			return nil, fmt.Errorf("read old evidence attributes: %w", err)
		}
		stats, err := run("--numstat")
		if err != nil {
			return nil, fmt.Errorf("classify old evidence: %w", err)
		}
		paths, err := evidenceBinaryPaths(stats)
		if err != nil {
			return nil, err
		}
		for path := range paths {
			binary[path] = true
		}
	}
	var result []EvidenceChange
	for _, change := range changes {
		path := ""
		if change.New != nil {
			path = change.New.Path
		} else if len(change.Old) > 0 {
			path = change.Old[0].Path
		}
		for _, blob := range append(append([]EvidenceBlob{}, change.Old...), evidenceNewBlobs(change.New)...) {
			if blob.Mode != "100644" && blob.Mode != "100755" {
				change.Omission = "symlink or submodule; not copied as an ordinary file"
			}
		}
		if change.New != nil && len(change.Old) > 0 {
			same := true
			for _, old := range change.Old {
				same = same && old.OID == change.New.OID && old.Path == change.New.Path
			}
			if same {
				change.Omission = "mode-only change; no content copied"
			}
		}
		if binary[path] || change.Omission != "" {
			result = append(result, change)
		}
	}
	return result, nil
}

func evidenceNewBlobs(blob *EvidenceBlob) []EvidenceBlob {
	if blob == nil {
		return nil
	}
	return []EvidenceBlob{*blob}
}

// evidenceAttributeContext borrows only the object store. No source checkout or
// user index is copied or changed. Only native rename policy is carried over.
func evidenceAttributeContext(ctx context.Context, repoPath, head string) (func(...string) *exec.Cmd, func(), error) {
	objects, err := gitOutput(evidenceGitCommand(ctx, repoPath, "rev-parse", "--path-format=absolute", "--git-path", "objects"))
	if err != nil {
		return nil, nil, fmt.Errorf("locate evidence objects: %w", err)
	}
	format, err := gitOutput(evidenceGitCommand(ctx, repoPath, "rev-parse", "--show-object-format"))
	if err != nil {
		return nil, nil, err
	}
	// Preserve only native review rename policy. Attribute files, diff drivers,
	// external commands and other ambient configuration remain isolated.
	renameConfig, err := gitOutput(evidenceGitCommand(ctx, repoPath, "config", "--null", "--get-regexp", `^diff\.(renames|renamelimit)$`))
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, nil, fmt.Errorf("read review rename settings: %w", err)
		}
	}
	var renameArgs []string
	var renameValue string
	var hasRenameValue bool
	for entry := range strings.SplitSeq(string(renameConfig), "\x00") {
		key, value, hasValue := strings.Cut(entry, "\n")
		if key == "" {
			continue
		}
		argument := key
		if hasValue {
			argument += "=" + value
		}
		renameArgs = append(renameArgs, "-c", argument)
		if key == "diff.renames" {
			renameValue = value
			if !hasValue {
				renameValue = "true"
			}
			hasRenameValue = true
		}
	}
	renameFlag := "-M"
	if hasRenameValue {
		if strings.EqualFold(renameValue, "copy") || strings.EqualFold(renameValue, "copies") {
			renameFlag = "-C"
		} else {
			// Parse only the effective value. Parsing ambient matches as booleans can
			// reject a valid copy mode even when a higher-priority boolean overrides it.
			// An empty explicit stdin config prevents lower-priority values or includes
			// from participating in this native boolean conversion.
			boolean, err := gitOutput(evidenceGitCommand(ctx, repoPath, "config", "--file=-", "--type=bool", "--default="+renameValue, "--get", "diff.renames"))
			if err != nil {
				return nil, nil, fmt.Errorf("read review rename mode: %w", err)
			}
			if strings.TrimSpace(string(boolean)) == "false" {
				renameFlag = "--no-renames"
			}
		}
	}
	dir, err := os.MkdirTemp("", "roborev-evidence-git-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	command := func(args ...string) *exec.Cmd {
		cmd := evidenceGitCommand(ctx, dir, args...)
		cmd.Env = append(cmd.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_ATTR_NOSYSTEM=1")
		return cmd
	}
	if _, err := gitOutput(command("init", "--bare", "--template=", "--object-format="+strings.TrimSpace(string(format)), dir)); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("initialize evidence metadata: %w", err)
	}
	objectPath := strings.TrimSuffix(string(objects), "\n")
	if err := os.WriteFile(filepath.Join(dir, "objects", "info", "alternates"), []byte(strconv.Quote(objectPath)+"\n"), 0o600); err != nil {
		cleanup()
		return nil, nil, err
	}
	worktree := filepath.Join(dir, "worktree")
	if err := os.Mkdir(worktree, 0o700); err != nil {
		cleanup()
		return nil, nil, err
	}
	baseCommand := command
	command = func(args ...string) *exec.Cmd {
		prefix := []string{"--git-dir=" + dir, "--work-tree=" + worktree, "-c", "core.bare=false", "-c", "core.attributesFile=" + os.DevNull}
		prefix = append(prefix, renameArgs...)
		if len(args) > 0 && args[0] == "diff-tree" {
			args = append([]string{args[0], renameFlag}, args[1:]...)
		}
		return baseCommand(append(prefix, args...)...)
	}
	if _, err := gitOutput(command("read-tree", head)); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("read evidence attribute tree: %w", err)
	}
	return command, cleanup, nil
}

func parseEvidenceRaw(data []byte, parents int) ([]EvidenceChange, error) {
	fields := strings.Split(string(data), "\x00")
	var changes []EvidenceChange
	for i := 0; i < len(fields)-1; {
		header := strings.Fields(fields[i])
		i++
		if len(header) != 2*(parents+1)+1 || !strings.HasPrefix(header[0], strings.Repeat(":", parents)) {
			return nil, fmt.Errorf("invalid evidence raw metadata")
		}
		header[0] = strings.TrimLeft(header[0], ":")
		kind := header[len(header)-1]
		paths := 1
		if parents > 1 {
			paths = parents + 1
		} else if strings.HasPrefix(kind, "R") || strings.HasPrefix(kind, "C") {
			paths = 2
		}
		if i+paths >= len(fields) {
			return nil, fmt.Errorf("missing evidence raw path")
		}
		change := EvidenceChange{Kind: kind}
		for p := range parents {
			path := fields[i]
			if parents > 1 {
				path = fields[i+p]
			}
			if header[p] != "000000" {
				change.Old = append(change.Old, EvidenceBlob{Path: path, Mode: header[p], OID: header[parents+1+p], Parent: p + 1})
			}
		}
		if header[parents] != "000000" {
			change.New = &EvidenceBlob{Path: fields[i+paths-1], Mode: header[parents], OID: header[2*parents+1]}
		}
		i += paths
		changes = append(changes, change)
	}
	return changes, nil
}

func evidenceBinaryPaths(data []byte) (map[string]bool, error) {
	fields := strings.Split(string(data), "\x00")
	result := make(map[string]bool)
	for i := 0; i < len(fields)-1; i++ {
		parts := strings.SplitN(fields[i], "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid evidence numstat metadata")
		}
		path := parts[2]
		if path == "" {
			if i+2 >= len(fields)-1 {
				return nil, fmt.Errorf("missing evidence rename paths")
			}
			i += 2
			path = fields[i]
		}
		if parts[0] == "-" && parts[1] == "-" {
			result[path] = true
		}
	}
	return result, nil
}
