package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForgeCredentialEnvKeys(t *testing.T) {
	assert := assert.New(t)

	keys := ForgeCredentialEnvKeys()
	for _, want := range []string{
		"GITLAB_TOKEN", "GL_TOKEN", "CI_JOB_TOKEN",
		// Aliases that carry the job token or another injected credential;
		// stripping only CI_JOB_TOKEN would leave it readable.
		"CI_REGISTRY_PASSWORD", "CI_REPOSITORY_URL",
		"CI_DEPLOY_PASSWORD", "CI_DEPENDENCY_PROXY_PASSWORD",
		"CI_JOB_JWT", "CI_JOB_JWT_V1", "CI_JOB_JWT_V2",
		// Deprecated pre-9.0 names GitLab still injects.
		"CI_BUILD_TOKEN", "CI_BUILD_REPO",
		"GH_TOKEN", "GITHUB_TOKEN",
	} {
		assert.Contains(keys, want, "forge credential %s must be stripped", want)
	}
	// Non-secret CI identity must survive: agents need to know which project
	// and instance they are reviewing.
	for _, keep := range []string{
		"CI_SERVER_URL", "CI_PROJECT_URL", "CI_PROJECT_PATH", "CI_REGISTRY_USER",
	} {
		assert.NotContains(keys, keep, "%s is not a credential", keep)
	}
	// Provider credentials are not forge credentials: stripping them
	// would break the model call the agent needs to review anything.
	for _, keep := range []string{
		"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_API_KEY",
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY",
	} {
		assert.NotContains(keys, keep, "%s must not be stripped as a forge credential", keep)
	}
}

func TestStripForgeCredentials(t *testing.T) {
	tests := []struct {
		name       string
		env        []string
		keepGitHub bool
		expected   []string
	}{
		{
			name:     "StripsGitLabTokens",
			env:      []string{"PATH=/bin", "GITLAB_TOKEN=glpat-x", "GL_TOKEN=glpat-y"},
			expected: []string{"PATH=/bin"},
		},
		{
			name:     "StripsGitLabJobToken",
			env:      []string{"CI_JOB_TOKEN=job", "CI_SERVER_URL=https://gitlab.example.com"},
			expected: []string{"CI_SERVER_URL=https://gitlab.example.com"},
		},
		{
			name: "StripsGitLabJobTokenAliases",
			env: []string{
				"CI_REGISTRY_PASSWORD=job",
				"CI_REPOSITORY_URL=https://gitlab-ci-token:job@gitlab.example.com/g/p.git",
				"CI_DEPLOY_PASSWORD=deploy",
				"CI_DEPENDENCY_PROXY_PASSWORD=proxy",
				"CI_JOB_JWT=jwt",
				"CI_JOB_JWT_V1=jwt1",
				"CI_JOB_JWT_V2=jwt2",
				"CI_BUILD_TOKEN=job",
				"CI_BUILD_REPO=https://gitlab-ci-token:job@gitlab.example.com/g/p.git",
				"CI_PROJECT_PATH=g/p",
				"CI_REGISTRY_USER=gitlab-ci-token",
			},
			expected: []string{
				"CI_PROJECT_PATH=g/p",
				"CI_REGISTRY_USER=gitlab-ci-token",
			},
		},
		{
			name:       "KeepGitHubStillStripsGitLabAliases",
			env:        []string{"GH_TOKEN=ghp-x", "CI_REGISTRY_PASSWORD=job", "CI_REPOSITORY_URL=https://gitlab-ci-token:job@h/g/p.git"},
			keepGitHub: true,
			expected:   []string{"GH_TOKEN=ghp-x"},
		},
		{
			name:     "StripsGitHubTokens",
			env:      []string{"HOME=/home/u", "GH_TOKEN=ghp-x", "GITHUB_TOKEN=ghp-y"},
			expected: []string{"HOME=/home/u"},
		},
		{
			name: "KeepsProviderCredentials",
			env: []string{
				"GOOGLE_APPLICATION_CREDENTIALS=/run/sa.json",
				"ANTHROPIC_API_KEY=sk-ant",
				"OPENAI_API_KEY=sk-oai",
				"GOOGLE_API_KEY=goog",
				"GITLAB_TOKEN=glpat-x",
			},
			expected: []string{
				"GOOGLE_APPLICATION_CREDENTIALS=/run/sa.json",
				"ANTHROPIC_API_KEY=sk-ant",
				"OPENAI_API_KEY=sk-oai",
				"GOOGLE_API_KEY=goog",
			},
		},
		{
			name: "KeepsForgeEndpointsWhichAreNotSecrets",
			env: []string{
				"GITHUB_API_URL=https://gh.example.com/api/v3",
				"GITLAB_HOST=https://gitlab.example.com",
				"GH_HOST=gh.example.com",
				"GL_HOST=gitlab.example.com",
			},
			expected: []string{
				"GITHUB_API_URL=https://gh.example.com/api/v3",
				"GITLAB_HOST=https://gitlab.example.com",
				"GH_HOST=gh.example.com",
				"GL_HOST=gitlab.example.com",
			},
		},
		{
			name: "MatchesWholeKeysOnly",
			env: []string{
				"MY_GITLAB_TOKEN=1", "GITLAB_TOKEN_OLD=2", "GITLAB_TOKEN=3",
			},
			expected: []string{"MY_GITLAB_TOKEN=1", "GITLAB_TOKEN_OLD=2"},
		},
		{
			name:       "KeepGitHubRetainsGitHubTokensOnly",
			env:        []string{"GH_TOKEN=ghp-x", "GITHUB_TOKEN=ghp-y", "GITLAB_TOKEN=glpat-x", "CI_JOB_TOKEN=job"},
			keepGitHub: true,
			expected:   []string{"GH_TOKEN=ghp-x", "GITHUB_TOKEN=ghp-y"},
		},
		{
			name:     "EmptyEnv",
			env:      nil,
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripForgeCredentials(tt.env, tt.keepGitHub)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestStripForgeCredentialsStripsGitHubByDefault(t *testing.T) {
	got := StripForgeCredentials([]string{"GH_TOKEN=ghp-x", "PATH=/bin"})
	assert.Equal(t, []string{"PATH=/bin"}, got)
}

// forgeCredentialTestEnv sets every forge credential plus a provider
// credential in the parent process so child environments can be inspected.
// forgeCredentialValues gives every stripped key a distinct sentinel value. It
// is derived from ForgeCredentialEnvKeys rather than hand-listed, so a key added
// to the strip list is automatically set in the test environment — otherwise an
// absent-key assertion would compare against a value nothing ever set and pass
// no matter what the strip does.
func forgeCredentialValues() map[string]string {
	values := make(map[string]string)
	for i, key := range ForgeCredentialEnvKeys() {
		values[key] = fmt.Sprintf("secret-%s-%d", strings.ToLower(key), i)
	}
	return values
}

func forgeCredentialTestEnv(t *testing.T) {
	t.Helper()
	for key, value := range forgeCredentialValues() {
		t.Setenv(key, value)
	}
	// A provider credential, kept on purpose (see forge_env.go).
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/run/secrets/sa.json")
}

// TestConfigureSubprocessStripsForgeCredentials checks that every key on the
// strip list really is removed at the choke point. It derives its expectations
// from ForgeCredentialEnvKeys, so it cannot catch a key being deleted from that
// list — the literal expectations in TestForgeCredentialEnvKeys cover that half.
// Together: one test pins what belongs on the list, this one pins that the list
// is enforced.
func TestConfigureSubprocessStripsForgeCredentials(t *testing.T) {
	forgeCredentialTestEnv(t)

	tests := []struct {
		name        string
		keepGitHub  bool
		absent      []string
		presentKeys []string
		present     []string
	}{
		{
			name:   "DefaultStripsEveryForgeCredential",
			absent: ForgeCredentialEnvKeys(),
			present: []string{
				"GOOGLE_APPLICATION_CREDENTIALS=/run/secrets/sa.json",
				"GIT_OPTIONAL_LOCKS=0",
			},
		},
		{
			name:       "GitHubAuthAgentsKeepGitHubTokens",
			keepGitHub: true,
			absent:     gitlabForgeCredentialEnvKeys,
			presentKeys: []string{
				"GH_TOKEN", "GITHUB_TOKEN",
			},
			present: []string{
				"GOOGLE_APPLICATION_CREDENTIALS=/run/secrets/sa.json",
				"GIT_OPTIONAL_LOCKS=0",
			},
		},
	}

	values := forgeCredentialValues()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)

			cmd := exec.CommandContext(context.Background(), "does-not-run")
			var opts []subprocessOption
			if tt.keepGitHub {
				opts = append(opts, withGitHubCredentials())
			}
			configureSubprocess(cmd, opts...)

			require.NotEmpty(t, cmd.Env, "configureSubprocess must populate cmd.Env")
			require.NotEmpty(t, tt.absent)
			for _, key := range tt.absent {
				value, ok := values[key]
				// A key with no sentinel would make the assertion below
				// compare against something nothing ever set, so it would
				// pass regardless of what the strip did.
				require.True(t, ok, "%s has no sentinel value in the test env", key)
				assert.NotContains(cmd.Env, key+"="+value,
					"%s must not reach the agent subprocess", key)
			}
			for _, key := range tt.presentKeys {
				assert.Contains(cmd.Env, key+"="+values[key],
					"%s must survive for a GitHub-authenticating agent", key)
			}
			for _, entry := range tt.present {
				assert.Contains(cmd.Env, entry)
			}
		})
	}
}

// echoForgeCredentialsScript prints the forge credentials it can see so a
// real agent invocation can be inspected end to end.
const echoForgeCredentialsScript = `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
echo "> gitlab=[$GITLAB_TOKEN] gl=[$GL_TOKEN] cijob=[$CI_JOB_TOKEN] gh=[$GH_TOKEN] github=[$GITHUB_TOKEN] gac=[$GOOGLE_APPLICATION_CREDENTIALS]"
`

func TestAgentSubprocessForgeCredentialVisibility(t *testing.T) {
	skipIfWindows(t)
	forgeCredentialTestEnv(t)

	cmdPath := writeTempCommand(t, echoForgeCredentialsScript)

	values := forgeCredentialValues()
	// echoedKeys maps the script's label for each credential to its env var, so
	// a "gh=[<sentinel>]" expectation is built from the same value the
	// environment was seeded with rather than a literal that can drift.
	echoedKeys := map[string]string{
		"gitlab": "GITLAB_TOKEN",
		"gl":     "GL_TOKEN",
		"cijob":  "CI_JOB_TOKEN",
		"gh":     "GH_TOKEN",
		"github": "GITHUB_TOKEN",
	}

	tests := []struct {
		name        string
		agent       Agent
		absentKeys  []string
		presentEcho []string
		present     []string
	}{
		{
			// droid stands in for every agent that has no forge auth.
			name:  "DroidSeesNoForgeCredential",
			agent: NewDroidAgent(cmdPath),
			absentKeys: []string{
				"GITLAB_TOKEN", "GL_TOKEN", "CI_JOB_TOKEN",
				"GH_TOKEN", "GITHUB_TOKEN",
			},
			present: []string{"gac=[/run/secrets/sa.json]"},
		},
		{
			// kiro-cli authenticates with a GitHub token.
			name:        "KiroKeepsGitHubTokens",
			agent:       NewKiroAgent(cmdPath),
			absentKeys:  []string{"GITLAB_TOKEN", "GL_TOKEN", "CI_JOB_TOKEN"},
			presentEcho: []string{"gh", "github"},
			present:     []string{"gac=[/run/secrets/sa.json]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)

			result, err := tt.agent.Review(
				context.Background(), t.TempDir(), "deadbeef", "review this", nil)
			require.NoError(t, err)

			for _, key := range tt.absentKeys {
				value, ok := values[key]
				require.True(t, ok, "%s has no sentinel value", key)
				assert.NotContains(result, value,
					"agent subprocess must not see %s", key)
			}
			for _, label := range tt.presentEcho {
				key := echoedKeys[label]
				require.NotEmpty(t, key, "unknown echo label %q", label)
				assert.Contains(result, label+"=["+values[key]+"]")
			}

			for _, want := range tt.present {
				assert.Contains(result, want)
			}
		})
	}
}

// TestStripForgeCredentialsCaseInsensitive covers the Windows environment
// semantics: a credential set under a different case is still readable by
// roborev via os.Getenv, so it must not survive into the agent environment.
func TestStripForgeCredentialsCaseInsensitive(t *testing.T) {
	tests := []struct {
		name            string
		caseInsensitive bool
		env             []string
		absent          []string
		present         []string
	}{
		{
			name:            "windows strips mixed case forge credentials",
			caseInsensitive: true,
			env: []string{
				"GitLab_Token=glpat-secret",
				"gl_token=also-secret",
				"Github_Token=ghp-secret",
				"PATH=/usr/bin",
			},
			absent: []string{
				"GitLab_Token=glpat-secret",
				"gl_token=also-secret",
				"Github_Token=ghp-secret",
			},
			present: []string{"PATH=/usr/bin"},
		},
		{
			name:            "windows still strips the canonical spelling",
			caseInsensitive: true,
			env:             []string{"GITLAB_TOKEN=glpat-secret", "HOME=/root"},
			absent:          []string{"GITLAB_TOKEN=glpat-secret"},
			present:         []string{"HOME=/root"},
		},
		{
			name:            "posix treats a different case as a different variable",
			caseInsensitive: false,
			env:             []string{"GitLab_Token=not-the-same-var", "GITLAB_TOKEN=secret"},
			absent:          []string{"GITLAB_TOKEN=secret"},
			present:         []string{"GitLab_Token=not-the-same-var"},
		},
		{
			name:            "provider credentials survive in both modes",
			caseInsensitive: true,
			env: []string{
				"GOOGLE_APPLICATION_CREDENTIALS=/tmp/sa.json",
				"GITLAB_TOKEN=glpat-secret",
			},
			absent:  []string{"GITLAB_TOKEN=glpat-secret"},
			present: []string{"GOOGLE_APPLICATION_CREDENTIALS=/tmp/sa.json"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)

			original := envKeysAreCaseInsensitive
			envKeysAreCaseInsensitive = tt.caseInsensitive
			t.Cleanup(func() { envKeysAreCaseInsensitive = original })

			got := StripForgeCredentials(tt.env)

			for _, absent := range tt.absent {
				assert.NotContains(got, absent)
			}
			for _, present := range tt.present {
				assert.Contains(got, present)
			}
		})
	}
}

// Capability probes (`claude --help` and friends) run the agent binary before
// any review starts. They went through configureCapabilityProbe, which never
// touched cmd.Env, so the probe inherited the forge tokens the review
// subprocess is careful to drop.
func TestConfigureCapabilityProbeStripsForgeCredentials(t *testing.T) {
	forgeCredentialTestEnv(t)
	assert := assert.New(t)

	cmd := exec.CommandContext(context.Background(), "does-not-run")
	configureCapabilityProbe(cmd)

	require.NotEmpty(t, cmd.Env, "configureCapabilityProbe must populate cmd.Env")
	for _, key := range ForgeCredentialEnvKeys() {
		assert.NotContains(envKeys(cmd.Env), key,
			"probe must not inherit forge credential %s", key)
	}
	assert.Contains(cmd.Env, "GOOGLE_APPLICATION_CREDENTIALS=/run/secrets/sa.json",
		"provider credentials must survive so the probe still runs")
}

// A runtime preload variable turns any child process into arbitrary code
// execution: NODE_OPTIONS=--require=<file> runs that file inside every Node
// CLI, and LD_PRELOAD/DYLD_INSERT_LIBRARIES do the same for native binaries.
// These are ordinary environment variables, so a GitLab pipeline starter can
// set them as pipeline variables and point them at a file in the tree under
// review.
func TestPreloadEnvKeysAreStripped(t *testing.T) {
	assert := assert.New(t)

	preload := []string{
		"NODE_OPTIONS=--require=/tmp/evil.js",
		"LD_PRELOAD=/tmp/evil.so",
		"LD_AUDIT=/tmp/evil.so",
		"DYLD_INSERT_LIBRARIES=/tmp/evil.dylib",
	}
	env := append([]string{"PATH=/usr/bin"}, preload...)

	stripped := StripUntrustedEnv(env)
	assert.Contains(stripped, "PATH=/usr/bin", "unrelated variables must survive")
	for _, entry := range preload {
		assert.NotContains(stripped, entry,
			"preload variable %s must be stripped", entry)
	}
}

// Both spawn paths have to drop preload variables: the probe runs the agent
// binary first, and the review subprocess runs it with untrusted content.
func TestSpawnPathsStripPreloadEnv(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*exec.Cmd)
	}{
		{"Subprocess", func(cmd *exec.Cmd) { configureSubprocess(cmd) }},
		{"CapabilityProbe", configureCapabilityProbe},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NODE_OPTIONS", "--require=/tmp/evil.js")
			t.Setenv("LD_PRELOAD", "/tmp/evil.so")

			cmd := exec.CommandContext(context.Background(), "does-not-run")
			tt.configure(cmd)

			require.NotEmpty(t, cmd.Env)
			keys := envKeys(cmd.Env)
			assert.NotContains(t, keys, "NODE_OPTIONS")
			assert.NotContains(t, keys, "LD_PRELOAD")
		})
	}
}

// envKeys returns just the names from a KEY=VALUE environment slice.
func envKeys(env []string) []string {
	keys := make([]string, 0, len(env))
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		keys = append(keys, key)
	}
	return keys
}
