package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kit/pathresolve"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/sandbox"
)

// CodexSandbox connects an isolated job to the nono credential route.
type CodexSandbox struct {
	Job      sandbox.Job
	upstream string
}

// WithSandbox returns a copy configured to run inside the job's nono sandbox.
func (a *CodexAgent) WithSandbox(s *CodexSandbox) *CodexAgent {
	clone := a.clone()
	clone.sandbox = s
	return clone
}

// schemaDir places the review schema where a sandboxed Codex can read it.
func (a *CodexAgent) schemaDir() string {
	if a.sandbox == nil {
		return ""
	}
	dir, err := filepath.Abs(a.sandbox.Job.WorkDir)
	if err != nil {
		return a.sandbox.Job.WorkDir
	}
	return dir
}

func (s *CodexSandbox) prepare(repoPath string) (sandbox.Job, func(), error) {
	job := s.Job
	workDir, err := filepath.Abs(job.WorkDir)
	if err == nil {
		workDir, err = pathresolve.EvalSymlinks(workDir)
	}
	if err != nil {
		return job, nil, fmt.Errorf("resolve sandbox work dir: %w", err)
	}
	repo, err := filepath.Abs(repoPath)
	if err == nil {
		repo, err = pathresolve.EvalSymlinks(repo)
	}
	if err != nil {
		return job, nil, fmt.Errorf("resolve sandbox repo: %w", err)
	}
	rel, err := filepath.Rel(workDir, repo)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return job, nil, fmt.Errorf("sandbox repo must be inside job work dir")
	}
	job.WorkDir = workDir
	hostHome, err := config.CodexHomeDir()
	if err != nil {
		return job, nil, err
	}
	authFile, err := filepath.Abs(filepath.Join(hostHome, "auth.json"))
	if err != nil {
		return job, nil, fmt.Errorf("resolve Codex login path: %w", err)
	}
	job.DenyReadPaths = append(append([]string{}, job.DenyReadPaths...), authFile)
	upstream := s.upstream
	if upstream == "" {
		upstream = "https://chatgpt.com/backend-api/codex"
	}
	job.Route = sandbox.Route{
		Name: "codex", Upstream: upstream, Path: "/responses", EnvVar: "CODEX_PHANTOM",
		Capture: []string{"/usr/bin/python3", "-I", "-c", `import json,sys; print(json.load(open(sys.argv[1]))["tokens"]["access_token"])`, authFile},
	}
	home, err := os.MkdirTemp(job.WorkDir, ".codex-home-")
	if err != nil {
		return job, nil, fmt.Errorf("create sandbox Codex home: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(home) }
	provider := map[string]any{
		"model_providers": map[string]any{"roborev-route": map[string]any{
			"name": "roborev-route", "env_key": "CODEX_PHANTOM",
			"requires_openai_auth": false, "wire_api": "responses",
		}},
	}
	data, err := toml.Marshal(provider)
	if err == nil {
		err = os.WriteFile(filepath.Join(home, "config.toml"), data, 0o600)
	}
	if err != nil {
		cleanup()
		return job, nil, err
	}
	job.Env = append(append([]string{}, job.Env...), "CODEX_HOME="+home)
	return job, cleanup, nil
}
