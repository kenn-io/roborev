package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/daemon"
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
	PathEnv         string               `json:"path_env"`
	Agents          []agent.Diagnosis    `json:"agents"`
	Requested       []agent.Diagnosis    `json:"requested"`
	HookTools       []agent.Diagnosis    `json:"hook_tools"`
	Panels          []daemon.DoctorPanel `json:"panels"`
	PanelsError     string               `json:"panels_error,omitempty"`
	RepoConfigError string               `json:"repo_config_error,omitempty"`
}

// doctorDaemon is the read-only daemon surface the doctor uses. None of these
// calls may start, restart, or register anything.
type doctorDaemon interface {
	Ping() (*daemon.PingInfo, error)
	Agents(ctx context.Context, repo string, names []string) (*doctorDaemonAgents, error)
	Status(ctx context.Context) (*generated.DaemonStatus, error)
	Health(ctx context.Context) (*generated.HealthStatus, error)
	FailedJobs(ctx context.Context, since time.Time) ([]generated.ReviewJob, error)
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

// liveDoctorDaemon reads the daemon through the generated API client. Unlike
// other commands it never calls ensureDaemon: diagnosing must not start or
// restart the daemon, so a daemon that is down is reported, not started.
type liveDoctorDaemon struct {
	ep daemon.DaemonEndpoint
}

func newDoctorDaemon(ep daemon.DaemonEndpoint) doctorDaemon {
	return liveDoctorDaemon{ep: ep}
}

func (d liveDoctorDaemon) Ping() (*daemon.PingInfo, error) {
	return daemon.ProbeDaemon(d.ep, 2*time.Second)
}

// client has no total deadline; requests stop when the command's context
// is canceled.
func (d liveDoctorDaemon) client() *roborevclient.Client {
	return d.ep.APIClient(0)
}

func (d liveDoctorDaemon) Agents(ctx context.Context, repo string, names []string) (*doctorDaemonAgents, error) {
	query := &generated.DoctorAgentsQuery{}
	if repo != "" {
		query.Repo = new(repo)
	}
	if len(names) > 0 {
		query.Agent = names
	}
	resp, err := d.client().DoctorAgents(ctx, &generated.DoctorAgentsRequestOptions{Query: query})
	if err != nil {
		return nil, daemonRequestError("ask the daemon about agents", err)
	}
	return doctorAgentsFromAPI(resp), nil
}

// doctorAgentsFromAPI converts the API response into the agent and panel
// types the checks share with the local fallback.
func doctorAgentsFromAPI(resp *generated.DoctorAgentsResponse) *doctorDaemonAgents {
	out := &doctorDaemonAgents{
		PathEnv:         resp.PathEnv,
		Agents:          diagnosesFromAPI(resp.Agents),
		Requested:       diagnosesFromAPI(resp.Requested),
		HookTools:       diagnosesFromAPI(resp.HookTools),
		RepoConfigError: deref(resp.RepoConfigError),
		PanelsError:     deref(resp.PanelsError),
	}
	for _, p := range resp.Panels {
		panel := daemon.DoctorPanel{
			Name:       p.Name,
			UsedFor:    p.UsedFor,
			Experiment: deref(p.Experiment),
			Error:      deref(p.ErrorData),
			Synthesis:  diagnosisFromAPI(p.Synthesis),
		}
		for _, m := range p.Members {
			panel.Members = append(panel.Members, daemon.DoctorPanelMember{Name: m.Name, Agent: m.Agent})
		}
		out.Panels = append(out.Panels, panel)
	}
	return out
}

func diagnosesFromAPI(in []generated.Diagnosis) []agent.Diagnosis {
	out := make([]agent.Diagnosis, 0, len(in))
	for _, d := range in {
		out = append(out, diagnosisFromAPI(d))
	}
	return out
}

func diagnosisFromAPI(d generated.Diagnosis) agent.Diagnosis {
	return agent.Diagnosis{
		Name:      d.Name,
		Available: d.Available,
		Command:   deref(d.Command),
		Path:      deref(d.Path),
		Error:     deref(d.ErrorData),
		Unknown:   d.Unknown != nil && *d.Unknown,
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (d liveDoctorDaemon) Status(ctx context.Context) (*generated.DaemonStatus, error) {
	status, err := d.client().GetStatus(ctx)
	if err != nil {
		return nil, daemonRequestError("get daemon status", err)
	}
	return status, nil
}

func (d liveDoctorDaemon) Health(ctx context.Context) (*generated.HealthStatus, error) {
	health, err := d.client().GetHealth(ctx)
	if err != nil {
		return nil, daemonRequestError("get daemon health", err)
	}
	return health, nil
}

// doctorFailedJobPage is the page size for the failed-job scan.
const doctorFailedJobPage = 100

// FailedJobs returns failed jobs enqueued at or after since. The jobs API
// lists newest first, so paging stops at the first page that reaches back
// past since.
func (d liveDoctorDaemon) FailedJobs(ctx context.Context, since time.Time) ([]generated.ReviewJob, error) {
	var all []generated.ReviewJob
	var cursor *string
	for {
		page, err := d.client().ListJobs(ctx, &generated.ListJobsRequestOptions{
			Query: &generated.ListJobsQuery{
				Status:     new("failed"),
				Limit:      new(int64(doctorFailedJobPage)),
				OmitPrompt: new(generated.ListJobsQueryOmitPromptTrue),
				// Panel members run their own agents; a failing member
				// inside a panel that still finished is otherwise hidden.
				IncludePanelMembers: new(generated.ListJobsQueryIncludePanelMembersTrue),
				Cursor:              cursor,
			},
		})
		if err != nil {
			return nil, daemonRequestError("list failed jobs", err)
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
	resp, err := d.client().ResolveRepo(ctx, &generated.ResolveRepoRequestOptions{
		Query: &generated.ResolveRepoQuery{Path: new(repo)},
	})
	if err != nil {
		return false, daemonRequestError("resolve repository", err)
	}
	return resp.Tracked, nil
}
