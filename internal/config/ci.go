// CI poller configuration and review-matrix resolution.

package config

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// GitHubAppConfig holds GitHub App authentication settings.
// Extracted from CIConfig for cohesion; embedded so TOML keys remain flat under [ci].
type GitHubAppConfig struct {
	GitHubAppID             int64  `toml:"github_app_id"`
	GitHubAppPrivateKey     string `toml:"github_app_private_key" sensitive:"true"` // PEM file path or inline; supports ${ENV_VAR}
	GitHubAppInstallationID int64  `toml:"github_app_installation_id"`

	// Multi-installation: map of owner → installation_id
	GitHubAppInstallations map[string]int64 `toml:"github_app_installations"`
}

// GitHubAppConfigured returns true if GitHub App authentication can be used.
// Requires app ID, private key, and at least one installation ID (singular or map).
func (c *GitHubAppConfig) GitHubAppConfigured() bool {
	return c.GitHubAppID != 0 && c.GitHubAppPrivateKey != "" &&
		(c.GitHubAppInstallationID != 0 || len(c.GitHubAppInstallations) > 0)
}

// InstallationIDForOwner returns the installation ID for a GitHub owner.
// Checks the normalized installations map first (skipping non-positive values),
// then falls back to the singular field. Owner comparison is case-insensitive.
func (c *GitHubAppConfig) InstallationIDForOwner(owner string) int64 {
	if id, ok := c.GitHubAppInstallations[strings.ToLower(owner)]; ok && id > 0 {
		return id
	}
	return c.GitHubAppInstallationID
}

// NormalizeInstallations lowercases all keys in GitHubAppInstallations
// so lookups are case-insensitive via direct map access.
// Returns an error if two keys collide after lowercasing (e.g., "wesm" and "Wesm").
func (c *GitHubAppConfig) NormalizeInstallations() error {
	if len(c.GitHubAppInstallations) == 0 {
		return nil
	}
	normalized := make(map[string]int64, len(c.GitHubAppInstallations))
	for k, v := range c.GitHubAppInstallations {
		lower := strings.ToLower(k)
		if _, exists := normalized[lower]; exists {
			return fmt.Errorf("case-colliding github_app_installations keys for %q", lower)
		}
		normalized[lower] = v
	}
	c.GitHubAppInstallations = normalized
	return nil
}

// GitHubAppPrivateKeyResolved expands env vars in the private key value,
// reads the file if it's a path, and returns the PEM content.
func (c *GitHubAppConfig) GitHubAppPrivateKeyResolved() (string, error) {
	val := os.ExpandEnv(c.GitHubAppPrivateKey)
	if val == "" {
		return "", fmt.Errorf("github_app_private_key is empty after expansion")
	}

	// If it looks like PEM content, return directly
	// TrimSpace handles leading whitespace/newlines in inline PEM content
	trimmed := strings.TrimSpace(val)
	if strings.HasPrefix(trimmed, "-----BEGIN") {
		return trimmed, nil
	}

	// Expand leading ~ to home directory
	if strings.HasPrefix(val, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home for github_app_private_key: %w", err)
		}
		val = home + val[1:]
	}

	// Otherwise treat as file path
	data, err := os.ReadFile(val)
	if err != nil {
		return "", fmt.Errorf("read private key file %s: %w", val, err)
	}
	return string(data), nil
}

// AgentReviewType pairs an agent name with a review type for the review matrix.
type AgentReviewType struct {
	Agent      string
	ReviewType string
}

// CIConfig holds configuration for the CI poller that watches GitHub PRs
type CIConfig struct {
	// Enabled enables the CI poller
	Enabled bool `toml:"enabled"`

	// PollInterval is how often to poll for PRs (e.g., "5m", "10m"). Default: 5m
	PollInterval string `toml:"poll_interval"`

	// Repos is the list of GitHub repos to poll in "owner/repo" format.
	// Supports glob patterns (e.g., "myorg/*", "myorg/api-*") using path.Match syntax.
	// The owner part must be literal — no wildcards before the "/".
	Repos []string `toml:"repos"`

	// ExcludeRepos is a list of glob patterns to exclude from the resolved repo list.
	// Applies to both exact entries and wildcard-expanded entries.
	ExcludeRepos []string `toml:"exclude_repos"`

	// MaxRepos is a safety cap on the total number of expanded repos. Default: 100.
	MaxRepos int `toml:"max_repos"`

	// ReviewTypes is the list of review types to run for each PR (e.g., ["security", "default"]).
	// Defaults to ["security"] if empty.
	ReviewTypes []string `toml:"review_types"`

	// Agents is the list of agents to run for each PR (e.g., ["codex", "gemini"]).
	// Defaults to auto-detection if empty.
	Agents []string `toml:"agents"`

	// Reviews maps agent names to review type lists. When set, replaces
	// the ReviewTypes x Agents cross-product with a granular matrix.
	// Example: {"codex": ["security", "review"], "gemini": ["review"]}
	Reviews map[string][]string `toml:"reviews"`

	// ThrottleInterval is the minimum time between reviews of the same PR.
	// If a PR was reviewed within this interval, new pushes are deferred.
	// Default: "1h". Set to "0" to disable throttling.
	ThrottleInterval string `toml:"throttle_interval"`

	// ThrottleBypassUsers is a list of GitHub usernames whose PRs
	// bypass the throttle interval and are always reviewed immediately.
	ThrottleBypassUsers []string `toml:"throttle_bypass_users"`

	// Model overrides the model for CI reviews (empty = use workflow resolution)
	Model string `toml:"model"`

	// SynthesisAgent is the agent used to synthesize multiple review outputs into one comment.
	// Defaults to the first available agent.
	SynthesisAgent string `toml:"synthesis_agent"`

	// SynthesisBackupAgent is tried when the primary synthesis
	// agent fails. Empty means no backup — failures fall through
	// to raw formatting.
	SynthesisBackupAgent string `toml:"synthesis_backup_agent"`

	// SynthesisModel overrides the model used for synthesis.
	SynthesisModel string `toml:"synthesis_model"`

	// MinSeverity filters out findings below this severity level during synthesis.
	// Valid values: critical, high, medium, low. Empty means no filter (include all).
	MinSeverity string `toml:"min_severity"`

	// Panel names a [review.panels.X] panel to run for CI reviews. When set,
	// the CI poller resolves the panel (members + synthesis) from the config
	// loaded off the PR's default branch instead of the agents x review_types
	// matrix. Empty means use the matrix.
	Panel string `toml:"panel"`

	// UpsertComments enables updating existing PR comments instead of
	// creating new ones. When true, roborev searches for its marker
	// comment and patches it. Default: false (create a new comment each run).
	UpsertComments bool `toml:"upsert_comments"`

	// IncludeCosts includes token cost estimates in CI PR comment footers.
	// Default: false (omit costs from GitHub comments).
	IncludeCosts bool `toml:"include_costs"`

	// BatchTimeout is how long to wait for all batch jobs to complete before
	// posting results with available reviews. Jobs still running after this
	// timeout are canceled. Default: "15m". Set to "0" to disable.
	BatchTimeout string `toml:"batch_timeout"`

	// GitHub App authentication (optional — comments appear as bot instead of personal account)
	GitHubAppConfig
}

// ResolvedReviewTypes returns the list of review types to use.
// Defaults to ["security"] if empty.
func (c *CIConfig) ResolvedReviewTypes() []string {
	if len(c.ReviewTypes) > 0 {
		return c.ReviewTypes
	}
	return []string{ReviewTypeSecurity}
}

// ResolvedAgents returns the list of agents to use.
// Defaults to [""] (empty = auto-detect) if empty.
func (c *CIConfig) ResolvedAgents() []string {
	if len(c.Agents) > 0 {
		return c.Agents
	}
	return []string{""}
}

// ResolvedReviewMatrix returns (agent, reviewType) pairs.
// If Reviews is set, uses it directly. Otherwise falls back to
// the cross-product of ResolvedAgents() x ResolvedReviewTypes().
func (c *CIConfig) ResolvedReviewMatrix() []AgentReviewType {
	if len(c.Reviews) > 0 {
		return reviewsMapToMatrix(c.Reviews)
	}
	agents := c.ResolvedAgents()
	reviewTypes := c.ResolvedReviewTypes()
	matrix := make(
		[]AgentReviewType, 0, len(agents)*len(reviewTypes),
	)
	for _, rt := range reviewTypes {
		for _, ag := range agents {
			matrix = append(matrix, AgentReviewType{
				Agent:      ag,
				ReviewType: rt,
			})
		}
	}
	return matrix
}

// ResolvedReviewMatrixForRepo returns the review matrix for a RepoCIConfig.
// If Reviews is set, uses it directly. Otherwise falls back to
// the cross-product of Agents x ReviewTypes (which may be empty,
// meaning "use global").
func (c *RepoCIConfig) ResolvedReviewMatrix() []AgentReviewType {
	if c.Reviews != nil {
		// Reviews map is configured — return the resolved matrix
		// even when empty (signals "disable reviews for this repo").
		m := reviewsMapToMatrix(c.Reviews)
		if m == nil {
			return []AgentReviewType{}
		}
		return m
	}
	return nil
}

// reviewsMapToMatrix converts a Reviews map to a sorted slice of
// AgentReviewType pairs. Agents are sorted alphabetically; review
// types preserve their declared order within each agent.
func reviewsMapToMatrix(
	reviews map[string][]string,
) []AgentReviewType {
	agents := make([]string, 0, len(reviews))
	for agent := range reviews {
		agents = append(agents, agent)
	}
	slices.Sort(agents)

	var matrix []AgentReviewType
	for _, agent := range agents {
		for _, rt := range reviews[agent] {
			matrix = append(matrix, AgentReviewType{
				Agent:      agent,
				ReviewType: rt,
			})
		}
	}
	return matrix
}

// ResolvedThrottleInterval returns the minimum time between reviews
// of the same PR. Defaults to 1h if empty or unparseable.
// Returns 0 (disabled) if explicitly set to "0".
func (c *CIConfig) ResolvedThrottleInterval() time.Duration {
	if c.ThrottleInterval == "" {
		return time.Hour
	}
	if c.ThrottleInterval == "0" {
		return 0
	}
	d, err := time.ParseDuration(c.ThrottleInterval)
	if err != nil || d < 0 {
		return time.Hour
	}
	return d
}

// ResolvedBatchTimeout returns how long to wait for all batch jobs
// before posting early with available results. Default: 15 minutes.
// Returns 0 (disabled) if explicitly set to "0".
func (c *CIConfig) ResolvedBatchTimeout() time.Duration {
	const defaultTimeout = 15 * time.Minute
	if c.BatchTimeout == "" {
		return defaultTimeout
	}
	if c.BatchTimeout == "0" {
		return 0
	}
	d, err := time.ParseDuration(c.BatchTimeout)
	if err != nil || d < 0 {
		return defaultTimeout
	}
	return d
}

// IsThrottleBypassed reports whether the given GitHub login is in
// the ThrottleBypassUsers list. Comparison is case-insensitive.
func (c *CIConfig) IsThrottleBypassed(login string) bool {
	lower := strings.ToLower(login)
	for _, u := range c.ThrottleBypassUsers {
		if strings.ToLower(u) == lower {
			return true
		}
	}
	return false
}

// ResolvedMaxRepos returns the maximum number of repos to poll.
// Defaults to 100 if not set or non-positive.
func (c *CIConfig) ResolvedMaxRepos() int {
	if c.MaxRepos > 0 {
		return c.MaxRepos
	}
	return 100
}

// RepoCIConfig holds per-repo CI overrides (used by the CI poller for this repo).
// These override the global [ci] settings when reviewing this specific repo.
type RepoCIConfig struct {
	// Agents overrides the list of agents for CI reviews of this repo.
	Agents []string `toml:"agents" comment:"Override the agents used by CI for this repo."`

	// ReviewTypes overrides the list of review types for CI reviews of this repo.
	ReviewTypes []string `toml:"review_types" comment:"Override the review types used by CI for this repo."`

	// Reviews maps agent names to review type lists. When set, replaces
	// the ReviewTypes x Agents cross-product for this repo.
	Reviews map[string][]string `toml:"reviews" comment:"Explicit CI review matrix for this repo: agent name to review types."`

	// Panel names a [review.panels.X] panel to run for CI reviews of this repo,
	// overriding the agents x review_types matrix.
	Panel string `toml:"panel" comment:"Named [review.panels.X] panel for CI."`

	// Reasoning overrides the reasoning level for CI reviews.
	Reasoning string `toml:"reasoning" comment:"Override the CI reasoning level for this repo: fast, standard, medium, thorough, or maximum."`

	// MinSeverity overrides the minimum severity filter for CI synthesis.
	MinSeverity string `toml:"min_severity" comment:"Override the minimum CI severity included in synthesized output."`

	// UpsertComments overrides the global ci.upsert_comments setting.
	// Use a pointer so we can distinguish "not set" from "explicitly false".
	UpsertComments *bool `toml:"upsert_comments" comment:"Override whether CI updates an existing PR comment instead of creating a new one."`

	// IncludeCosts overrides the global ci.include_costs setting.
	// Use a pointer so we can distinguish "not set" from "explicitly false".
	IncludeCosts *bool `toml:"include_costs" comment:"Override whether CI PR comments include token cost estimates."`
}

// ResolveCIAgents determines which agents to use for CI review execution.
// Priority: explicit CSV flag > repo [ci].agents > global [ci].agents > [""].
func ResolveCIAgents(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) []string {
	if explicit != "" {
		return splitTrimmedCSV(explicit)
	}
	var repoAgents []string
	if repoCfg != nil {
		repoAgents = repoCfg.CI.Agents
	}
	var globalAgents []string
	if globalCfg != nil {
		globalAgents = globalCfg.CI.Agents
	}
	return resolveSlice([]string{""}, repoAgents, globalAgents)
}

// ResolveCIReviewTypes determines which review types to use for CI review execution.
// Priority: explicit CSV flag > repo [ci].review_types > global [ci].review_types > ["security"].
func ResolveCIReviewTypes(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) []string {
	if explicit != "" {
		return splitTrimmedCSV(explicit)
	}
	var repoTypes []string
	if repoCfg != nil {
		repoTypes = repoCfg.CI.ReviewTypes
	}
	var globalTypes []string
	if globalCfg != nil {
		globalTypes = globalCfg.CI.ReviewTypes
	}
	return resolveSlice([]string{ReviewTypeSecurity}, repoTypes, globalTypes)
}

// ResolveCIReasoning determines the reasoning level for CI review execution.
// Priority: explicit > repo [ci].reasoning > "thorough".
func ResolveCIReasoning(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) (string, error) {
	var repoVal string
	if repoCfg != nil {
		repoVal = repoCfg.CI.Reasoning
	}
	_ = globalCfg
	return resolveNormalized("thorough", NormalizeReasoning, explicit, repoVal)
}

// ResolveCIMinSeverity determines the synthesis severity filter for CI review execution.
// Priority: explicit > repo [ci].min_severity > global [ci].min_severity > "".
func ResolveCIMinSeverity(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) (string, error) {
	var repoVal string
	if repoCfg != nil {
		repoVal = repoCfg.CI.MinSeverity
	}
	var globalVal string
	if globalCfg != nil {
		globalVal = globalCfg.CI.MinSeverity
	}
	return resolveNormalized("", NormalizeMinSeverity, explicit, repoVal, globalVal)
}

// ResolveCISynthesisAgent determines the synthesis agent for CI review execution.
// Priority: explicit > global [ci].synthesis_agent > "".
func ResolveCISynthesisAgent(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) string {
	var globalVal string
	if globalCfg != nil {
		globalVal = strings.TrimSpace(globalCfg.CI.SynthesisAgent)
	}
	_ = repoCfg
	return resolve("", strings.TrimSpace(explicit), globalVal)
}

// ResolveCIUpsertComments determines whether CI should update an existing PR comment.
// Priority: repo [ci].upsert_comments > global [ci].upsert_comments > false.
func ResolveCIUpsertComments(
	repoCfg *RepoConfig,
	globalCfg *Config,
) bool {
	var repoVal *bool
	if repoCfg != nil {
		repoVal = repoCfg.CI.UpsertComments
	}
	var globalVal *bool
	if globalCfg != nil {
		globalVal = &globalCfg.CI.UpsertComments
	}
	return resolveBool(false, repoVal, globalVal)
}

// ResolveCIIncludeCosts determines whether CI PR comments should include costs.
// Priority: repo [ci].include_costs > global [ci].include_costs > false.
func ResolveCIIncludeCosts(
	repoCfg *RepoConfig,
	globalCfg *Config,
) bool {
	var repoVal *bool
	if repoCfg != nil {
		repoVal = repoCfg.CI.IncludeCosts
	}
	var globalVal *bool
	if globalCfg != nil {
		globalVal = &globalCfg.CI.IncludeCosts
	}
	return resolveBool(false, repoVal, globalVal)
}

// ResolveCIWorkflowAgents determines which agents to encode into a generated CI workflow.
// Priority: explicit CSV flag > repo [ci].agents > repo agent > global [ci].agents > global default_agent > ["codex"].
func ResolveCIWorkflowAgents(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) []string {
	if explicit != "" {
		return splitTrimmedCSV(explicit)
	}
	var repoCIAgents []string
	var repoAgent []string
	if repoCfg != nil {
		repoCIAgents = repoCfg.CI.Agents
		repoAgent = singleTrimmedValue(repoCfg.Agent)
	}
	var globalCIAgents []string
	var globalAgent []string
	if globalCfg != nil {
		globalCIAgents = globalCfg.CI.Agents
		globalAgent = singleTrimmedValue(globalCfg.DefaultAgent)
	}
	return resolveSlice(
		[]string{"codex"},
		repoCIAgents,
		repoAgent,
		globalCIAgents,
		globalAgent,
	)
}
