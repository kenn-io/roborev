package agent

import (
	"context"
	"slices"
	"strings"
)

type untrustedCheckoutContextKey struct{}

// WithUntrustedCheckout marks agent work whose working directory is a checkout
// written by someone else, such as the head of a pull request under review.
//
// Agent CLIs read project configuration from their working directory, and
// some of it runs commands: Claude Code runs hooks from .claude/settings.json
// and starts the stdio MCP servers in .mcp.json, even under -p. A pull request
// author who adds those files would run code in the review process, which
// holds the provider credentials. Adapters that see this mark load only user
// configuration. WithCIReview sets it as well.
func WithUntrustedCheckout(ctx context.Context) context.Context {
	return context.WithValue(ctx, untrustedCheckoutContextKey{}, true)
}

func untrustedCheckout(ctx context.Context) bool {
	untrusted, _ := ctx.Value(untrustedCheckoutContextKey{}).(bool)
	return untrusted
}

// untrustedCheckoutEnvKeys are removed from an agent's environment when it
// runs in an untrusted checkout, because each one tells an agent CLI to load
// configuration from that checkout:
//
//   - COPILOT_ALLOW_ALL trusts the working directory, which loads its hooks,
//     MCP servers, plugins and skills. The GITHUB_COPILOT_PROMPT_MODE_*
//     variables load repository hooks, workspace MCP servers and extensions in
//     prompt mode, which skips them by default.
//   - GEMINI_CLI_TRUST_WORKSPACE=true trusts the workspace, which loads hooks,
//     MCP servers and tool discovery commands from .gemini/settings.json.
//
// The variables in untrustedCheckoutEnvValues are removed and then set again.
var untrustedCheckoutEnvKeys = []string{
	"COPILOT_ALLOW_ALL",
	"GITHUB_COPILOT_PROMPT_MODE_REPO_HOOKS",
	"GITHUB_COPILOT_PROMPT_MODE_WORKSPACE_MCP",
	"GITHUB_COPILOT_PROMPT_MODE_EXTENSIONS",
}

// untrustedCheckoutEnvValues are set for every agent in an untrusted
// checkout. Each is read by one CLI and ignored by the others.
//
//   - GEMINI_CLI_TRUST_WORKSPACE=false marks the workspace untrusted, so Gemini
//     skips its settings, hooks and MCP servers. When the user settings enable
//     folder trust, headless Gemini exits instead of running untrusted.
//   - KILO_DISABLE_PROJECT_CONFIG=1 skips project kilo.json, opencode.json and
//     the .kilo and .kilocode directories, which hold plugins that Kilo runs
//     at startup and MCP servers.
//   - OPENCODE_DISABLE_PROJECT_CONFIG=1 skips project MCP servers and custom
//     tools. OpenCode still imports project plugins; see the CI docs.
//   - GROK_FOLDER_TRUST=1 turns Grok's folder trust on even when the user
//     config turns it off. Grok then skips an untrusted folder's hooks, MCP
//     servers and plugins.
var untrustedCheckoutEnvValues = []string{
	"GEMINI_CLI_TRUST_WORKSPACE=false",
	"GROK_FOLDER_TRUST=1",
	"KILO_DISABLE_PROJECT_CONFIG=1",
	"OPENCODE_DISABLE_PROJECT_CONFIG=1",
}

// untrustedCheckoutEnv returns env adjusted for an agent that runs in an
// untrusted checkout. Other environments are returned unchanged.
func untrustedCheckoutEnv(ctx context.Context, env []string) []string {
	if !untrustedCheckout(ctx) {
		return env
	}
	keys := slices.Clone(untrustedCheckoutEnvKeys)
	for _, entry := range untrustedCheckoutEnvValues {
		key, _, _ := strings.Cut(entry, "=")
		keys = append(keys, key)
	}
	return append(filterEnv(env, keys...), untrustedCheckoutEnvValues...)
}
