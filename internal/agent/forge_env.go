package agent

import (
	"log"
	"strings"
)

// Forge (GitHub/GitLab) API credentials must not reach agent subprocesses.
// Review and synthesis agents read merge request and pull request content
// written by whoever opened it, so their input is untrusted. An agent that
// follows an injected instruction can copy any value it can read into the
// review text, and that text is posted back as a PR/MR comment — so a token
// sitting in the inherited environment is one prompt away from being
// published, even for agents launched without shell access.
//
// roborev resolves these credentials in its own process (see
// cmd/roborev/ci.go and internal/{github,gitlab}), so filtering the child
// environment does not affect comment posting or any other API call.
//
// The strip happens at the choke points in process.go — configureSubprocess
// for the run itself and configureCapabilityProbe for the `--help` probes that
// precede it — so a newly added agent cannot forget to do it. Both also drop
// the runtime preload variables below, since a preloaded hook runs inside the
// agent binary and would otherwise read whatever that process can.

// gitlabForgeCredentialEnvKeys lists the GitLab credentials roborev reads to
// post merge request notes. No agent CLI authenticates with any of them, so
// they are stripped unconditionally.
//
// CI_JOB_TOKEN is included even though roborev never uses it (it cannot
// create notes): GitLab injects it into every job, it grants API access for
// the job's lifetime, and an agent has no legitimate use for it.
//
// The CI_* entries after it carry the same job credential under another name,
// so stripping only CI_JOB_TOKEN would leave the token readable:
//
//   - CI_REGISTRY_PASSWORD is the job token, issued for the container registry.
//   - CI_REPOSITORY_URL embeds it in a clone URL
//     (https://gitlab-ci-token:<token>@host/group/project.git).
//   - CI_DEPLOY_PASSWORD and CI_DEPENDENCY_PROXY_PASSWORD are separate deploy
//     and dependency-proxy credentials GitLab injects the same way.
//   - CI_JOB_JWT and its V1/V2 spellings are deprecated but still live ID
//     tokens for whatever accepts them.
//   - CI_BUILD_TOKEN and CI_BUILD_REPO are the pre-9.0 names GitLab still
//     injects for backwards compatibility, carrying the same job token and the
//     same credentialed clone URL as CI_JOB_TOKEN and CI_REPOSITORY_URL. An
//     agent can read the token from either spelling, so both have to go.
//
// Non-secret identity stays available: CI_SERVER_URL, CI_PROJECT_URL,
// CI_PROJECT_PATH, and CI_REGISTRY_USER name the same things without a
// credential in them.
var gitlabForgeCredentialEnvKeys = []string{
	"GITLAB_TOKEN",
	"GL_TOKEN",
	"CI_JOB_TOKEN",
	"CI_REGISTRY_PASSWORD",
	"CI_REPOSITORY_URL",
	"CI_DEPLOY_PASSWORD",
	"CI_DEPENDENCY_PROXY_PASSWORD",
	"CI_JOB_JWT",
	"CI_JOB_JWT_V1",
	"CI_JOB_JWT_V2",
	"CI_BUILD_TOKEN",
	"CI_BUILD_REPO",
}

// githubForgeCredentialEnvKeys lists the GitHub credentials roborev reads to
// post pull request comments and commit statuses.
//
// These are stripped by default, but two agent CLIs authenticate with a
// GitHub token and cannot run without it: copilot and kiro-cli (see
// ghaction.AgentEnvVar, which maps both to GITHUB_TOKEN, and the generated
// workflow's hardcoded GH_TOKEN line). Those two launch sites opt out via
// withGitHubCredentials(); every other agent gets them removed.
var githubForgeCredentialEnvKeys = []string{
	"GH_TOKEN",
	"GITHUB_TOKEN",
}

// Deliberately NOT stripped:
//
//   - GOOGLE_APPLICATION_CREDENTIALS. This is a model-provider credential,
//     not a forge credential: Claude Code routed at Vertex AI (and the Gemini
//     CLI) authenticates with the service-account file it points at. Removing
//     it would break the model call outright, so the agent could not review
//     anything at all. Provider credentials are the one class of secret an
//     agent must be able to read.
//   - ANTHROPIC_API_KEY and the other ANTHROPIC_*/CLAUDE_* routing vars.
//     Already handled by claudeStripKeys in claude.go, which strips the
//     inherited values and re-injects only the key roborev was configured
//     with, so roborev owns the routing decision.
//   - GITHUB_API_URL, GH_HOST, CI_SERVER_URL, GITLAB_HOST, GL_HOST. Endpoint
//     hostnames, not secrets; an agent learning them leaks nothing.

// preloadEnvKeys lists environment variables that make a process run code
// chosen by whoever set them, before the program's own entry point.
//
// NODE_OPTIONS=--require=<file> preloads that file into every Node CLI, which
// is what the claude-code and gemini agents are; LD_PRELOAD and LD_AUDIT do the
// same for native binaries on Linux, and DYLD_INSERT_LIBRARIES on macOS. They
// are ordinary variables, so in CI whoever starts the pipeline can set them —
// the same capability that could otherwise redirect the API origin — and point
// them at a file in the tree under review.
//
// Stripping them is not about secrecy but about who chooses the code: roborev
// spawns these processes, so their code has to come from the agent binary, not
// from the job environment. A job that legitimately needs NODE_OPTIONS (memory
// tuning, say) must set it for its own steps, not for roborev's children.
var preloadEnvKeys = []string{
	"NODE_OPTIONS",
	"LD_PRELOAD",
	"LD_AUDIT",
	"DYLD_INSERT_LIBRARIES",
}

// StripUntrustedEnv returns a copy of env without the values roborev refuses
// to hand a child process: forge credentials and runtime preload hooks.
func StripUntrustedEnv(env []string) []string {
	return stripUntrustedEnv(env, false)
}

func stripUntrustedEnv(env []string, keepGitHub bool) []string {
	return filterEnv(stripForgeCredentials(env, keepGitHub), preloadEnvKeys...)
}

// ForgeCredentialEnvKeys returns the forge credential environment variable
// names roborev keeps out of agent subprocesses.
func ForgeCredentialEnvKeys() []string {
	keys := make(
		[]string,
		0,
		len(gitlabForgeCredentialEnvKeys)+len(githubForgeCredentialEnvKeys),
	)
	keys = append(keys, gitlabForgeCredentialEnvKeys...)
	keys = append(keys, githubForgeCredentialEnvKeys...)
	return keys
}

// StripForgeCredentials returns a copy of env with every forge credential
// removed. Use it wherever an agent subprocess environment is built outside
// configureSubprocess.
func StripForgeCredentials(env []string) []string {
	return stripForgeCredentials(env, false)
}

// StripUntrustedEnvLogged behaves like StripUntrustedEnv and, when something
// was actually present, logs which variables it removed.
//
// ACP-launched agents have no equivalent of the withGitHubCredentials() opt-out
// that copilot and kiro-cli use: the ACP transport hands the agent an arbitrary
// command line, so keeping a token readable there would be the most direct
// exfiltration channel available. An ACP agent configured to authenticate with
// GH_TOKEN/GITHUB_TOKEN therefore fails with an opaque provider auth error, and
// nothing in that error points at the strip. The log line names the removed
// variables so the cause is diagnosable from the job output. site identifies the
// launch path for the log message.
func StripUntrustedEnvLogged(env []string, site string) []string {
	return logRemovedUntrustedEnv(env, StripUntrustedEnv(env), site)
}

// logRemovedUntrustedEnv reports which entries the strip removed, by name
// only, and returns stripped unchanged. Silent when nothing was removed, so the
// common case of no forge credentials in the environment stays quiet.
func logRemovedUntrustedEnv(before, stripped []string, site string) []string {
	if removed := removedEnvKeys(before, stripped); len(removed) > 0 {
		log.Printf(
			"%s: removed untrusted variables from agent environment: %s "+
				"(see internal/agent/forge_env.go)",
			site, strings.Join(removed, ", "))
	}
	return stripped
}

// removedEnvKeys returns the names of the entries present in before but not in
// after. Only names are reported — never values.
func removedEnvKeys(before, after []string) []string {
	kept := make(map[string]struct{}, len(after))
	for _, e := range after {
		kept[e] = struct{}{}
	}
	var removed []string
	for _, e := range before {
		if _, ok := kept[e]; ok {
			continue
		}
		key, _, _ := strings.Cut(e, "=")
		removed = append(removed, key)
	}
	return removed
}

// stripForgeCredentials removes forge credentials from env. When
// keepGitHub is true the GitHub tokens survive, for agent CLIs that
// authenticate with them.
func stripForgeCredentials(env []string, keepGitHub bool) []string {
	keys := ForgeCredentialEnvKeys()
	if keepGitHub {
		keys = gitlabForgeCredentialEnvKeys
	}
	return filterEnv(env, keys...)
}
