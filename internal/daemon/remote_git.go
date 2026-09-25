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

	"go.kenn.io/roborev/internal/procutil"
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
	fetchRemotes(ctx, repoRoot)
	return missingCommits(ctx, repoRoot, shas)
}

// fetchRemotes runs "git fetch --all" with the user's environment and git
// config, like the CI poller's fetch, so credential helpers, insteadOf
// rewrites, and core.sshCommand apply. A failed fetch is logged: the caller
// can still upload the commits as a pack.
func fetchRemotes(ctx context.Context, repoRoot string) {
	unlock := lockGitMetadata(repoRoot)
	defer unlock()
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "fetch", "--all", "--quiet")
	procutil.HideConsole(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("remote enqueue: fetch in %s failed: %v: %s", repoRoot, err, strings.TrimSpace(string(out)))
		return
	}
	pruneUploadRefsLocked(ctx, repoRoot)
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

// pruneUploadRefsLocked drops upload refs whose commit a remote-tracking
// branch now contains. Failures are logged because pruning is housekeeping.
// The caller must hold lockGitMetadata(repoRoot), which is not re-entrant.
func pruneUploadRefsLocked(ctx context.Context, repoRoot string) {
	out, err := gitcmd.New().Output(ctx, repoRoot, "for-each-ref",
		"--format=%(refname) %(objectname)", uploadRefPrefix)
	if err != nil {
		log.Printf("remote uploads: list upload refs in %s: %v", repoRoot, err)
		return
	}
	for line := range strings.Lines(string(out)) {
		refname, sha, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		contains, err := gitcmd.New().Output(ctx, repoRoot, "for-each-ref",
			"--count=1", "--contains", sha, "--format=%(refname)", "refs/remotes/")
		if err != nil {
			log.Printf("remote uploads: check %s in %s: %v", sha, repoRoot, err)
			continue
		}
		if strings.TrimSpace(string(contains)) == "" {
			continue
		}
		if _, _, err := gitcmd.New().Run(ctx, repoRoot, nil, "update-ref", "-d", refname); err != nil {
			log.Printf("remote uploads: delete %s in %s: %v", refname, repoRoot, err)
		}
	}
}

// missingBaseError reports a pack whose tip needs objects that neither the
// pack nor the daemon clone has. err is git's report, kept for logging.
type missingBaseError struct {
	tip string
	err error
}

func (e *missingBaseError) Error() string {
	return fmt.Sprintf("daemon clone lacks base commits for %s; fetch on the daemon host", e.tip)
}

func (e *missingBaseError) Unwrap() error { return e.err }

// badPackError reports a pack that git index-pack rejected, which is a
// problem with the caller's upload rather than with the daemon.
type badPackError struct{ err error }

func (e *badPackError) Error() string { return fmt.Sprintf("index pack: %v", e.err) }

func (e *badPackError) Unwrap() error { return e.err }

// readErrRecorder remembers the first read error other than io.EOF.
type readErrRecorder struct {
	r   io.Reader
	err error
}

func (rr *readErrRecorder) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && rr.err == nil {
		rr.err = err
	}
	return n, err
}

// importPack stores a caller-supplied git pack in the clone's object store
// and pins each tip under refs/roborev/uploads/. It never touches the
// working tree, the index, branches, or remote-tracking refs.
func importPack(ctx context.Context, repoRoot string, pack io.Reader, tips []string) error {
	for _, tip := range tips {
		if !isFullSHA(tip) {
			return fmt.Errorf("pack tip %q: %w", tip, errRemoteRefNotSHA)
		}
	}
	unlock := lockGitMetadata(repoRoot)
	defer unlock()
	body := &readErrRecorder{r: pack}
	if _, _, err := gitcmd.New().Run(ctx, repoRoot, body, "index-pack", "--stdin"); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("index pack: %w", ctxErr)
		}
		// A failed read ends index-pack's input early, so git reports a
		// truncated pack; the read error is the real cause.
		if body.err != nil {
			return fmt.Errorf("read pack: %w", body.err)
		}
		// Only index-pack exiting with a failure means it rejected the
		// input. Anything else, such as git not starting, is a daemon-side
		// failure.
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			return &badPackError{err: err}
		}
		return fmt.Errorf("index pack: %w", err)
	}
	for _, tip := range tips {
		_, _, err := gitcmd.New().Run(ctx, repoRoot, nil,
			"rev-list", "--quiet", "--objects", tip, "--not", "--all")
		if err == nil {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("check pack tip %s: %w", tip, ctxErr)
		}
		// Only a git run that exited with a failure means objects are
		// missing. Anything else, such as git not starting, is returned as is.
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			return &missingBaseError{tip: tip, err: err}
		}
		return fmt.Errorf("check pack tip %s: %w", tip, err)
	}
	for _, tip := range tips {
		if _, _, err := gitcmd.New().Run(ctx, repoRoot, nil, "update-ref", uploadRefPrefix+tip, tip); err != nil {
			return fmt.Errorf("pin %s: %w", tip, err)
		}
	}
	return nil
}
