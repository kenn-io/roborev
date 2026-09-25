package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

var errRemoteRefNotSHA = errors.New("remote enqueue needs full commit SHAs")

var fullSHAPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func isFullSHA(s string) bool { return fullSHAPattern.MatchString(s) }

// parseRemoteGitRef returns the commits a remote enqueue names. Remote
// callers must send full SHAs because symbolic refs would resolve in the
// daemon's clone rather than the caller's. The inclusive "<sha>^..<sha>"
// form names <sha> itself; the enqueue handler's empty-tree fallback covers
// a root <sha>.
func parseRemoteGitRef(ref string) ([]string, error) {
	start, end, isRange := strings.Cut(ref, "..")
	if !isRange {
		if !isFullSHA(ref) {
			return nil, errRemoteRefNotSHA
		}
		return []string{ref}, nil
	}
	start = strings.TrimSuffix(start, "^")
	if !isFullSHA(start) || !isFullSHA(end) {
		return nil, errRemoteRefNotSHA
	}
	return []string{start, end}, nil
}

// missingCommits returns the SHAs the clone lacks. rev-parse --verify
// --quiet exits 1 for a missing object and 128 for a real failure, such as
// a path that is not a repository, so only exit 1 counts as missing.
func missingCommits(ctx context.Context, repoRoot string, shas []string) ([]string, error) {
	var missing []string
	for _, sha := range shas {
		_, _, err := gitcmd.New().Run(ctx, repoRoot, nil,
			"rev-parse", "--verify", "--quiet", sha+"^{commit}")
		if err == nil {
			continue
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
			missing = append(missing, sha)
			continue
		}
		return nil, fmt.Errorf("check commit %s in %s: %w", sha, repoRoot, err)
	}
	return missing, nil
}

// ensureRemoteCommits fetches once from the clone's configured remotes when
// any named commit is missing, and returns the commits still missing.
func ensureRemoteCommits(ctx context.Context, repoRoot string, shas []string) ([]string, error) {
	missing, err := missingCommits(ctx, repoRoot, shas)
	if err != nil || len(missing) == 0 {
		return missing, err
	}
	if _, _, err := gitcmd.New().Run(ctx, repoRoot, nil, "fetch", "--all", "--quiet"); err != nil {
		log.Printf("remote enqueue: fetch in %s failed: %v", repoRoot, err)
	} else {
		pruneUploadRefs(ctx, repoRoot)
	}
	return missingCommits(ctx, repoRoot, shas)
}

// daemonHaves lists the distinct commits at the clone's ref tips, peeling
// annotated tags and skipping refs that point at non-commits.
func daemonHaves(ctx context.Context, repoRoot string) ([]string, error) {
	out, err := gitcmd.New().Output(ctx, repoRoot, "for-each-ref",
		"--format=%(objecttype) %(objectname) %(*objecttype) %(*objectname)")
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}
	seen := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) >= 2 && fields[0] == "commit":
			seen[fields[1]] = true
		case len(fields) == 4 && fields[2] == "commit":
			seen[fields[3]] = true
		}
	}
	haves := make([]string, 0, len(seen))
	for sha := range seen {
		haves = append(haves, sha)
	}
	slices.Sort(haves)
	return haves, nil
}

const uploadRefPrefix = "refs/roborev/uploads/"

// pruneUploadRefs drops upload refs whose commit a remote-tracking branch
// now contains. Failures are logged because pruning is housekeeping.
func pruneUploadRefs(ctx context.Context, repoRoot string) {
	out, err := gitcmd.New().Output(ctx, repoRoot, "for-each-ref", "--format=%(objectname)", uploadRefPrefix)
	if err != nil {
		log.Printf("remote uploads: list upload refs in %s: %v", repoRoot, err)
		return
	}
	for sha := range strings.FieldsSeq(string(out)) {
		contains, err := gitcmd.New().Output(ctx, repoRoot, "for-each-ref",
			"--count=1", "--contains", sha, "--format=%(refname)", "refs/remotes/")
		if err != nil {
			log.Printf("remote uploads: check %s in %s: %v", sha, repoRoot, err)
			continue
		}
		if strings.TrimSpace(string(contains)) == "" {
			continue
		}
		if _, _, err := gitcmd.New().Run(ctx, repoRoot, nil, "update-ref", "-d", uploadRefPrefix+sha); err != nil {
			log.Printf("remote uploads: delete %s in %s: %v", uploadRefPrefix+sha, repoRoot, err)
		}
	}
}

type missingBaseError struct{ tip string }

func (e *missingBaseError) Error() string {
	return fmt.Sprintf("daemon clone lacks base commits for %s; fetch on the daemon host", e.tip)
}

// importPack stores a caller-supplied git pack in the clone's object store
// and pins each tip under refs/roborev/uploads/. It never touches the
// working tree, the index, branches, or remote-tracking refs.
func importPack(ctx context.Context, repoRoot string, pack io.Reader, tips []string) error {
	if _, _, err := gitcmd.New().Run(ctx, repoRoot, pack, "index-pack", "--stdin"); err != nil {
		return fmt.Errorf("index pack: %w", err)
	}
	for _, tip := range tips {
		if _, _, err := gitcmd.New().Run(ctx, repoRoot, nil,
			"rev-list", "--quiet", "--objects", tip, "--not", "--all"); err != nil {
			return &missingBaseError{tip: tip}
		}
	}
	for _, tip := range tips {
		if _, _, err := gitcmd.New().Run(ctx, repoRoot, nil, "update-ref", uploadRefPrefix+tip, tip); err != nil {
			return fmt.Errorf("pin %s: %w", tip, err)
		}
	}
	return nil
}
