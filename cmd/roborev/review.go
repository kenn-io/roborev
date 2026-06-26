package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

// MaxDirtyDiffSize is the maximum size of a dirty diff in bytes (200KB)
const MaxDirtyDiffSize = 200 * 1024

// describeEnqueue formats the post-enqueue confirmation line. The
// "Enqueued job <id>" token is preserved verbatim (skills parse it). For a
// panel run the id is the synthesis (parent) job and the line notes the panel
// name and reviewer count; memberCount is 0 for a single-agent review.
func describeEnqueue(job storage.ReviewJob, memberCount int, dirty bool) string {
	if memberCount > 0 {
		name := job.PanelName
		if name == "" {
			name = "panel"
		}
		return fmt.Sprintf("Enqueued job %d (panel: %s, %d reviewers) for %s",
			job.ID, name, memberCount, shortRef(job.GitRef))
	}
	if dirty {
		return fmt.Sprintf("Enqueued dirty review job %d (agent: %s)", job.ID, job.Agent)
	}
	return fmt.Sprintf("Enqueued job %d for %s (agent: %s)", job.ID, shortRef(job.GitRef), job.Agent)
}

func reviewCmd() *cobra.Command {
	var (
		repoPath    string
		sha         string
		agent       string
		model       string
		reasoning   string
		reviewType  string
		fast        bool
		quiet       bool
		dirty       bool
		wait        bool
		branch      string
		baseBranch  string
		since       string
		local       bool
		provider    string
		minSeverity string
		panel       string
	)

	cmd := &cobra.Command{
		Use:   "review [commit] or review [start] [end]",
		Short: "Review a commit, commit range, or uncommitted changes",
		Long: `Review a commit, commit range, or uncommitted changes.

Examples:
  roborev review              # Review HEAD
  roborev review abc123       # Review specific commit
  roborev review abc123 def456  # Review range from abc123 to def456 (inclusive)
  roborev review --dirty      # Review uncommitted changes
  roborev review --dirty --wait  # Review uncommitted changes and wait for result
  roborev review --type design   # Design-focused review of HEAD
  roborev review --branch     # Review all commits on current branch since main
  roborev review --branch --base develop  # Review branch against develop
  roborev review --branch=feature-xyz     # Review a specific branch
  roborev review --since HEAD~5  # Review last 5 commits
  roborev review --since abc123  # Review commits since abc123 (exclusive)
  roborev review --type security   # Security-focused review of HEAD
  roborev review --branch --type security  # Security review of branch
  roborev review --type lookahead  # Time-series look-ahead bias review of HEAD
`,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			ctx := cmd.Context()
			// In quiet mode, any error becomes a silent non-zero exit
			// (hook backgrounds the call, so output would be noise).
			if quiet {
				defer func() { err = quietExit(cmd, err) }()
			}

			// --fast is shorthand for --reasoning fast (explicit --reasoning takes precedence)
			reasoning = resolveReasoningWithFast(reasoning, fast, cmd.Flags().Changed("reasoning"))

			// Default to current directory
			if repoPath == "" {
				repoPath = "."
			}

			// Get repo root
			root, err := gitrepo.Root(ctx, repoPath)
			if err != nil {
				if quiet {
					return nil // Not a repo - silent exit for hooks
				}
				// Scan for child git repos to give a helpful hint
				if children := findChildGitRepos(repoPath); len(children) > 0 {
					absDir, _ := filepath.Abs(repoPath)
					var b strings.Builder
					b.WriteString("not in a git repository; use --repo to specify one:")
					for _, name := range children {
						b.WriteString("\n  roborev review --repo ")
						b.WriteString(filepath.Join(absDir, name))
					}
					return fmt.Errorf("%s", b.String())
				}
				return fmt.Errorf("not a git repository: %w", err)
			}

			// Skip during rebase to avoid reviewing every replayed commit
			if git.IsRebaseInProgress(root) {
				if !quiet {
					cmd.Println("Skipping: rebase in progress")
				}
				return nil // Intentional skip, exit 0
			}

			// Validate mutually exclusive options
			if branch != "" && dirty {
				return usageErr(cmd, fmt.Errorf("cannot use --branch with --dirty"))
			}
			if branch != "" && since != "" {
				return usageErr(cmd, fmt.Errorf("cannot use --branch with --since"))
			}
			if since != "" && dirty {
				return usageErr(cmd, fmt.Errorf("cannot use --since with --dirty"))
			}
			if branch != "" && len(args) > 0 {
				return usageErr(cmd, fmt.Errorf("cannot specify commits with --branch (to review a specific branch, use --branch=<name>)"))
			}
			if since != "" && len(args) > 0 {
				return usageErr(cmd, fmt.Errorf("cannot specify commits with --since"))
			}

			// Validate --type flag
			if reviewType != "" && reviewType != "security" && reviewType != "design" && reviewType != "lookahead" {
				return usageErr(cmd, fmt.Errorf("invalid --type %q (valid: security, design, lookahead)", reviewType))
			}

			// Auto-install/upgrade hooks when running from CLI
			// (not when called from a hook via --quiet).
			// Runs after validation so invalid args don't
			// cause side effects.
			if !quiet {
				autoInstallHooks(ctx, root)
			}

			// Ensure daemon is running (skip for --local mode)
			if !local {
				if err := ensureDaemon(); err != nil {
					return err // Return error (quiet mode silences output, not exit code)
				}
			}

			var gitRef string
			var diffContent string
			var dirtyFiles []string

			if branch != "" {
				// Branch review - review all commits since diverging from base
				targetRef := "HEAD"
				targetLabel := gitrepo.CurrentBranch(ctx, root)
				if branch != "HEAD" {
					targetRef = branch
					targetLabel = branch
					if _, err := gitrepo.Resolve(ctx, root, targetRef); err != nil {
						return fmt.Errorf("cannot resolve branch %q: %w", branch, err)
					}
				}

				base := baseBranch
				if base == "" {
					base = git.GetBranchBase(root, targetRef)
				}
				if base == "" {
					// Prefer the branch's upstream tracking ref only when it resolves
					// to a trunk-named branch (e.g., local main tracking upstream/main
					// in a fork). A branch tracking its own remote counterpart
					// (e.g., feature tracking origin/feature) is not trunk — using it
					// would skip already-pushed feature commits, contradicting
					// "--branch reviews all commits since trunk".
					upstream, uerr := git.GetUpstream(root, targetRef)
					var missing *git.UpstreamMissingError
					if errors.As(uerr, &missing) {
						return fmt.Errorf("%w (or pass --base <ref>)", missing)
					}
					if uerr != nil {
						return fmt.Errorf("resolve upstream for %s: %w (pass --base <ref> to skip)", targetRef, uerr)
					}
					if upstream != "" && git.UpstreamIsTrunk(root, targetRef) {
						base = upstream
					}
				}
				if base == "" {
					var err error
					base, err = gitrepo.DefaultBranch(ctx, root)
					if err != nil {
						return fmt.Errorf("cannot determine base branch: %w", err)
					}
				}

				// Validate not on base branch (only when reviewing current branch).
				// With UpstreamIsTrunk gating, `base` is either the user's --base
				// override, a trunk-shaped upstream, or GetDefaultBranch — never a
				// self-counterpart, so this check fires only for true trunk cases.
				if targetRef == "HEAD" {
					currentBranch := gitrepo.CurrentBranch(ctx, root)
					if git.IsOnBaseBranch(root, currentBranch, base) {
						return fmt.Errorf("already on %s - create a feature branch first", currentBranch)
					}
				}

				// Get merge-base
				mergeBase, err := git.GetMergeBase(root, base, targetRef)
				if err != nil {
					return fmt.Errorf("cannot find merge-base with %s: %w", base, err)
				}

				// Validate has commits
				rangeRef := mergeBase + ".." + targetRef
				commits, err := git.GetRangeCommits(root, rangeRef)
				if err != nil {
					return fmt.Errorf("cannot get commits: %w", err)
				}
				if len(commits) == 0 {
					return fmt.Errorf("no commits on branch since %s", base)
				}

				gitRef = rangeRef

				if !quiet {
					cmd.Printf("Reviewing branch %q: %d commits since %s\n",
						targetLabel, len(commits), base)
				}
			} else if since != "" {
				// Review commits since a specific commit (exclusive)
				sinceCommit, err := gitrepo.Resolve(ctx, root, since)
				if err != nil {
					return fmt.Errorf("invalid --since commit %q: %w", since, err)
				}

				// Validate has commits
				commits, err := git.GetCommitsSince(root, sinceCommit)
				if err != nil {
					return fmt.Errorf("cannot get commits: %w", err)
				}
				if len(commits) == 0 {
					return fmt.Errorf("no commits since %s", since)
				}

				gitRef = sinceCommit + ".." + "HEAD"

				if !quiet {
					cmd.Printf("Reviewing %d commits since %s\n", len(commits), since)
				}
			} else if dirty {
				// Dirty review - capture uncommitted changes
				hasChanges, err := gitrepo.HasUncommittedChanges(ctx, root)
				if err != nil {
					return fmt.Errorf("check uncommitted changes: %w", err)
				}
				if !hasChanges {
					return fmt.Errorf("no uncommitted changes to review")
				}
				dirtyFiles, err = git.GetDirtyFilesChanged(root)
				if err != nil {
					return fmt.Errorf("get dirty files: %w", err)
				}

				// Generate dirty diff (includes untracked files).
				// Use working-tree repo config (not default branch) for
				// dirty reviews so local exclude_patterns changes apply.
				globalCfg, _ := config.LoadGlobal()
				excludes := config.ResolveExcludePatternsLocal(
					root, globalCfg, reviewType,
				)
				diffContent, err = git.GetDirtyDiff(root, excludes...)
				if err != nil {
					return fmt.Errorf("get dirty diff: %w", err)
				}

				// Check size limit
				if len(diffContent) > MaxDirtyDiffSize {
					return fmt.Errorf("dirty diff too large (%d bytes, max %d bytes)\nConsider committing changes in smaller chunks",
						len(diffContent), MaxDirtyDiffSize)
				}

				if diffContent == "" && !prompt.HasDependencyMetadataFiles(dirtyFiles) {
					return fmt.Errorf("no changes to review (diff is empty)")
				}

				gitRef = "dirty"
			} else if len(args) >= 2 {
				// Range: START END -> START^..END (inclusive)
				gitRef = args[0] + "^.." + args[1]
			} else if len(args) == 1 {
				// Single commit
				gitRef = args[0]
			} else {
				gitRef = sha
			}

			// Get branch name for tracking. When --branch=<name> targets
			// a different branch, use that name instead of the checked-out branch.
			branchName := gitrepo.CurrentBranch(ctx, root)
			if branch != "" && branch != "HEAD" {
				branchName = branch
			}

			// Handle --local mode: run agent directly without daemon
			if local {
				// Panels are resolved daemon-side, so --local can only run a
				// single agent. Warn rather than silently dropping the panel.
				if panel != "" && panel != "none" && !quiet {
					cmd.PrintErrf("Note: --panel %q is ignored with --local (panels require the daemon); running a single-agent review.\n", panel)
				}
				return runLocalReview(cmd, root, gitRef, diffContent, dirtyFiles, agent, model, provider, reasoning, reviewType, quiet, minSeverity)
			}

			// Build request body
			reqFields := daemon.EnqueueRequest{
				RepoPath:    root,
				GitRef:      gitRef,
				Branch:      branchName,
				Agent:       agent,
				Model:       model,
				Provider:    provider,
				Reasoning:   reasoning,
				ReviewType:  reviewType,
				DiffContent: diffContent,
				DirtyFiles:  dirtyFiles,
				MinSeverity: minSeverity,
				Panel:       panel,
			}

			reqBody, _ := json.Marshal(reqFields)

			ep := getDaemonEndpoint()
			resp, err := ep.HTTPClient(10*time.Second).Post(ep.BaseURL()+"/api/enqueue", "application/json", bytes.NewReader(reqBody))
			if err != nil {
				return fmt.Errorf("failed to connect to daemon: %w", err)
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)

			// Handle skipped response (200 OK with skipped flag)
			if resp.StatusCode == http.StatusOK {
				var skipResp struct {
					Skipped bool   `json:"skipped"`
					Reason  string `json:"reason"`
				}
				if err := json.Unmarshal(body, &skipResp); err == nil && skipResp.Skipped {
					if !quiet {
						cmd.Printf("Skipped: %s\n", skipResp.Reason)
					}
					return nil
				}
			}

			if resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("review failed: %s", body)
			}

			// A panel enqueue returns a PanelEnqueueResponse: the embedded
			// synthesis job plus member_job_ids. A single-agent enqueue returns
			// a bare ReviewJob (no member_job_ids). Decoding into the embedded
			// shape handles both.
			var enq struct {
				storage.ReviewJob
				MemberJobIDs []int64 `json:"member_job_ids"`
			}
			_ = json.Unmarshal(body, &enq)
			job := enq.ReviewJob

			if !quiet {
				cmd.Println(describeEnqueue(job, len(enq.MemberJobIDs), dirty))
			}

			// If --wait, poll until job completes and show result.
			// Verdict-fail comes back as a bare *exitError; silence cobra
			// here so the user doesn't see "Error: exit code 1" after the
			// review output is already on screen.
			if wait {
				return silenceIfExit(cmd, waitForJob(cmd, ep, job.ID, quiet))
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "path to git repository (default: current directory)")
	cmd.Flags().StringVar(&sha, "sha", "HEAD", "commit SHA to review (used when no positional args)")
	cmd.Flags().StringVar(&agent, "agent", "", "agent to use (codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, pi)")
	cmd.Flags().StringVar(&model, "model", "", "model for agent (format varies: opencode uses provider/model, others use model name)")
	cmd.Flags().StringVar(&reasoning, "reasoning", "", "reasoning level: fast, standard, medium, thorough (default), or maximum")
	cmd.Flags().BoolVar(&fast, "fast", false, "shorthand for --reasoning fast")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress informational output and the usage block; runtime errors are still printed")
	cmd.Flags().BoolVar(&dirty, "dirty", false, "review uncommitted changes instead of a commit")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for review to complete and show result")
	cmd.Flags().StringVar(&branch, "branch", "", "review all changes since branch diverged from base (optionally specify branch name)")
	cmd.Flags().Lookup("branch").NoOptDefVal = "HEAD"
	cmd.Flags().StringVar(&baseBranch, "base", "", "base branch for --branch comparison (default: auto-detect)")
	cmd.Flags().StringVar(&since, "since", "", "review commits since this commit (exclusive, like git's .. range)")
	cmd.Flags().BoolVar(&local, "local", false, "run review locally without daemon (streams output to console)")
	cmd.Flags().StringVar(&reviewType, "type", "", "review type (security, design, lookahead) — changes system prompt")
	cmd.Flags().StringVar(&provider, "provider", "", "provider for pi agent (e.g. anthropic, openai)")
	cmd.Flags().StringVar(&minSeverity, "min-severity", "", "minimum severity threshold: critical, high, medium, low")
	cmd.Flags().StringVar(&panel, "panel", "", "review panel to fan out to (config panel name; 'none' forces single-agent)")
	registerAgentCompletion(cmd)
	registerReasoningCompletion(cmd)
	registerReviewTypeCompletion(cmd)

	return cmd
}

// runLocalReview runs a review directly without the daemon
func runLocalReview(cmd *cobra.Command, repoPath, gitRef, diffContent string, dirtyFiles []string, agentName, model, provider, reasoning, reviewType string, quiet bool, minSeverity string) error {
	ctx := cmd.Context()

	// Load config
	cfg, err := config.LoadGlobal()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Resolve and validate reasoning (matches daemon behavior)
	reasoning, err = config.ResolveReviewReasoning(reasoning, repoPath, cfg)
	if err != nil {
		return fmt.Errorf("invalid reasoning: %w", err)
	}

	// Resolve review min-severity
	resolvedMinSev, err := config.ResolveReviewMinSeverity(minSeverity, repoPath, cfg)
	if err != nil {
		return fmt.Errorf("invalid min-severity: %w", err)
	}

	// Map review_type to config workflow (matches daemon behavior)
	workflow := "review"
	if !config.IsDefaultReviewType(reviewType) {
		workflow = reviewType
	}
	if err := config.ValidateRepoConfig(repoPath); err != nil {
		return fmt.Errorf("resolve workflow config: %w", err)
	}

	// Resolve agent/model preferences (matches daemon behavior).
	resolution, err := agent.ResolveWorkflowConfig(
		agentName, repoPath, cfg, workflow, reasoning,
	)
	if err != nil {
		return fmt.Errorf("resolve workflow config: %w", err)
	}

	// Get the agent (try backup before hardcoded chain)
	a, err := agent.GetPreferredOrBackupWithConfig(
		repoPath, resolution.PreferredAgent, cfg, resolution.BackupAgent,
	)
	if err != nil {
		return fmt.Errorf("get agent: %w", err)
	}

	// Configure agent with model and reasoning. applyModelForAgent
	// handles backup-vs-primary model resolution.
	reasoningLevel := agent.ParseReasoningLevel(reasoning)
	a = a.WithReasoning(reasoningLevel)
	a, model = applyModelForAgent(
		a, resolution.PreferredAgent, resolution.BackupAgent,
		model, repoPath, cfg, workflow, reasoning,
	)
	a = agent.WithCodexSkillsDisabled(
		a,
		config.ResolveDisableCodexReviewSkills(repoPath, cfg),
	)
	a = agent.WithCodexUserConfigIgnored(
		a,
		config.ResolveIgnoreCodexReviewUserConfig(repoPath, cfg),
	)

	// Configure provider for pi agent
	if provider != "" {
		if pa, ok := a.(*agent.PiAgent); ok {
			a = pa.WithProvider(provider)
		}
	}

	// Use consistent output writer, respecting --quiet
	out := cmd.OutOrStdout()
	if quiet {
		out = io.Discard
	}

	if !quiet {
		fmt.Fprintf(out, "Running %s review (model: %s, reasoning: %s)...\n\n", a.Name(), model, reasoning)
	}

	// Build prompt
	pb := prompt.NewBuilderWithConfig(nil, cfg).WithContext(ctx).ForRepo(repoPath, 0).WithKataClient(kata.NewCLIClient(repoPath))
	var reviewPrompt string
	var snapshotCleanup func()
	if diffContent != "" || len(dirtyFiles) > 0 {
		// Dirty review
		dirtyResult, dirtyErr := pb.BuildDirtyWithSnapshotAndFiles(diffContent, dirtyFiles, cfg.ReviewContextCount, a.Name(), reviewType, resolvedMinSev)
		reviewPrompt = dirtyResult.Prompt
		snapshotCleanup = dirtyResult.Cleanup
		err = dirtyErr
	} else {
		excludes := config.ResolveExcludePatterns(ctx, repoPath, cfg, reviewType)
		result, buildErr := pb.BuildWithSnapshot(gitRef, cfg.ReviewContextCount, a.Name(), reviewType, resolvedMinSev, excludes)
		reviewPrompt = result.Prompt
		snapshotCleanup = result.Cleanup
		err = buildErr
	}
	if snapshotCleanup != nil {
		defer snapshotCleanup()
	}
	if err != nil {
		return fmt.Errorf("build prompt: %w", err)
	}

	// Run review with output writer
	_, err = a.Review(cmd.Context(), repoPath, gitRef, reviewPrompt, out)
	if err != nil {
		return fmt.Errorf("review failed: %w", err)
	}

	if !quiet {
		fmt.Fprintln(out) // Final newline
	}
	return nil
}

// findChildGitRepos returns the names of immediate child directories that are git repos.
func findChildGitRepos(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var repos []string
	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] == '.' {
			continue
		}
		gitDir := filepath.Join(dir, e.Name(), ".git")
		if _, err := os.Stat(gitDir); err == nil {
			repos = append(repos, e.Name())
		}
	}
	return repos
}

// tryBranchReview checks the repo config for post_commit_review = "branch".
// When set, it returns a merge-base..HEAD range ref for the current branch.
// Returns ("", false) silently on any error — hooks must never block commits.
func tryBranchReview(ctx context.Context, root, baseBranchOverride string) (string, bool) {
	mode := config.ResolvePostCommitReview(root)
	if mode != "branch" {
		return "", false
	}

	base := baseBranchOverride
	if base == "" {
		base = git.GetBranchBase(root, "HEAD")
	}
	if base == "" {
		// Prefer the branch's upstream tracking ref only when it resolves to
		// trunk. Hooks must never block commits, but any GetUpstream failure
		// (missing ref, corrupt config, subprocess error) means we cannot
		// confidently pick a base — skip instead of falling back.
		upstream, err := git.GetUpstream(root, "HEAD")
		if err != nil {
			return "", false
		}
		if upstream != "" && git.UpstreamIsTrunk(root, "HEAD") {
			base = upstream
		}
	}
	if base == "" {
		var err error
		base, err = gitrepo.DefaultBranch(ctx, root)
		if err != nil {
			return "", false
		}
	}

	// Don't branch-review in detached HEAD or on the base branch
	current := gitrepo.CurrentBranch(ctx, root)
	if current == "" || git.IsOnBaseBranch(root, current, base) {
		return "", false
	}

	mergeBase, err := git.GetMergeBase(root, base, "HEAD")
	if err != nil {
		return "", false
	}

	rangeRef := mergeBase + "..HEAD"
	commits, err := git.GetRangeCommits(root, rangeRef)
	if err != nil || len(commits) == 0 {
		return "", false
	}

	return rangeRef, true
}
