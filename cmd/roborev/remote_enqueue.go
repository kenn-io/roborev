package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

// remoteRepoIdentity returns the identity a remote daemon matches for the
// checkout at root. A local:// fallback can never match another host.
func remoteRepoIdentity(root string) (string, error) {
	id := config.ResolveRepoIdentity(root, nil)
	if strings.HasPrefix(id, "local://") {
		return "", fmt.Errorf(
			"repo at %s has no remote URL or .roborev-id, so a remote daemon cannot identify it; add a .roborev-id file matching the daemon host's checkout",
			root)
	}
	return id, nil
}

// resolveRemoteGitRef resolves refs to full SHAs locally, since the daemon
// would resolve names in its own clone. It keeps the inclusive START^..END
// form so the daemon's empty-tree fallback still covers a root START.
func resolveRemoteGitRef(ctx context.Context, root, ref string) (string, error) {
	if strings.Contains(ref, "...") {
		return "", fmt.Errorf(
			"git ref %q is a symmetric range; symmetric ranges (A...B) are not supported with a remote daemon, use A..B",
			ref)
	}
	resolve := func(r string) (string, error) {
		if r == "" {
			return "", fmt.Errorf("git ref %q has an empty side; use A..B with both sides set", ref)
		}
		// --end-of-options keeps an option-shaped ref from reaching git
		// as an option; --verify requires exactly one valid object.
		out, _, err := gitcmd.New().Run(ctx, root, nil,
			"rev-parse", "--verify", "--quiet", "--end-of-options", r+"^{commit}")
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", fmt.Errorf("resolve %s: %w", r, ctxErr)
			}
			// rev-parse --verify --quiet exits 1 only when the ref does
			// not resolve; anything else (128 for a broken or missing
			// repo) is a real failure.
			if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
				return "", fmt.Errorf("resolve %s: not a commit in this repo", r)
			}
			return "", fmt.Errorf("resolve %s in %s: %w", r, root, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	start, end, isRange := strings.Cut(ref, "..")
	if !isRange {
		return resolve(ref)
	}
	inclusive := strings.HasSuffix(start, "^")
	startSHA, err := resolve(strings.TrimSuffix(start, "^"))
	if err != nil {
		return "", err
	}
	endSHA, err := resolve(end)
	if err != nil {
		return "", err
	}
	if inclusive {
		startSHA += "^"
	}
	return startSHA + ".." + endSHA, nil
}

// remoteEnqueue queues a review on a remote daemon. If the daemon lacks
// the commits, it uploads them as a git pack and retries once.
func remoteEnqueue(
	ctx context.Context, ep daemon.DaemonEndpoint, client *http.Client,
	root string, req daemon.EnqueueRequest,
) (int, []byte, error) {
	identity, err := remoteRepoIdentity(root)
	if err != nil {
		return 0, nil, err
	}
	ref := req.GitRef
	if ref == "" {
		ref, req.CommitSHA = req.CommitSHA, ""
	}
	if req.GitRef, err = resolveRemoteGitRef(ctx, root, ref); err != nil {
		return 0, nil, err
	}
	req.RepoPath, req.RepoIdentity = "", identity
	body, err := json.Marshal(req)
	if err != nil {
		return 0, nil, err
	}
	status, respBody, err := postRemoteEnqueue(ctx, ep, client, body)
	if err != nil || status != http.StatusConflict {
		return status, respBody, err
	}
	var missing daemon.MissingCommitsResponse
	if json.Unmarshal(respBody, &missing) != nil || missing.Code != daemon.MissingCommitsCode {
		return status, respBody, nil
	}
	if err := checkMissingSent(req.GitRef, missing.Missing); err != nil {
		return 0, nil, err
	}
	if err := uploadRemotePack(ctx, ep, client, root, identity, missing.Missing, missing.Have); err != nil {
		return 0, nil, err
	}
	return postRemoteEnqueue(ctx, ep, client, body)
}

// checkMissingSent refuses to pack anything the daemon lists as missing
// unless it is a full SHA this request sent, as the commit or a range
// endpoint. pack-objects would accept any revision, such as a private
// branch name, so the list must never reach it unchecked.
func checkMissingSent(gitRef string, missing []string) error {
	start, end, isRange := strings.Cut(gitRef, "..")
	sent := []string{gitRef}
	if isRange {
		sent = []string{strings.TrimSuffix(start, "^"), end}
	}
	for _, sha := range missing {
		if !fullSHAPattern.MatchString(sha) || !slices.Contains(sent, sha) {
			return fmt.Errorf("remote daemon asked for %q, which is not a commit this review sent; refusing to upload it", sha)
		}
	}
	return nil
}

var fullSHAPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func postRemoteEnqueue(ctx context.Context, ep daemon.DaemonEndpoint, client *http.Client, body []byte) (int, []byte, error) {
	resp, err := newDaemonAPI(ep.BaseURL(), client).EnqueueJobRaw(ctx, nil, roborevclient.WithBody(body))
	if err != nil {
		return 0, nil, fmt.Errorf("failed to connect to remote daemon: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read remote daemon response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

// uploadRemotePack packs the missing commits, leaving out history reachable
// from daemon ref tips that this repo also has.
func uploadRemotePack(
	ctx context.Context, ep daemon.DaemonEndpoint, client *http.Client,
	root, identity string, missing, have []string,
) error {
	var revs strings.Builder
	for _, sha := range missing {
		revs.WriteString(sha + "\n")
	}
	if len(have) > 0 {
		out, _, err := gitcmd.New().Run(ctx, root, strings.NewReader(strings.Join(have, "\n")+"\n"),
			"cat-file", "--batch-check=%(objectname) %(objecttype)")
		if err != nil {
			return fmt.Errorf("check daemon commits locally: %w", err)
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			if sha, kind, ok := strings.Cut(line, " "); ok && kind == "commit" {
				revs.WriteString("^" + sha + "\n")
			}
		}
	}
	pack, _, err := gitcmd.New().Run(ctx, root, strings.NewReader(revs.String()),
		"pack-objects", "--revs", "--stdout", "-q")
	if err != nil {
		return fmt.Errorf("build pack for remote daemon: %w", err)
	}
	query := url.Values{"repo_identity": {identity}, "tip": missing}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ep.BaseURL()+daemon.RemotePackPath+"?"+query.Encode(), bytes.NewReader(pack))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("upload commits to remote daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload commits to remote daemon: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
