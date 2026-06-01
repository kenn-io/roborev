package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	ghpkg "go.kenn.io/roborev/internal/github"
	"go.kenn.io/roborev/internal/review"
)

func ciCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ci",
		Short: "CI-specific commands for running roborev in CI pipelines",
	}

	cmd.AddCommand(ciReviewCmd())
	return cmd
}

func ciReviewCmd() *cobra.Command {
	var (
		refFlag        string
		commentFlag    bool
		ghRepoFlag     string
		prFlag         int
		agentFlag      string
		reviewTypes    string
		reasoning      string
		minSeverity    string
		synthesisAgent string
	)

	cmd := &cobra.Command{
		Use:   "review",
		Short: "Run a full review matrix and optionally post a PR comment",
		Long: `Run the full review_type x agent matrix in parallel, ` +
			`synthesize results, and output or post a PR comment.

This command is designed for CI pipelines. It reads ` +
			`configuration from .roborev.toml and runs without ` +
			`a daemon or database.

Flags override config values. When run inside GitHub ` +
			`Actions, --ref, --gh-repo, and --pr are ` +
			`auto-detected from the environment.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCIReview(
				cmd.Context(), ciReviewOpts{
					ref:            refFlag,
					comment:        commentFlag,
					ghRepo:         ghRepoFlag,
					pr:             prFlag,
					agents:         agentFlag,
					reviewTypes:    reviewTypes,
					reasoning:      reasoning,
					minSeverity:    minSeverity,
					synthesisAgent: synthesisAgent,
				})
		},
	}

	cmd.Flags().StringVar(&refFlag, "ref", "",
		"git ref range (e.g., BASE..HEAD); "+
			"auto-detected in GitHub Actions")
	cmd.Flags().BoolVar(&commentFlag, "comment", false,
		"post result as PR comment via gh pr comment")
	cmd.Flags().StringVar(&ghRepoFlag, "gh-repo", "",
		"GitHub repo owner/name (auto from GITHUB_REPOSITORY)")
	cmd.Flags().IntVar(&prFlag, "pr", 0,
		"PR number (auto from event JSON / GITHUB_REF)")
	cmd.Flags().StringVar(&agentFlag, "agent", "",
		"comma-separated agent list (overrides config)")
	cmd.Flags().StringVar(&reviewTypes, "review-types", "",
		"comma-separated review types (overrides config)")
	cmd.Flags().StringVar(&reasoning, "reasoning", "",
		"reasoning level: fast, standard, medium, thorough, or maximum")
	cmd.Flags().StringVar(&minSeverity, "min-severity", "",
		"minimum severity filter: critical, high, medium, low")
	cmd.Flags().StringVar(&synthesisAgent, "synthesis-agent", "",
		"agent for synthesis (overrides config)")

	return cmd
}

type ciReviewOpts struct {
	ref            string
	comment        bool
	ghRepo         string
	pr             int
	agents         string
	reviewTypes    string
	reasoning      string
	minSeverity    string
	synthesisAgent string
}

func runCIReview(ctx context.Context, opts ciReviewOpts) error {
	// Validate flag-only inputs early (before git/config
	// checks) so users get clear errors even outside a repo.
	if opts.reviewTypes != "" {
		if _, err := config.ValidateReviewTypes(
			splitTrimmed(opts.reviewTypes)); err != nil {
			return err
		}
	}
	if opts.reasoning != "" {
		if _, err := config.NormalizeReasoning(
			opts.reasoning); err != nil {
			return err
		}
	}
	if opts.minSeverity != "" {
		if _, err := config.NormalizeMinSeverity(
			opts.minSeverity); err != nil {
			return err
		}
	}

	// Resolve git ref (from flag or environment — no repo
	// needed, so validate before the git repo check).
	gitRef := opts.ref
	if gitRef == "" {
		detected, err := detectGitRef()
		if err != nil {
			return fmt.Errorf(
				"--ref not provided and auto-detection "+
					"failed: %w", err)
		}
		gitRef = detected
	}

	// Determine repo root
	root, err := gitrepo.Root(ctx, ".")
	if err != nil {
		return fmt.Errorf(
			"not a git repository — " +
				"run this from inside a git repo")
	}

	// Load configs (warn on error, don't fail)
	globalCfg, err := config.LoadGlobal()
	if err != nil {
		log.Printf(
			"ci review: load global config: %v "+
				"(using defaults)", err)
	}
	repoCfg, err := config.LoadRepoConfig(root)
	if err != nil {
		log.Printf(
			"ci review: load repo config: %v "+
				"(using defaults)", err)
	}

	// Resolve agents
	agents := config.ResolveCIAgents(
		opts.agents, repoCfg, globalCfg)
	if len(agents) == 0 {
		return fmt.Errorf("no agents configured " +
			"(check --agent flag or config)")
	}

	// Resolve review types
	reviewTypes := config.ResolveCIReviewTypes(
		opts.reviewTypes, repoCfg, globalCfg)
	if len(reviewTypes) == 0 {
		return fmt.Errorf("no review types configured " +
			"(check --review-types flag or config)")
	}
	reviewTypes, err = config.ValidateReviewTypes(
		reviewTypes)
	if err != nil {
		return err
	}

	// Resolve reasoning
	reasoningLevel, err := config.ResolveCIReasoning(
		opts.reasoning, repoCfg, globalCfg)
	if err != nil {
		return err
	}

	// Resolve min severity for synthesis output filtering
	ciMinSev, err := config.ResolveCIMinSeverity(
		opts.minSeverity, repoCfg, globalCfg)
	if err != nil {
		return err
	}

	// Resolve review-level min severity for prompt injection
	reviewMinSev, err := config.ResolveReviewMinSeverity(
		"", root, globalCfg)
	if err != nil {
		return err
	}

	// Resolve synthesis agent
	synthAgent := config.ResolveCISynthesisAgent(
		opts.synthesisAgent, repoCfg, globalCfg)

	log.Printf(
		"ci review: ref=%s agents=%v types=%v "+
			"reasoning=%s ci_min_severity=%s review_min_severity=%s",
		gitRef, agents, reviewTypes,
		reasoningLevel, ciMinSev, reviewMinSev)

	// Run batch with review-level severity for prompt injection.
	// CI-level severity is applied separately during synthesis.
	batchCfg := review.BatchConfig{
		RepoPath:     root,
		GitRef:       gitRef,
		Agents:       agents,
		ReviewTypes:  reviewTypes,
		Reasoning:    reasoningLevel,
		GlobalConfig: globalCfg,
		MinSeverity:  reviewMinSev,
	}

	results := review.RunBatch(ctx, batchCfg)

	// Determine HEAD SHA for comment formatting
	headSHA := extractHeadSHA(gitRef)

	// Synthesize
	comment, synthErr := review.Synthesize(
		ctx, results, review.SynthesizeOpts{
			Agent:        synthAgent,
			MinSeverity:  ciMinSev,
			RepoPath:     root,
			GitRef:       gitRef,
			HeadSHA:      headSHA,
			GlobalConfig: globalCfg,
		})
	if synthErr != nil &&
		!errors.Is(synthErr, review.ErrAllFailed) {
		return fmt.Errorf("synthesize: %w", synthErr)
	}

	// Output to stdout (even on all-failed, for CI logs)
	fmt.Println(comment)

	// Post as PR comment if requested
	if opts.comment {
		ghRepo := opts.ghRepo
		if ghRepo == "" {
			ghRepo = os.Getenv("GITHUB_REPOSITORY")
		}
		if ghRepo == "" {
			return fmt.Errorf(
				"--comment requires --gh-repo or " +
					"GITHUB_REPOSITORY env var")
		}

		prNumber := opts.pr
		if prNumber == 0 {
			detected, err := detectPRNumber()
			if err != nil {
				return fmt.Errorf(
					"--comment requires --pr or "+
						"auto-detection from "+
						"GITHUB_EVENT_PATH/GITHUB_REF: %w",
					err)
			}
			prNumber = detected
		}

		upsert := config.ResolveCIUpsertComments(
			repoCfg, globalCfg)
		if err := postCIComment(
			ctx, ghRepo, prNumber, comment, upsert,
		); err != nil {
			return fmt.Errorf(
				"post PR comment: %w", err)
		}
		log.Printf(
			"ci review: posted comment on %s#%d",
			ghRepo, prNumber)
	}

	// Exit non-zero when all review jobs failed
	if synthErr != nil {
		return synthErr
	}

	return nil
}

func splitTrimmed(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ghPREvent represents the pull_request fields from a
// GitHub Actions event payload.
type ghPREvent struct {
	PullRequest struct {
		Number int `json:"number"`
		Base   struct {
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}

// readPREvent reads and unmarshals the GitHub Actions event
// file pointed to by GITHUB_EVENT_PATH.
func readPREvent() (*ghPREvent, error) {
	eventPath := os.Getenv("GITHUB_EVENT_PATH")
	if eventPath == "" {
		return nil, fmt.Errorf("GITHUB_EVENT_PATH not set")
	}
	data, err := os.ReadFile(eventPath)
	if err != nil {
		return nil, fmt.Errorf("read event file: %w", err)
	}
	var event ghPREvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, fmt.Errorf("parse event JSON: %w", err)
	}
	return &event, nil
}

// detectGitRef attempts to auto-detect the git ref range from
// the GitHub Actions environment.
func detectGitRef() (string, error) {
	event, err := readPREvent()
	if err != nil {
		return "", err
	}

	base := event.PullRequest.Base.SHA
	head := event.PullRequest.Head.SHA
	if base == "" || head == "" {
		return "", fmt.Errorf(
			"event JSON missing " +
				"pull_request.base.sha or " +
				"pull_request.head.sha")
	}

	return base + ".." + head, nil
}

// detectPRNumber attempts to auto-detect the PR number from
// the GitHub Actions environment.
func detectPRNumber() (int, error) {
	// Try event JSON first
	event, err := readPREvent()
	if err == nil && event.PullRequest.Number > 0 {
		return event.PullRequest.Number, nil
	}

	// Try GITHUB_REF (refs/pull/N/merge)
	ghRef := os.Getenv("GITHUB_REF")
	if strings.HasPrefix(ghRef, "refs/pull/") {
		parts := strings.Split(ghRef, "/")
		if len(parts) >= 3 {
			n, err := strconv.Atoi(parts[2])
			if err == nil && n > 0 {
				return n, nil
			}
		}
	}

	return 0, fmt.Errorf(
		"could not detect PR number from environment")
}

// extractHeadSHA extracts the HEAD SHA from a git ref range.
// For "BASE..HEAD" returns HEAD; for a single ref returns it.
func extractHeadSHA(gitRef string) string {
	if _, after, ok := strings.Cut(gitRef, ".."); ok {
		return after
	}
	return gitRef
}

// postCIComment posts a roborev comment on a GitHub PR.
// When upsert is true, it finds and patches an existing marker comment;
// otherwise it always creates a new comment.
func postCIComment(
	ctx context.Context,
	ghRepo string,
	prNumber int,
	body string,
	upsert bool,
) error {
	client, err := ciGitHubClient()
	if err != nil {
		return err
	}
	if upsert {
		return client.UpsertPRComment(ctx, ghRepo, prNumber, body)
	}
	return client.CreatePRComment(ctx, ghRepo, prNumber, body)
}

func ciGitHubClient() (*ghpkg.Client, error) {
	// Resolve the API base URL from GITHUB_API_URL (GitHub Actions
	// Enterprise) or GH_HOST. Without this, Enterprise tokens would
	// be sent to api.github.com instead of the configured host.
	rawBase := os.Getenv("GITHUB_API_URL")
	apiBaseURL, err := ghpkg.GitHubAPIBaseURL(rawBase)
	if err != nil {
		return nil, err
	}
	// Derive hostname from the resolved API base URL so the
	// gh auth token fallback targets the same host as the client.
	host := ghpkg.HostnameFromAPIBaseURL(apiBaseURL)
	token := ghpkg.ResolveAuthToken(context.Background(), ghpkg.EnvironmentToken(), host)
	if token == "" {
		return nil, fmt.Errorf("GitHub authentication required: set GH_TOKEN or GITHUB_TOKEN, or authenticate with gh auth login")
	}
	return ghpkg.NewClient(token, ghpkg.WithBaseURL(apiBaseURL))
}
