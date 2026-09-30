package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
	"go.kenn.io/roborev/internal/version"
	roborevclient "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

// doctorStatus is the outcome of one check. Only doctorFail makes the command
// exit non-zero; warnings are advice.
type doctorStatus string

const (
	doctorOK   doctorStatus = "ok"
	doctorInfo doctorStatus = "info"
	doctorWarn doctorStatus = "warn"
	doctorFail doctorStatus = "fail"
)

// doctorCheck is one finding. ID is a stable dotted identifier for scripts
// and agents; Summary is a one-line human description; Fix says what to do.
type doctorCheck struct {
	ID       string       `json:"id"`
	Category string       `json:"category"`
	Status   doctorStatus `json:"status"`
	Summary  string       `json:"summary"`
	Details  []string     `json:"details,omitempty"`
	Fix      string       `json:"fix,omitempty"`
}

type doctorSummary struct {
	OK   int `json:"ok"`
	Info int `json:"info"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

type doctorReport struct {
	Version string        `json:"version"`
	Repo    string        `json:"repo,omitempty"`
	Checks  []doctorCheck `json:"checks"`
	Summary doctorSummary `json:"summary"`
}

// doctorDaemonAgents is the daemon's view of agent and hook-tool
// availability.
type doctorDaemonAgents struct {
	PathEnv         string            `json:"path_env"`
	Agents          []agent.Diagnosis `json:"agents"`
	Requested       []agent.Diagnosis `json:"requested"`
	HookTools       []agent.Diagnosis `json:"hook_tools"`
	RepoConfigError string            `json:"repo_config_error,omitempty"`
}

// doctorDaemon is the read-only daemon surface the doctor uses. None of these
// calls may start, restart, or register anything.
type doctorDaemon interface {
	Ping() (*daemon.PingInfo, error)
	Agents(ctx context.Context, repo string, names []string) (*doctorDaemonAgents, error)
	Status(ctx context.Context) (*storage.DaemonStatus, error)
	Health(ctx context.Context) (*storage.HealthStatus, error)
	FailedJobs(ctx context.Context, since time.Time) ([]storage.ReviewJob, error)
	RepoTracked(ctx context.Context, repo string) (bool, error)
}

func doctorCmd() *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose roborev setup and configuration problems",
		Long: `Check roborev's configuration, daemon, agents, recent failures, and the
current repository for problems, and suggest fixes.

The doctor only reads state. It never starts or restarts the daemon, installs
hooks, or edits config. Agent availability is checked from both this shell
and the running daemon, because the daemon may have started with a different
PATH.

Each check reports ok, info, warn, or fail. The command exits 1 when any check
fails; warnings alone exit 0.

Examples:
  roborev doctor
  roborev doctor --json
  roborev doctor --repo ~/src/myproject`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			repoPath, err := doctorRepoRoot(cmd)
			if err != nil {
				return err
			}
			env := loadDoctorEnv(cmd.Context(), repoPath, newDoctorDaemon(getDaemonEndpoint()))
			report := buildDoctorReport(env)

			out := cmd.OutOrStdout()
			if jsonOutput {
				enc := jsontext.NewEncoder(out, jsontext.WithIndent("  "))
				if err := json.MarshalEncode(enc, report); err != nil {
					return err
				}
			} else {
				renderDoctorReport(out, report)
			}
			if report.Summary.Fail > 0 {
				return silentExit(cmd, 1)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output the report as JSON")
	cmd.Flags().String("repo", "", "repository to check (default: current directory)")
	return cmd
}

// doctorRepoRoot resolves the git root for --repo or the working directory,
// returning "" outside a git repository.
func doctorRepoRoot(cmd *cobra.Command) (string, error) {
	path, err := cmd.Flags().GetString("repo")
	if err != nil {
		return "", err
	}
	if path == "" {
		if path, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	root, err := findRepoRootFrom(path)
	if err != nil {
		if errors.Is(err, errNotGitRepository) {
			return "", nil
		}
		return "", err
	}
	return root, nil
}

func buildDoctorReport(env *doctorEnv) doctorReport {
	report := doctorReport{
		Version: version.Version,
		Repo:    env.repoPath,
		Checks:  runDoctorChecks(env),
	}
	for _, c := range report.Checks {
		switch c.Status {
		case doctorOK:
			report.Summary.OK++
		case doctorInfo:
			report.Summary.Info++
		case doctorWarn:
			report.Summary.Warn++
		case doctorFail:
			report.Summary.Fail++
		}
	}
	return report
}

var doctorCategoryTitles = map[string]string{
	"config":       "Configuration",
	"daemon":       "Daemon",
	"agents":       "Agents",
	"jobs":         "Recent failures",
	"repo":         "Repository",
	"integrations": "Integrations",
}

var doctorStatusLabels = map[doctorStatus]string{
	doctorOK:   "[ok]  ",
	doctorInfo: "[info]",
	doctorWarn: "[warn]",
	doctorFail: "[FAIL]",
}

func renderDoctorReport(w io.Writer, report doctorReport) {
	fmt.Fprintf(w, "roborev doctor (%s)\n", report.Version)
	if report.Repo != "" {
		fmt.Fprintf(w, "Repository: %s\n", report.Repo)
	}
	category := ""
	for _, c := range report.Checks {
		if c.Category != category {
			category = c.Category
			title := doctorCategoryTitles[category]
			if title == "" {
				title = category
			}
			fmt.Fprintf(w, "\n%s\n", title)
		}
		fmt.Fprintf(w, "  %s %s\n", doctorStatusLabels[c.Status], doctorDisplay(c.Summary))
		if c.Status == doctorOK {
			continue
		}
		for _, d := range c.Details {
			fmt.Fprintf(w, "         - %s\n", doctorDisplay(d))
		}
		if c.Fix != "" {
			fmt.Fprintf(w, "         fix: %s\n", doctorDisplay(c.Fix))
		}
	}

	s := report.Summary
	fmt.Fprintln(w)
	if s.Fail == 0 && s.Warn == 0 {
		fmt.Fprintln(w, "No problems found.")
		return
	}
	fmt.Fprintf(w, "%s, %s.\n", plural(s.Fail, "failure"), plural(s.Warn, "warning"))
}

// doctorDisplay strips terminal escapes from text that can carry agent
// output or file contents, and indents continuation lines under the check.
func doctorDisplay(s string) string {
	s = strings.TrimSpace(streamfmt.SanitizeControlKeepNewlines(s))
	return strings.ReplaceAll(s, "\n", "\n           ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// liveDoctorDaemon talks to the daemon at a fixed endpoint without the
// ensureDaemon start/restart behavior other commands use.
type liveDoctorDaemon struct {
	ep daemon.DaemonEndpoint
}

func newDoctorDaemon(ep daemon.DaemonEndpoint) doctorDaemon {
	return liveDoctorDaemon{ep: ep}
}

func (d liveDoctorDaemon) Ping() (*daemon.PingInfo, error) {
	return daemon.ProbeDaemon(d.ep, 2*time.Second)
}

func (d liveDoctorDaemon) client() *roborevclient.Client {
	return d.ep.APIClient(10 * time.Second)
}

func decodeDoctorResponse(resp *http.Response, err error, into any) error {
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned %s", resp.Status)
	}
	return json.UnmarshalRead(resp.Body, into)
}

func (d liveDoctorDaemon) Agents(ctx context.Context, repo string, names []string) (*doctorDaemonAgents, error) {
	opts := &generated.DoctorAgentsRequestOptions{Query: &generated.DoctorAgentsQuery{}}
	if repo != "" {
		opts.Query.Repo = new(repo)
	}
	if len(names) > 0 {
		opts.Query.Agent = names
	}
	resp, err := d.client().DoctorAgentsRaw(ctx, opts)
	var out doctorDaemonAgents
	if err := decodeDoctorResponse(resp, err, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (d liveDoctorDaemon) Status(ctx context.Context) (*storage.DaemonStatus, error) {
	resp, err := d.client().GetStatusRaw(ctx)
	var out storage.DaemonStatus
	if err := decodeDoctorResponse(resp, err, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (d liveDoctorDaemon) Health(ctx context.Context) (*storage.HealthStatus, error) {
	resp, err := d.client().GetHealthRaw(ctx)
	var out storage.HealthStatus
	if err := decodeDoctorResponse(resp, err, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// doctorFailedJobPage is the page size for the failed-job scan.
const doctorFailedJobPage = 100

// FailedJobs returns failed jobs enqueued at or after since. The jobs API
// lists newest first, so paging stops at the first page that reaches back
// past since.
func (d liveDoctorDaemon) FailedJobs(ctx context.Context, since time.Time) ([]storage.ReviewJob, error) {
	var all []storage.ReviewJob
	var cursor *string
	for {
		resp, err := d.client().ListJobsRaw(ctx, &generated.ListJobsRequestOptions{
			Query: &generated.ListJobsQuery{
				Status:     new("failed"),
				Limit:      new(int64(doctorFailedJobPage)),
				OmitPrompt: new(generated.ListJobsQueryOmitPromptTrue),
				Cursor:     cursor,
			},
		})
		var page struct {
			Jobs       []storage.ReviewJob `json:"jobs"`
			HasMore    bool                `json:"has_more"`
			NextCursor *string             `json:"next_cursor"`
		}
		if err := decodeDoctorResponse(resp, err, &page); err != nil {
			return nil, err
		}
		reachedCutoff := false
		for _, j := range page.Jobs {
			if j.EnqueuedAt.Before(since) {
				reachedCutoff = true
				break
			}
			all = append(all, j)
		}
		if reachedCutoff || !page.HasMore || page.NextCursor == nil {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

func (d liveDoctorDaemon) RepoTracked(ctx context.Context, repo string) (bool, error) {
	resp, err := d.client().ResolveRepoRaw(ctx, &generated.ResolveRepoRequestOptions{
		Query: &generated.ResolveRepoQuery{Path: new(repo)},
	})
	var out struct {
		Tracked bool `json:"tracked"`
	}
	if err := decodeDoctorResponse(resp, err, &out); err != nil {
		return false, err
	}
	return out.Tracked, nil
}
