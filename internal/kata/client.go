package kata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
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

// NewCLIClientWithEnv is NewCLIClient plus extra environment entries appended to
// the process environment (used by tests to set KATA_HOME).
func NewCLIClientWithEnv(workdir string, env []string) *CLIClient {
	c := NewCLIClient(workdir)
	c.env = env
	return c
}

// realRun executes kata and returns stdout. A missing binary becomes
// ErrUnavailable; a non-zero exit includes stderr.
func realRun(ctx context.Context, c *CLIClient, args []string, stdin io.Reader) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Dir = c.workdir
	if len(c.env) > 0 {
		cmd.Env = append(os.Environ(), c.env...)
	}
	cmd.Stdin = stdin

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
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("kata %s: exit %d: %s", verb, ee.ExitCode(), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("kata %s: %w", verb, err)
	}
	return out, nil
}

// Binding resolves the workspace project by reading .kata.local.toml (local
// override) then .kata.toml, from workdir upward. It does not require the kata
// binary. A present-but-unreadable config file stops the search with an error
// rather than silently binding to a parent project.
func (c *CLIClient) Binding(_ context.Context) (Binding, error) {
	dir := c.workdir
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Binding{}, err
	}
	for {
		for _, base := range []string{".kata.local.toml", ".kata.toml"} {
			name, found, err := readKataProjectName(filepath.Join(abs, base))
			if err != nil {
				return Binding{}, err
			}
			if found {
				return Binding{Project: name}, nil
			}
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return Binding{}, ErrNoBinding
		}
		abs = parent
	}
}

// readKataProjectName reads [project].name from a kata config file. found is
// true only when the file exists, parses, and carries a non-empty name. A
// missing file returns (found=false, err=nil) so the caller keeps searching; a
// present-but-unreadable or malformed file returns a non-nil err so the caller
// stops rather than silently binding to a parent project.
func readKataProjectName(path string) (name string, found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("kata: read %s: %w", path, err)
	}
	var doc struct {
		Project struct {
			Name string `toml:"name"`
		} `toml:"project"`
	}
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return "", false, fmt.Errorf("kata: parse %s: %w", path, err)
	}
	return doc.Project.Name, doc.Project.Name != "", nil
}

type listEnvelope struct {
	Issues []Issue `json:"issues"`
}

type showEnvelope struct {
	Issue  Issue `json:"issue"`
	Labels []struct {
		Label string `json:"label"`
	} `json:"labels"`
}

type createEnvelope struct {
	Issue struct {
		ShortID string `json:"short_id"`
	} `json:"issue"`
	Reused bool `json:"reused"`
}

// List returns issues filtered by status (default "open").
func (c *CLIClient) List(ctx context.Context, opts ListOpts) ([]Issue, error) {
	status := opts.Status
	if status == "" {
		status = "open"
	}
	out, err := c.run(ctx, c, []string{"list", "--json", "--status", status}, nil)
	if err != nil {
		return nil, err
	}
	var env listEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("parse kata list: %w", err)
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
	for _, l := range env.Labels {
		if l.Label != "" {
			iss.Labels = append(iss.Labels, l.Label)
		}
	}
	return iss, nil
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
	return CreateResult{ShortID: env.Issue.ShortID, Reused: env.Reused}, nil
}
