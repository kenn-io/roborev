package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/sandbox"
)

// CodexSandbox connects an isolated job to the nono credential route.
type CodexSandbox struct {
	Job      sandbox.Job
	Upstream string
}

// WithSandbox returns a copy configured to run inside the job's nono sandbox.
func (a *CodexAgent) WithSandbox(s *CodexSandbox) *CodexAgent {
	clone := a.clone()
	clone.sandbox = s
	clone.omitThreadSource = true
	return clone
}

// schemaDir places the review schema where a sandboxed Codex can read it.
func (a *CodexAgent) schemaDir() string {
	if a.sandbox == nil {
		return ""
	}
	return a.sandbox.Job.WorkDir
}

func (s *CodexSandbox) prepare(repoPath string) (sandbox.Job, func(), error) {
	job := s.Job
	workDir, err := sandbox.ResolvePath(job.WorkDir)
	if err != nil {
		return job, nil, fmt.Errorf("resolve sandbox work dir: %w", err)
	}
	repo, err := sandbox.ResolvePath(repoPath)
	if err != nil {
		return job, nil, fmt.Errorf("resolve sandbox repo: %w", err)
	}
	rel, err := filepath.Rel(workDir, repo)
	if err != nil || !filepath.IsLocal(rel) {
		return job, nil, fmt.Errorf("sandbox repo must be inside job work dir")
	}
	job.WorkDir = workDir
	hostHome, err := config.CodexHomeDir()
	if err != nil {
		return job, nil, err
	}
	hostHome, err = sandbox.ResolvePath(hostHome)
	if err != nil {
		return job, nil, fmt.Errorf("resolve Codex login path: %w", err)
	}
	job.DenyReadPaths = append(append([]string{}, job.DenyReadPaths...), filepath.Join(hostHome, "auth.json"))
	upstream := s.Upstream
	if upstream == "" {
		upstream = "https://chatgpt.com/backend-api/codex"
	}
	job.Route = sandbox.Route{
		Name: "codex", Upstream: upstream, Path: "/responses", EnvVar: "CODEX_PHANTOM",
		Capture: []string{"/usr/bin/python3", "-I", "-c", `import json,sys; print(json.load(open(sys.argv[1]))["tokens"]["access_token"])`, filepath.Join(hostHome, "auth.json")},
	}
	home, err := os.MkdirTemp(job.WorkDir, ".codex-home-")
	if err != nil {
		return job, nil, fmt.Errorf("create sandbox Codex home: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(home) }
	const provider = `[model_providers.roborev-route]
name = "roborev-route"
env_key = "CODEX_PHANTOM"
requires_openai_auth = false
wire_api = "responses"
`
	err = os.WriteFile(filepath.Join(home, "config.toml"), []byte(provider), 0o600)
	if err != nil {
		cleanup()
		return job, nil, err
	}
	job.Env = append(append([]string{}, job.Env...), "CODEX_HOME="+home)
	return job, cleanup, nil
}
