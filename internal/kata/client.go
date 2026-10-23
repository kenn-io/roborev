package kata

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kit/pathresolve"

	"go.kenn.io/roborev/internal/procutil"
)

// CLIClient implements Client by shelling out to the kata CLI.
type CLIClient struct {
	bin     string
	workdir string
	env     []string
	run     func(ctx context.Context, c *CLIClient, args []string, stdin io.Reader) ([]byte, error)
}

var _ Client = (*CLIClient)(nil)

// NewCLIClient returns a CLIClient bound to workdir for project resolution.
func NewCLIClient(workdir string) *CLIClient {
	return &CLIClient{bin: "kata", workdir: workdir, run: realRun}
}

// NewCLIClientWithEnv is NewCLIClient with env as the complete environment of
// every kata subprocess. Nothing is inherited from the process, so a test can
// keep the caller's Kata routing, token, and attribution out of the run.
func NewCLIClientWithEnv(workdir string, env []string) *CLIClient {
	c := NewCLIClient(workdir)
	c.env = env
	return c
}

func buildKataCmd(ctx context.Context, c *CLIClient, args []string, stdin io.Reader) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	procutil.HideConsole(cmd)
	cmd.Dir = c.workdir
	if c.env != nil {
		cmd.Env = c.env
	}
	cmd.Stdin = stdin
	return cmd
}

// realRun executes kata and returns stdout. A missing binary becomes
// ErrUnavailable; a non-zero exit includes stderr.
func realRun(ctx context.Context, c *CLIClient, args []string, stdin io.Reader) ([]byte, error) {
	cmd := buildKataCmd(ctx, c, args, stdin)

	verb := "kata"
	if len(args) > 0 {
		verb = args[0]
	}

	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, ErrUnavailable
		}
		// A cancelled or timed-out context surfaces as a killed-process
		// ExitError; preserve ctx.Err so callers can errors.Is it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("kata %s: %w", verb, ctxErr)
		}
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("kata %s: exit %d: %s", verb, ee.ExitCode(), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("kata %s: %w", verb, err)
	}
	return out, nil
}

// Binding resolves the workspace project by reading .kata.local.toml (local
// override) then .kata.toml, from workdir upward. It does not require the kata
// binary. A present-but-unreadable or unsupported config file stops the search
// rather than silently binding to a parent project.
func (c *CLIClient) Binding(_ context.Context) (Binding, error) {
	return workspaceBinding(c.workdir)
}

// workspaceBinding reads the binding for workdir. Both clients use it, so the
// API connection resolves a workspace exactly as the kata CLI does.
func workspaceBinding(workdir string) (Binding, error) {
	selection, err := discoverWorkspace(workdir)
	return Binding{Project: selection.name}, err
}

// WorkspaceBinding resolves the workspace selection for a daemon proxy. It
// includes the registered alias for committed bindings, while local overrides
// keep selecting their project name directly.
func WorkspaceBinding(ctx context.Context, workdir string) (Binding, error) {
	selection, err := discoverWorkspace(workdir)
	if err != nil {
		return Binding{}, err
	}
	binding := Binding{Project: selection.name}
	if selection.local {
		return binding, nil
	}
	alias, err := workspaceAlias(ctx, selection)
	if err != nil {
		return Binding{}, fmt.Errorf("%w: %w", ErrIncompatible, err)
	}
	binding.Alias = &ProjectAlias{Identity: alias.Identity, Kind: alias.Kind}
	return binding, nil
}

type workspaceProject struct {
	name, root, gitRoot string
	local               bool
}

func discoverWorkspace(workdir string) (workspaceProject, error) {
	abs, err := filepath.Abs(cmp.Or(workdir, "."))
	if err != nil {
		return workspaceProject{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return workspaceProject{}, fmt.Errorf("kata: stat workspace: %w", err)
	}
	resolved, err := pathresolve.EvalSymlinks(abs)
	if err != nil {
		return workspaceProject{}, fmt.Errorf("kata: resolve workspace: %w", err)
	}
	if !info.IsDir() {
		abs, resolved = filepath.Dir(abs), filepath.Dir(resolved)
	}
	lexicalGit, err := workspaceGitRoot(abs)
	if err != nil {
		return workspaceProject{}, err
	}
	physicalGit, err := workspaceGitRoot(resolved)
	if err != nil {
		return workspaceProject{}, err
	}
	gitRoot := lexicalGit
	if physicalGit != "" {
		resolvedLexical, resolveErr := pathresolve.EvalSymlinks(lexicalGit)
		if resolveErr != nil || resolvedLexical != physicalGit {
			abs, gitRoot = resolved, physicalGit
		}
	}
	for {
		for _, base := range []string{".kata.local.toml", ".kata.toml"} {
			local := base == ".kata.local.toml"
			name, found, err := readKataProjectName(filepath.Join(abs, base), local)
			if err != nil {
				return workspaceProject{}, err
			}
			if found {
				return workspaceProject{name: name, root: abs, gitRoot: gitRoot, local: local}, nil
			}
		}
		if abs == gitRoot {
			break
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	return workspaceProject{}, ErrNoBinding
}

func workspaceGitRoot(start string) (string, error) {
	for {
		_, err := os.Stat(filepath.Join(start, ".git"))
		switch {
		case err == nil:
			return start, nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("kata: discover .git: %w", err)
		case filepath.Dir(start) == start:
			return "", nil
		}
		start = filepath.Dir(start)
	}
}

func readKataProjectName(path string, local bool) (name string, found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("kata: read %s: %w", path, err)
	}
	var doc struct {
		Version int `toml:"version"`
		Project struct {
			Name     string `toml:"name"`
			Identity string `toml:"identity"`
		} `toml:"project"`
	}
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return "", false, fmt.Errorf("kata: parse %s: %w", path, err)
	}
	if doc.Version != 1 {
		return "", false, fmt.Errorf("kata: unsupported binding version %d (expected 1)", doc.Version)
	}
	name = strings.TrimSpace(doc.Project.Name)
	identity := strings.TrimSpace(doc.Project.Identity)
	if !local && name == "" && identity != "" {
		name = identity[strings.LastIndexAny(identity, "/:")+1:]
	}
	if name == "" {
		if local {
			return "", false, nil
		}
		return "", false, fmt.Errorf("kata: %s: project.name must be non-empty", path)
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return "", false, fmt.Errorf("kata: %s: project.name contains non-printable character %U", path, r)
		}
	}
	return name, true, nil
}

type listEnvelope struct {
	Issues []Issue `json:"issues"`
}

// linkEdge is one link of an issue as Kata reports it.
type linkEdge struct {
	Type string   `json:"type"`
	From LinkPeer `json:"from"`
	To   LinkPeer `json:"to"`
}

type showEnvelope struct {
	Issue  Issue      `json:"issue"`
	Links  []linkEdge `json:"links"`
	Labels []struct {
		Label string `json:"label"`
	} `json:"labels"`
}

type createEnvelope struct {
	Issue struct {
		ShortID    string `json:"short_id"`
		UID        string `json:"uid"`
		ProjectUID string `json:"project_uid"`
	} `json:"issue"`
	Reused bool `json:"reused"`
}

// List returns issues filtered by status (default "open").
func (c *CLIClient) List(ctx context.Context, opts ListOpts) ([]Issue, error) {
	status := opts.Status
	if status == "" {
		status = "open"
	}
	args := []string{"list", "--json", "--status", status}
	if opts.Unlimited {
		args = append(args, "--limit", "0")
	}
	out, err := c.run(ctx, c, args, nil)
	if err != nil {
		return nil, err
	}
	var env listEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("parse kata list: %w", err)
	}
	if opts.Unlimited {
		var raw struct {
			Issues *[]map[string]jsontext.Value `json:"issues"`
		}
		if err := json.Unmarshal(out, &raw); err != nil {
			return nil, err
		}
		if raw.Issues == nil {
			return nil, fmt.Errorf("kata list requires an issues array for complete graph capture")
		}
		for i, row := range *raw.Issues {
			if _, complete := row["body"]; complete {
				continue
			}
			detail, err := c.Show(ctx, env.Issues[i].ShortID)
			if err != nil {
				return nil, err
			}
			if detail.ShortID != env.Issues[i].ShortID {
				return nil, fmt.Errorf("kata show returned a different issue")
			}
			if detail.QualifiedID == "" {
				detail.QualifiedID = env.Issues[i].QualifiedID
			}
			env.Issues[i] = detail
		}
	}
	return env.Issues, nil
}

// Show returns a single issue by ref (a bare short_id resolves against the
// workspace binding).
func (c *CLIClient) Show(ctx context.Context, ref string) (Issue, error) {
	out, err := c.run(ctx, c, []string{"show", ref, "--json"}, nil)
	if err != nil {
		return Issue{}, err
	}
	var env showEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return Issue{}, fmt.Errorf("parse kata show: %w", err)
	}
	iss := env.Issue
	if len(env.Links) > 0 {
		currentID, err := c.qualifiedShownIssueID(ctx, ref, iss)
		if err != nil {
			return Issue{}, err
		}
		iss.QualifiedID = currentID
		if err := attachLinks(&iss, currentID, env.Links); err != nil {
			return Issue{}, err
		}
	}
	for _, l := range env.Labels {
		if l.Label != "" {
			iss.Labels = append(iss.Labels, l.Label)
		}
	}
	return iss, nil
}

// attachLinks sorts an issue's links into parent, blocks, blocked-by, and
// related, using currentID to tell which end of each link is this issue.
func attachLinks(iss *Issue, currentID string, links []linkEdge) error {
	for _, link := range links {
		fromID := qualifiedLinkPeerID(link.From)
		toID := qualifiedLinkPeerID(link.To)
		outgoing := fromID == currentID
		if !outgoing && toID != currentID {
			return fmt.Errorf("kata show returned a link that cannot be resolved against %q", currentID)
		}
		if fromID == "" || toID == "" {
			return fmt.Errorf("kata show returned a link without a qualified issue identity")
		}
		peer := link.From
		if outgoing {
			peer = link.To
		}
		switch link.Type {
		case "parent":
			if outgoing {
				iss.Parent = &peer
			}
		case "blocks":
			if outgoing {
				iss.Blocks = append(iss.Blocks, peer)
			} else {
				iss.BlockedBy = append(iss.BlockedBy, peer)
			}
		case "related":
			iss.Related = append(iss.Related, peer)
		}
	}
	return nil
}

func (c *CLIClient) qualifiedShownIssueID(ctx context.Context, ref string, iss Issue) (string, error) {
	if iss.QualifiedID != "" {
		return iss.QualifiedID, nil
	}
	if project, shortID, qualified := strings.Cut(ref, "#"); qualified {
		if project == "" || shortID == "" || (iss.ShortID != "" && shortID != iss.ShortID) {
			return "", fmt.Errorf("kata show returned an issue that does not match %q", ref)
		}
		return ref, nil
	}
	binding, err := c.Binding(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve Kata issue link direction: %w", err)
	}
	if binding.Project == "" || iss.ShortID == "" {
		return "", fmt.Errorf("resolve Kata issue link direction: workspace binding and issue ID are required")
	}
	return binding.Project + "#" + iss.ShortID, nil
}

func qualifiedLinkPeerID(peer LinkPeer) string {
	if peer.QualifiedID != "" {
		return peer.QualifiedID
	}
	if peer.Project != "" && peer.ShortID != "" {
		return peer.Project + "#" + peer.ShortID
	}
	return ""
}

// Create files an issue, piping the body over stdin via --body-stdin.
func (c *CLIClient) Create(ctx context.Context, req CreateReq) (CreateResult, error) {
	args := []string{"create", req.Title, "--json", "--body-stdin"}
	if req.Project != "" {
		args = append(args, "--project", req.Project)
	}
	for _, l := range req.Labels {
		args = append(args, "--label", l)
	}
	if req.Priority != nil {
		args = append(args, "--priority", strconv.Itoa(*req.Priority))
	}
	if req.IdempotencyKey != "" {
		args = append(args, "--idempotency-key", req.IdempotencyKey)
	}
	out, err := c.run(ctx, c, args, strings.NewReader(req.Body))
	if err != nil {
		return CreateResult{}, err
	}
	var env createEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return CreateResult{}, fmt.Errorf("parse kata create: %w", err)
	}
	return CreateResult{ShortID: env.Issue.ShortID, IssueUID: env.Issue.UID, ProjectUID: env.Issue.ProjectUID, Reused: env.Reused}, nil
}

// Comment is not supported by the kata CLI client: replies need the Kata API
// connection, which reports who Kata attributed the write to and which comment
// it stored. Nothing is run.
func (c *CLIClient) Comment(context.Context, CommentReq) (CommentResult, error) {
	return CommentResult{}, ErrCommentsUnsupported
}

// Thread is not supported by the kata CLI client. Nothing is run.
func (c *CLIClient) Thread(context.Context, ThreadReq) (Thread, error) {
	return Thread{}, ErrCommentsUnsupported
}
