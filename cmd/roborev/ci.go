package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	ghpkg "go.kenn.io/roborev/internal/github"
	glpkg "go.kenn.io/roborev/internal/gitlab"
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
		glRepoFlag     string
		glHostFlag     string
		upsertFlag     bool
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
			`synthesize results, and output or post a PR/MR comment.

This command is designed for CI pipelines. It reads ` +
			`configuration from .roborev.toml and runs without ` +
			`a daemon or database.

Flags override config values. When run inside GitHub ` +
			`Actions, --ref, --gh-repo, and --pr are ` +
			`auto-detected from the environment. When run inside ` +
			`GitLab CI, --ref, --gl-repo, and --pr (the merge ` +
			`request IID) are auto-detected instead.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Tri-state: absent leaves the config chain alone, present pins
			// it, which is the point in the protected flow where the merge
			// request's own .roborev.toml would otherwise decide.
			var upsert *bool
			if cmd.Flags().Changed("upsert-comments") {
				upsert = &upsertFlag
			}
			return runCIReview(
				cmd.Context(), ciReviewOpts{
					ref:            refFlag,
					comment:        commentFlag,
					ghRepo:         ghRepoFlag,
					glRepo:         glRepoFlag,
					glHost:         glHostFlag,
					upsertComments: upsert,
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
			"auto-detected in GitHub Actions and GitLab CI")
	cmd.Flags().BoolVar(&commentFlag, "comment", false,
		"post result as a PR comment (GitHub) or MR note (GitLab)")
	cmd.Flags().StringVar(&ghRepoFlag, "gh-repo", "",
		"GitHub repo owner/name (auto from GITHUB_REPOSITORY)")
	cmd.Flags().StringVar(&glRepoFlag, "gl-repo", "",
		"GitLab project group/name "+
			"(auto from CI_MERGE_REQUEST_PROJECT_PATH / CI_PROJECT_PATH)")
	cmd.Flags().StringVar(&glHostFlag, "gl-host", "",
		"GitLab server URL or hostname; selects GitLab and overrides "+
			"CI_SERVER_URL / GITLAB_HOST / GL_HOST (pin this in the job "+
			"script when GITLAB_TOKEN is a protected variable)")
	cmd.MarkFlagsMutuallyExclusive("gh-repo", "gl-repo")
	cmd.MarkFlagsMutuallyExclusive("gh-repo", "gl-host")
	cmd.Flags().BoolVar(&upsertFlag, "upsert-comments", false,
		"update the previous roborev comment instead of adding a new one "+
			"(overrides config; pin --upsert-comments=false to keep the "+
			"record append-only when repo config is not trusted)")
	cmd.Flags().IntVar(&prFlag, "pr", 0,
		"PR number / GitLab MR IID "+
			"(auto from event JSON / GITHUB_REF / CI_MERGE_REQUEST_IID)")
	cmd.Flags().StringVar(&agentFlag, "agent", "",
		"comma-separated agent list (overrides config)")
	cmd.Flags().StringVar(&reviewTypes, "review-types", "",
		"comma-separated review types (overrides config)")
	cmd.Flags().StringVar(&reasoning, "reasoning", "",
		"reasoning level: legacy presets fast, standard, thorough, maximum; exact tiers low, medium, high, xhigh, max")
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
	glRepo         string
	glHost         string
	upsertComments *bool
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
	// A negative --pr would otherwise reach the API as
	// .../merge_requests/-3/notes and 404 opaquely, after the
	// whole review matrix has already run.
	if opts.pr < 0 {
		return fmt.Errorf(
			"--pr must be a positive number (got %d)", opts.pr)
	}

	// Determine which forge this run targets before anything
	// forge-specific (ref detection, comment posting).
	forge, err := detectCIForge(opts)
	if err != nil {
		return err
	}

	// With --comment the run ends in an API call, so check the
	// credential now rather than after the review matrix. The
	// likeliest misconfiguration is a Protected GITLAB_TOKEN,
	// which a merge-request-branch pipeline cannot read at all —
	// failing afterwards would waste a full multi-agent review.
	if opts.comment {
		if err := checkForgeAuth(ctx, forge, opts); err != nil {
			return err
		}
	}

	// Determine repo root
	root, err := gitrepo.Root(ctx, ".")
	if err != nil {
		return fmt.Errorf(
			"not a git repository — " +
				"run this from inside a git repo")
	}

	// Resolve git ref (from flag or environment). Auto-detection
	// may need the checkout to resolve a merge base, so it runs
	// after the repo root is known.
	gitRef := opts.ref
	if gitRef == "" {
		detected, err := detectGitRefForForge(forge, root)
		switch {
		case errors.Is(err, errNoChangesToReview):
			// Not a failure: the push added nothing to review, so there is
			// no verdict to reach and nothing to post.
			fmt.Println("roborev: no changes to review")
			return nil
		case err != nil:
			return fmt.Errorf(
				"--ref not provided and auto-detection "+
					"failed: %w", err)
		}
		gitRef = detected
	}

	// Bind the range to the merge request before spending the review matrix
	// on it. Posting checks again afterwards, to catch a force push that
	// lands while the reviews run.
	if opts.comment && forge == ciForgeGitLab {
		canonical, err := verifyGitLabMRRange(ctx, opts, root, gitRef, false)
		switch {
		case errors.Is(err, errMRHeadMoved):
			fmt.Println("roborev: " + err.Error() + " — nothing to post")
			return nil
		case err != nil:
			return err
		case canonical != "":
			// Review exactly the commits that were checked, not the
			// expressions they came from.
			gitRef = canonical
		}
	}

	// Load configs (warn on error, don't fail)
	globalCfg := loadCIGlobalConfig(root)
	repoCfg, err := config.LoadRepoConfig(root)
	if err != nil {
		log.Printf(
			"ci review: load repo config: %v "+
				"(using defaults)", err)
	}

	// The Claude adapter strips ANTHROPIC_API_KEY from the inherited
	// environment and re-injects only the key roborev was given, so hand
	// it over explicitly before any review runs.
	cfgAnthropicKey := ""
	if globalCfg != nil {
		cfgAnthropicKey = globalCfg.AnthropicAPIKey
	}
	agent.SetAnthropicAPIKey(
		resolveCIAnthropicAPIKey(cfgAnthropicKey))

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
	reviewMinSev, err := resolveCIReviewMinSeverity(opts, root, globalCfg)
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
	//
	// RepoPath is the current checkout, not a worktree pinned at the
	// range's head: agents diff `gitRef` but open files from whatever is
	// checked out. In a merge request / pull request pipeline those are
	// the same tree. In the protected-branch flow (--pr and --ref handed
	// over by a trigger, head fetched via refs/merge-requests/<iid>/head)
	// they are not, so file context comes from the protected branch. See
	// docs/integrations/gitlab.md for the operator-side workaround.
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

	// Post as PR/MR comment if requested
	if opts.comment {
		upsert := resolveCIUpsert(opts, repoCfg, globalCfg)
		switch err := postCIReviewComment(
			ctx, forge, opts, results, comment, upsert, root, gitRef,
		); {
		case errors.Is(err, errMRHeadMoved):
			// Same benign race as before the review, so same outcome: the
			// verdict is about code the merge request no longer has, and a
			// pipeline for the new head will post its own.
			fmt.Println("roborev: " + err.Error() + " — nothing to post")
			return nil
		case err != nil:
			return err
		}
	}

	// Exit non-zero when all review jobs failed
	if synthErr != nil {
		return synthErr
	}

	return nil
}

func postCIReviewComment(
	ctx context.Context,
	forge ciForge,
	opts ciReviewOpts,
	results []review.ReviewResult,
	comment string,
	upsert bool,
	repoPath string,
	gitRef string,
) error {
	if !review.HasSubstantiveOutput(results) {
		return nil
	}
	return postForgeComment(ctx, forge, opts, comment, upsert, repoPath, gitRef)
}

// resolveCIAnthropicAPIKey picks the Anthropic API key for a pipeline run.
// The global config field wins so an explicitly configured key keeps
// precedence, matching `roborev refine`; otherwise the ANTHROPIC_API_KEY
// environment variable is used, which is how CI systems hand the credential
// to the job.
// resolveCIReviewMinSeverity picks the threshold the member reviews are
// prompted with. --min-severity is passed through as the explicit value so the
// flag pins both halves of the run: without it, only synthesis honored the
// flag while the review prompts took `review_min_severity` from the checkout's
// .roborev.toml. In the protected MR-head worktree flow that file belongs to
// the author under review, who could raise it to "critical" and keep medium
// and high findings from ever being reported — leaving synthesis nothing to
// filter and the job looking clean.
func resolveCIReviewMinSeverity(
	opts ciReviewOpts, repoPath string, globalCfg *config.Config,
) (string, error) {
	return config.ResolveReviewMinSeverity(
		opts.minSeverity, repoPath, globalCfg)
}

// loadCIGlobalConfig loads the operator's global configuration, unless it
// resolves inside the repository being reviewed.
//
// Global config is trusted: its <agent>_cmd entries name the binaries roborev
// executes. Its location comes from ROBOREV_DATA_DIR, or from HOME through the
// default, and both are ordinary environment variables a pipeline starter can
// set. Pointing either into the checkout would have roborev read a file the
// merge request author committed and run a program they chose.
//
// The check is on where the path lands rather than how it looks, so it covers
// a relative path resolving against the working directory, an absolute one
// naming the checkout, and a symlink such as /proc/self/cwd that is absolute
// but resolves inside it. Symlinks are resolved on both sides before the
// comparison for that reason.
func loadCIGlobalConfig(root string) *config.Config {
	path := config.GlobalConfigPath()
	if insideRepo(root, path) {
		log.Printf(
			"ci review: ignoring global config at %s: it resolves inside "+
				"the repository under review, whose contents are not "+
				"trusted (using defaults)", path)
		return nil
	}
	cfg, err := config.LoadGlobal()
	if err != nil {
		log.Printf(
			"ci review: load global config: %v (using defaults)", err)
	}
	return cfg
}

// insideRepo reports whether path lands inside root once both are resolved.
// An unresolvable path is treated as inside: the point is to decide whether a
// file can be trusted, and "could not tell" is not a yes.
func insideRepo(root, path string) bool {
	if strings.TrimSpace(root) == "" {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	// The config file itself need not exist, so resolve the directory holding
	// it; that is the part an attacker would point at the checkout.
	resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		// A data directory that does not exist cannot hold a committed
		// config either, so there is nothing to distrust.
		return false
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedDir)
	if err != nil {
		return true
	}
	return rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveCIUpsert decides whether to replace the previous roborev comment.
// The flag wins so a protected job can pin it: upsert_comments is otherwise
// the one [ci] setting with no override, and in the merge-request worktree
// flow the author's .roborev.toml would decide whether an earlier note — the
// findings in it included — gets replaced.
func resolveCIUpsert(
	opts ciReviewOpts, repoCfg *config.RepoConfig, globalCfg *config.Config,
) bool {
	if opts.upsertComments != nil {
		return *opts.upsertComments
	}
	return config.ResolveCIUpsertComments(repoCfg, globalCfg)
}

func resolveCIAnthropicAPIKey(configKey string) string {
	if key := strings.TrimSpace(configKey); key != "" {
		return key
	}
	return strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
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

// ciForge identifies which forge a `ci review` run targets.
type ciForge string

const (
	ciForgeGitHub ciForge = "github"
	ciForgeGitLab ciForge = "gitlab"
)

// detectCIForge decides which forge to talk to. Precedence, highest first:
//
//  1. explicit flags: --gl-repo / --gl-host select GitLab, --gh-repo selects
//     GitHub (--gh-repo cannot be combined with either GitLab flag)
//  2. GitLab CI environment (GITLAB_CI=true)
//  3. GitHub Actions environment (GITHUB_ACTIONS=true)
//
// --gl-host counts as forge selection because it exists to pin the API origin
// against variables injected at pipeline start. A pin that an injected
// GITHUB_ACTIONS=true could steer to the GitHub client — whose origin comes
// from the equally injectable GITHUB_API_URL, and whose token is present in
// jobs that run the copilot or kiro agents — would not pin anything.
//
// The env order puts GITLAB_CI first for the same reason: the two indicators
// are not equally trustworthy when both are present. GitHub Actions job env
// comes from the runner or committed workflow code, so GITLAB_CI inside real
// Actions can only be set deliberately by trusted code, while GitLab pipeline
// variables outrank predefined ones, so GITHUB_ACTIONS inside real GitLab CI
// can be injected by whoever starts the pipeline.
//
// Anything else defaults to GitHub, which keeps the pre-GitLab behavior of
// this command unchanged.
func detectCIForge(opts ciReviewOpts) (ciForge, error) {
	if opts.ghRepo != "" && opts.glRepo != "" {
		return "", fmt.Errorf(
			"--gh-repo and --gl-repo are mutually exclusive")
	}
	if opts.ghRepo != "" && opts.glHost != "" {
		return "", fmt.Errorf(
			"--gh-repo and --gl-host are mutually exclusive")
	}
	if opts.glRepo != "" || opts.glHost != "" {
		return ciForgeGitLab, nil
	}
	if opts.ghRepo != "" {
		return ciForgeGitHub, nil
	}
	if isTruthyEnv("GITLAB_CI") {
		return ciForgeGitLab, nil
	}
	if isTruthyEnv("GITHUB_ACTIONS") {
		return ciForgeGitHub, nil
	}
	return ciForgeGitHub, nil
}

// isTruthyEnv reports whether an env var is set to a CI-style truthy value.
func isTruthyEnv(key string) bool {
	value := strings.TrimSpace(os.Getenv(key))
	return strings.EqualFold(value, "true") || value == "1"
}

// detectGitRefForForge auto-detects the ref range for the active forge.
// repoPath is the repository checkout, used when detection has to resolve a
// merge base locally.
func detectGitRefForForge(forge ciForge, repoPath string) (string, error) {
	if forge == ciForgeGitLab {
		return detectGitLabGitRef(repoPath)
	}
	return detectGitRef()
}

// postForgeComment posts the synthesized review to the active forge.
// repoPath and gitRef describe the range that was reviewed, used to check that
// the verdict still describes the merge request it is about to land on.
func postForgeComment(
	ctx context.Context,
	forge ciForge,
	opts ciReviewOpts,
	body string,
	upsert bool,
	repoPath, gitRef string,
) error {
	if forge == ciForgeGitLab {
		return postGitLabCIComment(ctx, opts, body, upsert, repoPath, gitRef)
	}
	return postGitHubCIComment(ctx, opts, body, upsert)
}

// errMRHeadMoved reports that the merge request advanced past the commits this
// run reviewed. The verdict is about code the merge request no longer has, so
// there is nothing valid left to post; the run stops without failing.
var errMRHeadMoved = errors.New("merge request head moved during the review")

// headMovedError reports that the merge request advanced past the reviewed
// commits, wrapping errMRHeadMoved so callers exit cleanly instead of failing.
func headMovedError(reviewed string, mrIID int, current string) error {
	return fmt.Errorf("%w: reviewed %s, merge request !%d is now at %s",
		errMRHeadMoved, reviewed, mrIID, current)
}

// verifyGitLabMRRange checks that the reviewed range is the merge request the
// note will land on, in both extent and head.
//
// With an explicit --pr — the protected-branch flow — --pr and --ref are
// independent inputs chosen by whoever starts the pipeline, so the range is
// validated in full: it must be a real BASE..HEAD range, end at the merge
// request's head, and start no later than the merge request's own base.
// Checking only the head would still let HEAD~1..HEAD post a passing verdict
// covering the last commit of many, or — with upsert — replace a note that
// carried findings.
//
// A starting point earlier than the merge request's base is allowed: it
// reviews more than the merge request, which omits nothing. This also absorbs
// the case where GitLab's recorded base is staler than the merge base the job
// computed locally.
//
// An auto-detected --pr is bound to the pipeline's own commits by GitLab, so
// the range itself is not re-derived. The head is still compared, because a
// force push landing mid-review would otherwise let this run post about code
// the merge request no longer has. That comparison uses the source branch SHA
// CI reported rather than the checked-out commit, which in a merged-results
// pipeline is a synthetic merge commit that never equals the head.
func verifyGitLabMRRange(
	ctx context.Context, opts ciReviewOpts, repoPath, gitRef string,
	recheck bool,
) (string, error) {
	// Project first, then merge request, so the message names whichever is
	// missing in the same order posting would have reported it.
	glRepo := opts.glRepo
	if glRepo == "" {
		glRepo = detectGitLabProjectPath()
	}
	if glRepo == "" {
		return "", fmt.Errorf(
			"--comment requires --gl-repo or " +
				"CI_MERGE_REQUEST_PROJECT_PATH/CI_PROJECT_PATH env var")
	}

	mrIID := opts.pr
	if mrIID == 0 {
		detected, err := detectMRIID()
		if err != nil {
			// Reported here rather than at posting time: this runs before the
			// review matrix, so a run that can never post fails in a second
			// instead of after a full multi-agent review.
			return "", fmt.Errorf(
				"--comment requires --pr or auto-detection from "+
					"CI_MERGE_REQUEST_IID: %w", err)
		}
		mrIID = detected
	}

	client, err := ciGitLabClient(ctx, opts.glHost)
	if err != nil {
		return "", err
	}
	refs, err := client.MergeRequestRefs(ctx, glRepo, mrIID)
	if err != nil {
		return "", fmt.Errorf("verify merge request !%d: %w", mrIID, err)
	}

	base, head, ok := strings.Cut(gitRef, "..")
	base, head = strings.TrimSpace(base), strings.TrimSpace(head)
	if !ok || base == "" || head == "" {
		return "", fmt.Errorf(
			"--comment on merge request !%d requires a BASE..HEAD range, "+
				"got %q: a single ref reviews one commit and would post a "+
				"verdict covering only that commit", mrIID, gitRef)
	}
	// Resolved once, here, and handed back for the review to use. Passing the
	// original expressions on would let a revision like ":/fix" name one
	// commit while it is checked and another while it is reviewed.
	base, head = resolveRev(repoPath, base), resolveRev(repoPath, head)
	if strings.EqualFold(base, head) {
		return "", fmt.Errorf(
			"--comment on merge request !%d requires a non-empty range, but "+
				"%q covers no commits", mrIID, gitRef)
	}
	if !strings.EqualFold(head, refs.HeadSHA) {
		if recheck {
			// The range matched when the review started, so the head moved
			// while it ran: a race, and the only case that provably is one.
			// A mismatch on the first pass is not — the range simply does not
			// describe this merge request, whether it was mistyped or the
			// variables it came from were overridden — so it fails loudly
			// rather than exiting clean with nothing reviewed.
			return "", headMovedError(head, mrIID, refs.HeadSHA)
		}
		return "", fmt.Errorf(
			"refusing to post: reviewed up to %s but merge request !%d is "+
				"at %s — the note would describe code the merge request does "+
				"not have (review the merge request head, or re-run against "+
				"the current one)",
			head, mrIID, refs.HeadSHA)
	}
	if err := verifyRangeCoversMR(repoPath, base, mrIID, refs.BaseSHA); err != nil {
		return "", err
	}
	return base + ".." + head, nil
}

// verifyRangeCoversMR checks that the reviewed range starts exactly where the
// merge request's diff does.
//
// Equality rather than "at least as much": a base earlier than the merge
// request's looks harmless, since reviewing more cannot omit commits, but the
// diff is computed tree to tree, so a change the merge request makes can cancel
// against an opposing change between the two bases and disappear from what the
// agents see. A merge request that reverts a recent commit is the ordinary way
// to hit that, and in the protected flow whoever supplies --ref could pick the
// base deliberately.
//
// An absent diff base fails closed. GitLab omits it on a merge request it has
// not finished preparing, and accepting any base in that window would let a
// narrowed range post a verdict covering part of the change.
func verifyRangeCoversMR(
	repoPath, base string, mrIID int, mrBase string,
) error {
	mrBase = strings.TrimSpace(mrBase)
	if mrBase == "" {
		return fmt.Errorf(
			"refusing to post: merge request !%d reports no diff base, so "+
				"the reviewed range cannot be checked against it (GitLab may "+
				"still be preparing the merge request — re-run once it is "+
				"ready)", mrIID)
	}
	if !strings.EqualFold(resolveRev(repoPath, base), resolveRev(repoPath, mrBase)) {
		return fmt.Errorf(
			"refusing to post: the reviewed range starts at %s but merge "+
				"request !%d's diff starts at %s, so the note would not "+
				"describe the same change (review %s..<merge request head>)",
			base, mrIID, mrBase, mrBase)
	}
	return nil
}

// resolveRev expands a ref or abbreviated SHA to a full commit SHA, returning
// it unchanged when the clone cannot resolve it so the caller compares — and
// reports — what was actually asked for.
func resolveRev(repoPath, rev string) string {
	if repoPath == "" || rev == "" {
		return rev
	}
	if full, err := git.ResolveSHA(repoPath, rev+"^{commit}"); err == nil {
		return full
	}
	return rev
}

func postGitHubCIComment(
	ctx context.Context,
	opts ciReviewOpts,
	body string,
	upsert bool,
) error {
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

	if err := postCIComment(
		ctx, ghRepo, prNumber, body, upsert,
	); err != nil {
		return fmt.Errorf("post PR comment: %w", err)
	}
	log.Printf(
		"ci review: posted comment on %s#%d",
		ghRepo, prNumber)
	return nil
}

func postGitLabCIComment(
	ctx context.Context,
	opts ciReviewOpts,
	body string,
	upsert bool,
	repoPath, gitRef string,
) error {
	// Re-checked here, not only before the review, so a force push landing
	// while the matrix ran cannot leave a verdict about code the merge
	// request no longer has.
	if _, err := verifyGitLabMRRange(
		ctx, opts, repoPath, gitRef, true,
	); err != nil {
		return err
	}

	glRepo := opts.glRepo
	if glRepo == "" {
		glRepo = detectGitLabProjectPath()
	}
	if glRepo == "" {
		return fmt.Errorf(
			"--comment requires --gl-repo or " +
				"CI_MERGE_REQUEST_PROJECT_PATH/" +
				"CI_PROJECT_PATH env var")
	}

	mrIID := opts.pr
	if mrIID == 0 {
		detected, err := detectMRIID()
		if err != nil {
			return fmt.Errorf(
				"--comment requires --pr or "+
					"auto-detection from "+
					"CI_MERGE_REQUEST_IID: %w", err)
		}
		mrIID = detected
	}

	client, err := ciGitLabClient(ctx, opts.glHost)
	if err != nil {
		return err
	}
	if upsert {
		err = client.UpsertMRComment(ctx, glRepo, mrIID, body)
	} else {
		err = client.CreateMRComment(ctx, glRepo, mrIID, body)
	}
	if err != nil {
		return fmt.Errorf("post MR comment: %w", err)
	}
	log.Printf(
		"ci review: posted comment on %s!%d",
		glRepo, mrIID)
	return nil
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

// zeroSHA is the all-zero SHA GitLab reports for the first push on a branch.
const zeroSHA = "0000000000000000000000000000000000000000"

// errNoChangesToReview reports that the push contains nothing this run should
// review. It is not a failure: the job stops early and exits zero.
var errNoChangesToReview = errors.New("no changes to review")

// detectGitLabProjectPath returns the GitLab project the merge request belongs
// to. CI_MERGE_REQUEST_PROJECT_PATH is the merge request's target project and
// is the one CI_MERGE_REQUEST_IID is scoped to; CI_PROJECT_PATH is the project
// running the pipeline, which is the fork in fork merge request pipelines.
// Outside merge request pipelines only CI_PROJECT_PATH is set.
func detectGitLabProjectPath() string {
	for _, key := range []string{
		"CI_MERGE_REQUEST_PROJECT_PATH",
		"CI_PROJECT_PATH",
	} {
		if path := strings.TrimSpace(os.Getenv(key)); path != "" {
			return path
		}
	}
	return ""
}

// detectGitLabGitRef auto-detects the git ref range from the GitLab CI
// environment. Merge request pipelines expose the diff base directly; when it
// is missing (branch pipelines, or a first push) the detection falls back to
// the target branch SHA, then the previous commit, then the HEAD commit alone.
//
// In merged results pipelines CI_COMMIT_SHA is GitLab's synthetic merge commit,
// so diffing it against the diff base would pull in target branch changes.
// CI_MERGE_REQUEST_SOURCE_BRANCH_SHA holds the real source branch HEAD there
// and is empty everywhere else, so it is preferred when populated and actually
// present in the clone.
//
// repoPath is the local checkout used to resolve a merge base for the fallback
// bases, which are branch tips rather than diff bases.
func detectGitLabGitRef(repoPath string) (string, error) {
	// The source branch HEAD is preferred, but only if this clone has it. A
	// shallow merged results checkout (GIT_DEPTH=1) fetches GitLab's synthetic
	// merge commit and nothing else, so CI_MERGE_REQUEST_SOURCE_BRANCH_SHA can
	// name a commit that is not here — and every later git operation would then
	// fail on it. Falling back to the checked-out SHA pulls target branch changes
	// into the diff, which is worse than a clean merged-results diff but still a
	// usable review; the base guard below makes the same trade.
	head, headKey := "", ""
	var absent []string
	for _, key := range []string{
		"CI_MERGE_REQUEST_SOURCE_BRANCH_SHA",
		"CI_COMMIT_SHA",
	} {
		candidate := strings.TrimSpace(os.Getenv(key))
		if candidate == "" || candidate == zeroSHA {
			continue
		}
		if !commitPresent(repoPath, candidate) {
			absent = append(absent,
				fmt.Sprintf("%s (%s)", candidate, key))
			continue
		}
		head, headKey = candidate, key
		break
	}
	if head == "" {
		if len(absent) > 0 {
			// Nothing left to degrade to: there is no commit to review. Say so
			// instead of returning a ref that fails deeper in the pipeline.
			return "", fmt.Errorf(
				"no head commit present in this clone, tried %s "+
					"(shallow clone? increase GIT_DEPTH or "+
					"fetch more history)",
				strings.Join(absent, ", "))
		}
		return "", fmt.Errorf(
			"CI_COMMIT_SHA not set (not a GitLab CI job?)")
	}
	if len(absent) > 0 {
		log.Printf(
			"ci review: head commit %s is not present in this clone "+
				"(shallow clone? increase GIT_DEPTH or fetch more history) "+
				"— reviewing %s (%s) instead",
			strings.Join(absent, ", "), head, headKey)
	}

	base, baseKey := "", ""
	for _, key := range []string{
		"CI_MERGE_REQUEST_DIFF_BASE_SHA",
		"CI_MERGE_REQUEST_TARGET_BRANCH_SHA",
		"CI_COMMIT_BEFORE_SHA",
	} {
		candidate := strings.TrimSpace(os.Getenv(key))
		if candidate != "" && candidate != zeroSHA {
			base, baseKey = candidate, key
			break
		}
	}
	if base == "" {
		// Nothing named a usable base. In a real pipeline that means a branch's
		// first push: merge request pipelines always set the diff base, and later
		// pushes set a non-zero CI_COMMIT_BEFORE_SHA, so only the first push
		// leaves every candidate empty or zero.
		//
		// A first push usually carries several commits, so there is a range here
		// even though no variable names it. Returning head alone would diff it
		// with `git show` — one commit — and post a verdict covering the last
		// commit of the push.
		return gitLabFirstPushRef(repoPath, head)
	}
	if baseKey != "CI_MERGE_REQUEST_DIFF_BASE_SHA" {
		base = gitLabMergeBase(repoPath, base, head, baseKey)
	}
	// A base was named but this clone does not have it — a shallow clone is the
	// usual reason, and also why merge-base resolution failed above. Both ways
	// out are bad, so take the honest one and fail.
	//
	// base..head cannot work: every later git operation would hit the same
	// missing commit. Narrowing to head alone is worse than failing, though,
	// because a single ref is diffed with `git show` — one commit — so a
	// twelve-commit merge request would be reviewed as its last commit and could
	// still post "Review Passed". A review that silently covered a fraction of
	// the change is more harmful than a job that stops and says why. This also
	// matches the head guard above, which errors rather than degrading when no
	// candidate head is present.
	if !commitPresent(repoPath, base) {
		return "", fmt.Errorf(
			"base commit %s (%s) is not present in this clone, so the "+
				"merge request range cannot be reviewed "+
				"(shallow clone? increase GIT_DEPTH or fetch more history)",
			base, baseKey)
	}
	return base + ".." + head, nil
}

// commitPresent reports whether rev names a commit object this clone actually
// has. Peeling with ^{commit} is what makes this a presence check: plain
// rev-parse echoes any well-formed SHA back without reading the object.
//
// Outside a git repository it reports true. There is no evidence either way
// there, and narrowing the review on the strength of a git command that could
// not run would be worse than trusting the SHA GitLab supplied.
func commitPresent(repoPath, rev string) bool {
	if repoPath == "" {
		repoPath = "."
	}
	if _, err := git.ResolveSHA(repoPath, rev+"^{commit}"); err == nil {
		return true
	}
	if _, err := git.ResolveGitDir(repoPath); err != nil {
		return true
	}
	return false
}

// gitLabFirstPushRef builds the range for a push that no CI variable describes,
// which in practice is a branch's first push.
//
// CI_DEFAULT_BRANCH names the branch the push diverged from, so its merge base
// with head is the range. That variable is itself overridable, like every
// predefined one — but it only selects a review scope, and whoever can override
// it already chose what the branch contains, so it is not the trust anchor
// problem an overridable API origin would be.
//
// When the default branch cannot be resolved — it is unset, or a shallow clone
// does not have it — there is no way to tell a one-commit push from a
// twelve-commit one. Fail and ask for an explicit --ref rather than review the
// tip and imply the rest passed.
func gitLabFirstPushRef(repoPath, head string) (string, error) {
	defaultBranch := strings.TrimSpace(os.Getenv("CI_DEFAULT_BRANCH"))
	if defaultBranch == "" {
		return "", fmt.Errorf(
			"no base commit available (first push?) and CI_DEFAULT_BRANCH is " +
				"not set, so the pushed range cannot be determined — " +
				"pass --ref BASE..HEAD explicitly")
	}

	// The default branch's own first push — a brand-new repository's initial
	// pipeline — lands here too, and the loop below cannot scope it: the
	// merge base of the default branch with itself is head, which would
	// narrow a multi-commit initial push to its tip.
	if strings.TrimSpace(os.Getenv("CI_COMMIT_BRANCH")) == defaultBranch {
		return gitLabDefaultBranchFirstPushRef(repoPath, head)
	}

	// Prefer the remote-tracking ref: the local branch of the same name is
	// often absent in a CI checkout, which fetches only the pushed ref.
	for _, ref := range []string{
		"refs/remotes/origin/" + defaultBranch,
		defaultBranch,
	} {
		if !commitPresent(repoPath, ref) {
			continue
		}
		mergeBase, err := git.GetMergeBase(repoPath, ref, head)
		if err != nil || mergeBase == "" {
			continue
		}
		if mergeBase == head {
			// head is already contained in the default branch, so the push
			// carried no commits of its own. The commit it points at was
			// reviewed when it landed on the default branch, and reviewing it
			// again would report on code this push did not touch.
			return "", errNoChangesToReview
		}
		return mergeBase + ".." + head, nil
	}

	return "", fmt.Errorf(
		"no base commit available (first push?) and the default branch %q "+
			"could not be resolved in this clone, so the pushed range "+
			"cannot be determined (shallow clone? increase GIT_DEPTH or "+
			"fetch the default branch) — or pass --ref BASE..HEAD explicitly",
		defaultBranch)
}

// gitLabDefaultBranchFirstPushRef scopes the initial push of the default
// branch itself. A single commit is the whole push, so it is reviewed as one
// ref. Anything more has no base to anchor a range on — the root commit has no
// parent — so the run stops and asks for --ref, the same honest trade the
// other unscopable cases make: reviewing the tip alone would post a verdict
// covering one commit of the push and imply the rest passed.
func gitLabDefaultBranchFirstPushRef(repoPath, head string) (string, error) {
	if repoPath == "" {
		repoPath = "."
	}
	count, err := git.CommitCount(repoPath, head)
	if err != nil {
		return "", fmt.Errorf(
			"initial push of the default branch: could not count its "+
				"commits: %w — pass --ref BASE..HEAD explicitly", err)
	}
	if count == 1 {
		// A count of one is only trustworthy when head is a real root
		// commit. A GIT_DEPTH=1 checkout grafts away the parent, so
		// rev-list reports one commit for a multi-commit push too; the raw
		// commit object still names the parent and tells the cases apart.
		root, err := git.IsRootCommit(repoPath, head)
		if err != nil {
			return "", fmt.Errorf(
				"initial push of the default branch: could not inspect "+
					"%s: %w — pass --ref BASE..HEAD explicitly", head, err)
		}
		if !root {
			return "", fmt.Errorf(
				"initial push of the default branch: only one commit is "+
					"present in this clone but %s has a parent, so history "+
					"was cut (shallow clone? increase GIT_DEPTH or fetch "+
					"more history) — or pass --ref BASE..HEAD explicitly",
				head)
		}
		return head, nil
	}
	return "", fmt.Errorf(
		"initial push of the default branch carries %d commits and no CI "+
			"variable names a base, so the pushed range cannot be reviewed "+
			"as one diff — pass --ref BASE..HEAD explicitly (for everything "+
			"after the root commit: --ref \"$(git rev-list --max-parents=0 "+
			"HEAD)..HEAD\")", count)
}

// gitLabMergeBase resolves the common ancestor of a fallback base and the head
// commit. CI_MERGE_REQUEST_DIFF_BASE_SHA is already the merge base GitLab
// computed for the merge request diff, but the fallbacks are plain tips:
// CI_COMMIT_BEFORE_SHA is the branch tip before the push, which is no longer an
// ancestor after a force push or rebase, and CI_MERGE_REQUEST_TARGET_BRANCH_SHA
// is the target branch tip, which may have advanced since the branches
// diverged. Diffing a tip directly makes commits that only exist on the other
// side show up as changes and produces findings unrelated to the review.
//
// In practice CI_COMMIT_BEFORE_SHA is the fallback that runs, and a branch
// pipeline is where it happens: GitLab sets CI_MERGE_REQUEST_DIFF_BASE_SHA in
// every merge request pipeline, while CI_MERGE_REQUEST_TARGET_BRANCH_SHA appears
// only in merged-results and merge-train pipelines — a strict subset. So the
// target-branch entry can only ever win if GitLab populates it without the diff
// base, which it does not do today; it is kept as defense in depth against that
// changing, not because any pipeline reaches it.
//
// A failed resolution (shallow clone, pruned objects) falls back to the tip with
// a warning. The caller then checks that the tip is actually present, since in a
// shallow clone it usually is not.
func gitLabMergeBase(repoPath, base, head, baseKey string) string {
	if repoPath == "" {
		repoPath = "."
	}
	mergeBase, err := git.GetMergeBase(repoPath, base, head)
	if err != nil || mergeBase == "" {
		log.Printf(
			"ci review: could not resolve merge base of %s (%s) "+
				"and %s: %v — comparing tips directly, which "+
				"may surface unrelated changes",
			base, baseKey, head, err)
		return base
	}
	return mergeBase
}

// detectMRIID auto-detects the merge request IID from the GitLab CI
// environment.
func detectMRIID() (int, error) {
	raw := strings.TrimSpace(os.Getenv("CI_MERGE_REQUEST_IID"))
	if raw == "" {
		return 0, fmt.Errorf(
			"CI_MERGE_REQUEST_IID not set " +
				"(not a merge request pipeline?)")
	}
	iid, err := strconv.Atoi(raw)
	if err != nil || iid <= 0 {
		return 0, fmt.Errorf(
			"invalid CI_MERGE_REQUEST_IID %q", raw)
	}
	return iid, nil
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
	client, err := ciGitHubClient(ctx)
	if err != nil {
		return err
	}
	if upsert {
		return client.UpsertPRComment(ctx, ghRepo, prNumber, body)
	}
	return client.CreatePRComment(ctx, ghRepo, prNumber, body)
}

// checkForgeAuth resolves the forge client up front so a missing or
// unreadable token fails before any review work is spent. It builds the
// same client the posting path builds, so a token that passes here is the
// one that will be used later.
func checkForgeAuth(ctx context.Context, forge ciForge, opts ciReviewOpts) error {
	var err error
	switch forge {
	case ciForgeGitLab:
		_, err = ciGitLabClient(ctx, opts.glHost)
	default:
		_, err = ciGitHubClient(ctx)
	}
	if err != nil {
		return fmt.Errorf("--comment requires forge credentials: %w", err)
	}
	return nil
}

func ciGitHubClient(ctx context.Context) (*ghpkg.Client, error) {
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
	// Use the caller's context so the `gh auth token` shell-out is
	// cancelable when the CI job is interrupted or times out.
	token := ghpkg.ResolveAuthToken(ctx, ghpkg.EnvironmentToken(), host)
	if token == "" {
		return nil, fmt.Errorf("GitHub authentication required: set GH_TOKEN or GITHUB_TOKEN, or authenticate with gh auth login")
	}
	return ghpkg.NewClient(token, ghpkg.WithBaseURL(apiBaseURL))
}

func ciGitLabClient(ctx context.Context, glHost string) (*glpkg.Client, error) {
	// Resolve the API base URL from --gl-host first. The flag comes from the
	// job script, which a pipeline starter cannot rewrite, so a pinned value
	// holds even when the pipeline was started with CI_SERVER_URL overridden
	// as a custom variable (predefined variables lose that precedence fight).
	// Without the flag, fall back to CI_SERVER_URL (set inside GitLab CI) or
	// GITLAB_HOST/GL_HOST so self-hosted tokens reach the configured instance
	// instead of gitlab.com.
	apiBaseURL, err := glpkg.GitLabAPIBaseURL(glHost)
	if err != nil {
		return nil, err
	}
	// Derive hostname from the resolved API base URL so the
	// glab auth token fallback targets the same host as the client.
	host := glpkg.HostnameFromAPIBaseURL(apiBaseURL)
	// Use the caller's context so the `glab auth token` shell-out is
	// cancelable when the CI job is interrupted or times out.
	token := glpkg.ResolveAuthToken(ctx, glpkg.EnvironmentToken(), host)
	if token == "" {
		return nil, fmt.Errorf("GitLab authentication required: set GITLAB_TOKEN or GL_TOKEN, or authenticate with glab auth login")
	}
	clientOpts := []glpkg.ClientOption{glpkg.WithBaseURL(apiBaseURL)}
	if strings.TrimSpace(glHost) != "" {
		// The operator pinned the origin in the job script, so honor that
		// against every environment-controlled redirect, proxies included.
		clientOpts = append(clientOpts, glpkg.WithPinnedOrigin())
	}
	return glpkg.NewClient(token, clientOpts...)
}
