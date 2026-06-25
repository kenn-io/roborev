package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/skills"
)

type checkStatus string

const (
	statusOK      checkStatus = "ok"
	statusMissing checkStatus = "missing"
	statusUnknown checkStatus = "unknown"
)

type quickstartCheck struct {
	ID         string      `json:"id"`
	Status     checkStatus `json:"status"`
	Details    string      `json:"details,omitempty"`
	FixCommand string      `json:"fix_command,omitempty"`
}

type quickstartState struct {
	InGitRepo     bool              `json:"in_git_repo"`
	DaemonRunning bool              `json:"daemon_running"`
	Checks        []quickstartCheck `json:"checks"`
}

// quickstartCheckIDs is the stable, ordered set of check IDs (JSON contract).
var quickstartCheckIDs = []string{
	"daemon_running",
	"post_commit_hook",
	"repo_registered",
	"repo_config",
	"configured_agent",
	"agent_hook_claude",
	"agent_hook_codex",
	"skills_installed",
}

func detectState(ctx context.Context, repoRoot string, inGitRepo bool) quickstartState {
	daemonUp := daemonReachable()
	global, _ := config.LoadGlobal()
	agent := config.ResolveAgent("", repoRoot, global)

	checks := []quickstartCheck{
		checkDaemon(daemonUp),
		checkPostCommitHook(ctx, repoRoot, inGitRepo),
		checkRepoRegistered(repoRoot, inGitRepo, daemonUp),
		checkRepoConfig(repoRoot, inGitRepo, agent),
		checkConfiguredAgent(repoRoot, inGitRepo, agent),
		checkAgentHook("agent_hook_claude", agenthook.DefaultClaudeSettingsPath(),
			"roborev agent-hook install --agent claude"),
		checkAgentHook("agent_hook_codex", agenthook.DefaultCodexHooksPath(),
			"roborev agent-hook install --agent codex"),
		checkSkills(),
	}

	return quickstartState{InGitRepo: inGitRepo, DaemonRunning: daemonUp, Checks: checks}
}

func daemonReachable() bool {
	_, err := probeDaemonWithRetry(getDaemonEndpoint(), 1*time.Second)
	return err == nil
}

func checkDaemon(up bool) quickstartCheck {
	if up {
		return quickstartCheck{ID: "daemon_running", Status: statusOK, Details: "daemon is running"}
	}
	return quickstartCheck{
		ID: "daemon_running", Status: statusMissing,
		Details: "daemon is not reachable", FixCommand: "roborev daemon start",
	}
}

func checkPostCommitHook(ctx context.Context, repoRoot string, inGitRepo bool) quickstartCheck {
	c := quickstartCheck{ID: "post_commit_hook"}
	if !inGitRepo {
		c.Status = statusUnknown
		return c
	}
	if githook.NotInstalled(ctx, repoRoot, "post-commit") {
		c.Status = statusMissing
		c.Details = "commits are not auto-reviewed"
		c.FixCommand = "roborev install-hook"
		return c
	}
	c.Status = statusOK
	c.Details = "every commit is auto-reviewed"
	return c
}

func checkRepoRegistered(repoRoot string, inGitRepo, daemonUp bool) quickstartCheck {
	c := quickstartCheck{ID: "repo_registered"}
	if !inGitRepo {
		c.Status = statusUnknown
		return c
	}
	if !daemonUp {
		c.Status = statusUnknown
		c.Details = "daemon unreachable; cannot verify registration"
		return c
	}
	tracked, err := repoTracked(repoRoot)
	if err != nil {
		c.Status = statusUnknown
		c.Details = fmt.Sprintf("could not query daemon: %v", err)
		return c
	}
	if tracked {
		c.Status = statusOK
		c.Details = "repo is registered with the daemon"
		return c
	}
	c.Status = statusMissing
	c.Details = "repo is not registered with the daemon"
	c.FixCommand = "roborev init"
	return c
}

func repoTracked(repoRoot string) (bool, error) {
	ep := getDaemonEndpoint()
	resp, err := ep.HTTPClient(5 * time.Second).Get(
		ep.BaseURL() + "/api/repos/resolve?path=" + url.QueryEscape(repoRoot))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("resolve repo: daemon returned %s", resp.Status)
	}
	var body struct {
		Tracked bool `json:"tracked"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, err
	}
	return body.Tracked, nil
}

func checkRepoConfig(repoRoot string, inGitRepo bool, agent string) quickstartCheck {
	c := quickstartCheck{ID: "repo_config"}
	if !inGitRepo {
		c.Status = statusUnknown
		return c
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".roborev.toml")); err == nil {
		c.Status = statusOK
		c.Details = ".roborev.toml present"
		return c
	}
	c.Status = statusMissing
	c.Details = "no per-repo .roborev.toml (using global defaults)"
	c.FixCommand = fmt.Sprintf("roborev init --agent %s", agent)
	return c
}

func checkConfiguredAgent(repoRoot string, inGitRepo bool, agent string) quickstartCheck {
	c := quickstartCheck{ID: "configured_agent"}
	if !inGitRepo {
		c.Status = statusUnknown
		return c
	}
	explicit := false
	if repoCfg, err := config.LoadRepoConfig(repoRoot); err == nil && repoCfg != nil && repoCfg.Agent != "" {
		explicit = true
	}
	if raw, err := config.LoadRawGlobal(); err == nil {
		if v, ok := raw["default_agent"].(string); ok && v != "" {
			explicit = true
		}
	}
	if explicit {
		c.Status = statusOK
		c.Details = fmt.Sprintf("review agent: %s", agent)
		return c
	}
	c.Status = statusMissing
	c.Details = fmt.Sprintf("no agent configured; defaulting to %s", agent)
	c.FixCommand = fmt.Sprintf("roborev config set --local agent %s", agent)
	return c
}

func checkAgentHook(id, path, fix string) quickstartCheck {
	c := quickstartCheck{ID: id}
	installed, err := agenthook.Installed(path)
	if err != nil {
		c.Status = statusUnknown
		c.Details = fmt.Sprintf("could not read %s: %v", path, err)
		return c
	}
	if installed {
		c.Status = statusOK
		c.Details = "agent hook installed"
		return c
	}
	c.Status = statusMissing
	c.Details = "agent hook not installed (no mid-session fix nudges)"
	c.FixCommand = fix
	return c
}

func checkSkills() quickstartCheck {
	c := quickstartCheck{ID: "skills_installed"}
	if skills.IsInstalled(skills.AgentClaude) || skills.IsInstalled(skills.AgentCodex) {
		c.Status = statusOK
		c.Details = "roborev skills installed"
		return c
	}
	c.Status = statusMissing
	c.Details = "roborev skills not installed"
	c.FixCommand = "roborev skills install"
	return c
}
