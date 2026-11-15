package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

const doctorFlow = `digraph roborev_sample {
 node [shape=box];
 start [shape=oval label="Review"];
 back [shape=oval label="Back to roborev_triage"];
 start -> back;
}`

func TestConfigGetLocalAgentHookReviewFlow(t *testing.T) {
	cfg := "[agent_hook]\nreview_flow = '''\n" + doctorFlow + "\n'''\n"
	env := setupConfigEnv(t, "", cfg)
	got, err := getValueForScope(env.Resolver, "agent_hook.review_flow", scopeLocal)
	require.NoError(t, err)
	assert.Equal(t, doctorFlow, strings.TrimSpace(got))
	_, err = validateKeyForScope("agent_hook.review_flow", doctorFlow, false)
	require.NoError(t, err)
}

func TestDoctorAgentHookFlow(t *testing.T) {
	for _, mode := range []string{"none", "committed", "differs", "crlf equivalent", "fallback", "custom agent", "custom droid", "custom agent env", "custom droid env", "legacy", "absent on default"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(agenthook.InstructionEnv, "")
			t.Setenv(agenthook.DroidInstructionEnv, "")
			repo := testutil.NewTestRepo(t)
			if mode != "none" && mode != "fallback" && mode != "absent on default" {
				flow := doctorFlow
				if mode == "crlf equivalent" {
					flow = strings.ReplaceAll(flow, "\n", "\r\n")
				}
				repo.CommitFile(".roborev.toml", "[agent_hook]\nreview_flow = '''\n"+flow+"\n'''\n", "configure")
			}
			if mode == "none" || mode == "fallback" {
				repo.CommitFile("README.md", "Sample\n", "initial")
			}
			if mode == "absent on default" {
				repo.CommitFile(".roborev.toml", "review_guidelines = 'Test'\n", "configure")
			}
			repo.Run("remote", "add", "origin", repo.Root)
			repo.Run("fetch", "origin")
			repo.Run("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
			if mode == "differs" || mode == "crlf equivalent" || mode == "fallback" || mode == "absent on default" {
				workingFlow := strings.Replace(doctorFlow, "roborev_sample", "roborev_other", 1)
				if mode == "crlf equivalent" {
					workingFlow = doctorFlow
				}
				require.NoError(t, os.WriteFile(filepath.Join(repo.Root, ".roborev.toml"), []byte("[agent_hook]\nreview_flow = '''\n"+workingFlow+"\n'''\n"), 0o600))
			}
			cfg, err := config.LoadRepoConfig(repo.Root)
			require.NoError(t, err)
			global := config.DefaultConfig()
			if mode == "custom agent" {
				global.AgentHook.Instruction = "Ask first."
			}
			if mode == "custom droid" {
				global.DroidHook.Instruction = "Ask first."
			}
			if mode == "custom agent env" {
				t.Setenv(agenthook.InstructionEnv, "Ask first.")
			}
			if mode == "custom droid env" {
				t.Setenv(agenthook.DroidInstructionEnv, "Ask first.")
			}
			if mode == "legacy" {
				global.AgentHook.Instruction = "Invoke the $roborev-fix skill now."
			}
			env := &doctorEnv{ctx: t.Context(), repoPath: repo.Root, repoBranch: "origin/main", repoCfg: cfg, global: global}
			checks := checkDoctorAgentHookFlow(env)
			if mode == "none" {
				assert.Empty(t, checks)
				return
			}
			require.Len(t, checks, 1)
			got := checks[0]
			switch mode {
			case "differs":
				assert.Equal(t, doctorWarn, got.Status)
				assert.Contains(t, got.Summary, "working tree")
			case "crlf equivalent":
				assert.Equal(t, doctorOK, got.Status)
			case "absent on default":
				assert.Equal(t, doctorWarn, got.Status)
				assert.Contains(t, got.Summary, "no repository flow")
			case "custom agent", "custom droid", "custom agent env", "custom droid env":
				assert.Equal(t, doctorWarn, got.Status)
				assert.Contains(t, got.Summary, "custom Agent Hook instruction")
				if mode == "custom agent env" {
					assert.Contains(t, got.Details, agenthook.InstructionEnv)
				}
				if mode == "custom droid env" {
					assert.Contains(t, got.Details, agenthook.DroidInstructionEnv)
				}
			default:
				assert.Equal(t, doctorOK, got.Status)
				assert.Contains(t, got.Summary, "bytes, from")
			}
		})
	}
}

func TestDoctorAgentHookFlowReportsInvalidTrustedConfigDespiteShadowRef(t *testing.T) {
	repo := testutil.NewTestRepo(t)
	repo.CommitFile(".roborev.toml", "[agent_hook]\nreview_flow = 'not dot'\n", "invalid flow")
	repo.Run("remote", "add", "origin", repo.Root)
	repo.Run("fetch", "origin")
	repo.Run("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	repo.Run("checkout", "-b", "origin/main")
	repo.CommitFile(".roborev.toml", "[agent_hook]\nreview_flow = '''\n"+doctorFlow+"\n'''\n", "shadow")
	cfg, err := config.LoadRepoConfig(repo.Root)
	require.NoError(t, err)
	checks := runDoctorChecks(&doctorEnv{
		ctx: t.Context(), repoPath: repo.Root, repoBranch: "origin/main", repoCfg: cfg,
		global: config.DefaultConfig(), daemon: fakeDoctorDaemon{}, pingErr: errors.New("connection refused"),
	})
	hookFlow := findDoctorCheck(t, checks, "repo.agent_hook_flow")
	assert.Equal(t, doctorFail, hookFlow.Status)
	assert.Contains(t, strings.Join(hookFlow.Details, " "), "agent_hook.review_flow")
}

func TestDoctorReportsInvalidDefaultFlowOnce(t *testing.T) {
	repo := testutil.NewTestRepo(t)
	repo.CommitFile(".roborev.toml", "[agent_hook]\nreview_flow = 'not dot'\n", "invalid default flow")
	defaultBranch := strings.TrimSpace(repo.Run("branch", "--show-current"))
	repo.Run("remote", "add", "origin", repo.Root)
	repo.Run("fetch", "origin")
	repo.Run("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+defaultBranch)
	repo.Run("checkout", "-b", "feature")
	repo.CommitFile(".roborev.toml", "[agent_hook]\nreview_flow = '''\n"+doctorFlow+"\n'''\n", "repair working copy")

	cfg, err := config.LoadRepoConfig(repo.Root)
	require.NoError(t, err)
	env := &doctorEnv{
		ctx: t.Context(), repoPath: repo.Root, repoCfg: cfg, repoBranch: "origin/" + defaultBranch,
		global: config.DefaultConfig(), daemon: fakeDoctorDaemon{}, pingErr: errors.New("connection refused"),
	}
	checks := runDoctorChecks(env)

	defaultBranchConfig := findDoctorCheck(t, checks, "config.repo_default_branch")
	assert.Equal(t, doctorFail, defaultBranchConfig.Status)
	assert.Contains(t, strings.Join(defaultBranchConfig.Details, " "), "agent_hook.review_flow")
	assert.Equal(t, -1, slices.IndexFunc(checks, func(c doctorCheck) bool { return c.ID == "repo.agent_hook_flow" }))
}
