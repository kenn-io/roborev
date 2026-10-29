package agent

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/sandbox"
)

// CodexSandbox connects an isolated job to the nono credential route.
type CodexSandbox struct {
	Job      sandbox.Job
	AuthFile string
	upstream string
}

// WithSandbox returns a copy configured to run inside the job's nono sandbox.
func (a *CodexAgent) WithSandbox(s *CodexSandbox) *CodexAgent {
	clone := a.clone()
	clone.sandbox = s
	return clone
}

func (s *CodexSandbox) prepare(repoPath string) (sandbox.Job, func(), error) {
	job := s.Job
	workDir, err := filepath.Abs(job.WorkDir)
	if err == nil {
		workDir, err = filepath.EvalSymlinks(workDir)
	}
	if err != nil {
		return job, nil, fmt.Errorf("resolve sandbox work dir: %w", err)
	}
	repo, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		return job, nil, fmt.Errorf("resolve sandbox repo: %w", err)
	}
	rel, err := filepath.Rel(workDir, repo)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return job, nil, fmt.Errorf("sandbox repo must be inside job work dir")
	}
	job.WorkDir = workDir
	authFile := s.AuthFile
	if authFile == "" {
		hostHome, err := config.CodexHomeDir()
		if err != nil {
			return job, nil, err
		}
		authFile = filepath.Join(hostHome, "auth.json")
	}
	authFile, err = filepath.Abs(authFile)
	if err != nil {
		return job, nil, err
	}
	data, err := os.ReadFile(authFile)
	if err != nil {
		return job, nil, fmt.Errorf("read Codex login: %w", err)
	}
	var login struct {
		Tokens struct {
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &login); err != nil {
		return job, nil, fmt.Errorf("parse Codex login: %w", err)
	}
	if login.Tokens.AccountID == "" {
		return job, nil, fmt.Errorf("Codex login is missing tokens.account_id")
	}
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
	file, err := os.OpenFile(filepath.Join(home, "config.toml"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		cleanup()
		return job, nil, err
	}
	provider := map[string]any{
		"model_providers": map[string]any{"roborev-route": map[string]any{
			"name": "roborev-route", "env_key": "CODEX_PHANTOM",
			"requires_openai_auth": false, "wire_api": "responses",
			"http_headers": map[string]string{"chatgpt-account-id": login.Tokens.AccountID},
		}},
	}
	if err := toml.NewEncoder(file).Encode(provider); err != nil {
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
