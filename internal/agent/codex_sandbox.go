package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"go.kenn.io/roborev/internal/sandbox"
)

// CodexSandbox connects an isolated job to the host credential proxy.
type CodexSandbox struct {
	Job      sandbox.Job
	ProxyKey string
}

// WithSandbox returns a copy configured to run inside the job's nono sandbox.
func (a *CodexAgent) WithSandbox(s *CodexSandbox) *CodexAgent {
	clone := a.clone()
	clone.sandbox = s
	return clone
}

func (s *CodexSandbox) prepare(repoPath string) (sandbox.Job, func(), error) {
	job := s.Job
	workDir, err := filepath.EvalSymlinks(job.WorkDir)
	if err != nil {
		return job, nil, fmt.Errorf("resolve sandbox work dir: %w", err)
	}
	repo, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		return job, nil, fmt.Errorf("resolve sandbox repo: %w", err)
	}
	rel, err := filepath.Rel(workDir, repo)
	if err != nil || (rel != "." && !filepath.IsLocal(rel)) {
		return job, nil, fmt.Errorf("sandbox repo must be inside job work dir")
	}
	home, err := os.MkdirTemp(job.WorkDir, ".codex-home-")
	if err != nil {
		return job, nil, fmt.Errorf("create sandbox Codex home: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(home) }
	file, err := os.OpenFile(filepath.Join(home, "config.toml"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		cleanup()
		return job, nil, err
	}
	config := map[string]any{
		"model_provider": "roborev-proxy",
		"web_search":     "disabled",
		"model_providers": map[string]any{"roborev-proxy": map[string]any{
			"name":                      "roborev-proxy",
			"base_url":                  fmt.Sprintf("http://127.0.0.1:%d/v1", job.ProxyPort),
			"experimental_bearer_token": s.ProxyKey,
			"requires_openai_auth":      false,
			"wire_api":                  "responses",
		}},
	}
	if err := toml.NewEncoder(file).Encode(config); err != nil {
		_ = file.Close()
		cleanup()
		return job, nil, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return job, nil, err
	}
	job.Env = append(append([]string{}, job.Env...), "CODEX_HOME="+home)
	return job, cleanup, nil
}
