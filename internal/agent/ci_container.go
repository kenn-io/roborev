package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// isolateCICommand runs generated-workflow agents in disposable containers.
// The runner prepares prompts and schemas before launch; agents can read those
// files and the checkout, but cannot alter them or inspect the runner's home.
// Provider keys remain available to the provider CLI. Publishing credentials
// and Git authentication are deliberately not part of this environment.
func isolateCICommand(cmd *exec.Cmd) {
	image := os.Getenv("ROBOREV_CI_AGENT_IMAGE")
	if image == "" {
		return
	}
	repo := os.Getenv("ROBOREV_CI_REPO")
	prepared := os.Getenv("TMPDIR")
	for name, path := range map[string]string{"ROBOREV_CI_REPO": repo, "TMPDIR": prepared} {
		if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) || strings.Contains(path, ",") {
			cmd.Err = fmt.Errorf("CI isolation requires an absolute, non-root %s without commas", name)
			return
		}
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		cmd.Err = fmt.Errorf("CI agent isolation requires docker: %w", err)
		return
	}
	dir := cmd.Dir
	if dir == "" {
		dir = repo
	}
	args := []string{
		"docker", "run", "--rm", "--init", "-i",
		"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--tmpfs", "/tmp:rw,exec,mode=1777",
		"--tmpfs", "/home/agent:rw,mode=1777",
		"--mount", "type=bind,src=" + repo + ",dst=" + repo + ",readonly",
		"--mount", "type=bind,src=" + prepared + ",dst=" + prepared + ",readonly",
		"--workdir", dir,
		"--env", "HOME=/home/agent",
		"--env", "GIT_CONFIG_NOSYSTEM=1",
		"--env", "GIT_CONFIG_GLOBAL=/dev/null",
		"--env", "GIT_CONFIG_COUNT=3",
		"--env", "GIT_CONFIG_KEY_0=safe.directory",
		"--env", "GIT_CONFIG_VALUE_0=" + repo,
		"--env", "GIT_CONFIG_KEY_1=credential.helper",
		"--env", "GIT_CONFIG_VALUE_1=",
		"--env", "GIT_CONFIG_KEY_2=core.hooksPath",
		"--env", "GIT_CONFIG_VALUE_2=/dev/null",
		"--env", "GIT_TERMINAL_PROMPT=0",
		"--env", "GIT_OPTIONAL_LOCKS=0",
	}
	// Use an allowlist, not a list of known runner secrets to remove. Never
	// pass GitHub tokens, even for adapters that normally retain them.
	for _, entry := range cmd.Env {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GOOGLE_API_KEY", "GEMINI_API_KEY", "XAI_API_KEY":
			args = append(args, "--env", entry)
		}
	}
	args = append(args, image, filepath.Base(cmd.Path))
	args = append(args, cmd.Args[1:]...)
	// The Docker client also gets no publishing or provider secrets in its
	// environment. Preserve only the client connection and executable lookup.
	cmd.Env = nil
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "PATH" || key == "HOME" || strings.HasPrefix(key, "DOCKER_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Path = docker
	cmd.Args = args
}
