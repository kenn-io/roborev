package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/sandbox"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

type ciCheckConfig struct {
	Agent     string `json:"agent,omitempty"`
	Model     string `json:"model,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
}

var ciCheckSchema = jsontext.Value(`{"type":"object","additionalProperties":false,"required":["verdicts"],"properties":{"verdicts":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["finding","keep","evidence","reason"],"properties":{"finding":{"type":"integer"},"keep":{"type":"boolean"},"evidence":{"type":"string"},"reason":{"type":"string"}}}}}}`)

const ciCheckPrompt = `An automated reviewer posted the findings in ../findings.md on this pull request. You are its last step before the comment posts: keep each finding only if it holds up. The worktree you're in is the PR head; origin/%s is its base.

Work in a scratch copy (cp -a . ../check-$$). Leave this worktree unchanged.

For each finding:
1. Make it fail. Write a test or command that shows the failure, starting from a state the product, its users, or an outside system can create: a file a user imports, a config value they type, and a server's reply all count. For an order of events, force that order.
2. Keep it if it fails that way and either a user on a normal workflow hits it, or the harm is lost data, a silently saved wrong result, or a security hole. Keep a finding about the project's own tests or CI when a test can't catch the bug it exists for and the fix is small.
3. Drop it if it doesn't fail, if nothing but a hand-built state reaches it, or if only rare timing or unusual input reaches it and the fix would add new machinery. Count everyone the failure reaches: in a library, a rare input in one consumer is still a broken build or wrong result in every repo that uses it. A bug that predates this PR still counts when the PR's code runs into it. Also drop it if the repo's AGENTS.md, CLAUDE.md, or .roborev.toml review guidelines say not to flag it.

If the sandbox stops a finding from running, keep it.
Return a JSON object with verdicts: [{"finding": 1, "keep": true, "evidence": "the command and its output, or what refutes it", "reason": "one sentence"}].
Work alone: no sub-agents or background jobs.`

func (wp *WorkerPool) checkAndCompleteSynthesis(ctx context.Context, workerID string, job *storage.ReviewJob, res synthesisResult) {
	var snapshot synthesisJobConfig
	if err := json.Unmarshal([]byte(job.PanelMemberConfigJSON), &snapshot); err == nil && snapshot.Check != nil {
		doc, err := reviewpkg.DecodeStructuredReview(res.review.StructuredOutput)
		if err == nil && doc.Legacy == nil && !doc.UnableToReview() && len(doc.Findings) > 0 {
			jobLog := newAppendingJobLogWriter(job.ID)
			defer jobLog.Close()
			path := config.GlobalConfigPath()
			if watcher, ok := wp.cfgGetter.(*ConfigWatcher); ok && watcher.configPath != "" {
				path = watcher.configPath
			}
			var env []string
			if wp.ciGitEnv != nil {
				if panel, err := wp.db.GetCIPanelBySynthesisJobID(job.ID); err == nil && panel != nil {
					env = wp.ciGitEnv(panel.GithubRepo)
				}
			}
			checked, checkErr := runCICheck(ctx, job, *snapshot.Check, doc, filepath.Join(filepath.Dir(path), "credential-proxy.key"), env, jobLog)
			if checkErr != nil {
				fmt.Fprintf(jobLog, "CI checker failed; keeping original review: %v\n", checkErr)
			} else {
				raw, encodeErr := json.Marshal(checked)
				if encodeErr == nil {
					res.review.StructuredOutput = raw
					res.review.Output = checked.Markdown(job.MinSeverity)
					res.review.Verdict = reviewpkg.SynthesisVerdict(checked, job.MinSeverity)
				}
			}
		}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		wp.handleUpdateInterruption(ctx, workerID, job)
		return
	}
	wp.completeSynthesisContext(workerID, job, res)
}

// runCICheck owns the disposable checkout and every process used by the checker.
func runCICheck(ctx context.Context, job *storage.ReviewJob, cfg ciCheckConfig, doc structuredreview.Document, keyPath string, gitEnv []string, out io.Writer) (structuredreview.Document, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if cfg.Agent != "" && cfg.Agent != "codex" {
		return doc, fmt.Errorf("CI checker requires codex")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return doc, fmt.Errorf("read credential proxy key: %w", err)
	}
	if strings.TrimSpace(string(key)) == "" {
		return doc, fmt.Errorf("credential proxy key is empty")
	}
	root, err := os.MkdirTemp("", "roborev-ci-check-*")
	if err != nil {
		return doc, err
	}
	defer os.RemoveAll(root)
	work, gitDir := filepath.Join(root, "work"), filepath.Join(root, "git")
	repo := filepath.Join(work, "repo")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return doc, err
	}
	// Explicit git dirs and a clean config keep host git operations independent of
	// both the caller's environment and the checkout's .git file.
	hostEnv := make([]string, 0)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			hostEnv = append(hostEnv, entry)
		}
	}
	hostEnv = append(hostEnv, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	gitRun := func(dir string, args ...string) error {
		argv := append([]string{"--git-dir=" + dir, "-c", "core.hooksPath=/dev/null"}, args...)
		cmd := exec.CommandContext(ctx, "git", argv...)
		cmd.Env = hostEnv
		data, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("checker git %s: %w: %s", args[0], err, data)
		}
		return nil
	}
	if err := gitRun(filepath.Join(job.RepoPath, ".git"), "clone", "--local", "--no-hardlinks", "--no-checkout", "--separate-git-dir="+gitDir, job.RepoPath, repo); err != nil {
		return doc, err
	}
	if err := gitRun(gitDir, "--work-tree="+repo, "checkout", "--detach", headOf(job.GitRef)); err != nil {
		return doc, err
	}
	base, _, ranged := strings.Cut(job.GitRef, "..")
	if !ranged {
		return doc, fmt.Errorf("CI checker requires frozen base..head range")
	}
	if job.CIBaseBranch == "" {
		return doc, fmt.Errorf("CI checker requires base branch")
	}
	if err := gitRun(gitDir, "update-ref", "refs/remotes/origin/"+job.CIBaseBranch, base); err != nil {
		return doc, err
	}
	if err := os.WriteFile(filepath.Join(work, "findings.md"), []byte(doc.Markdown(job.MinSeverity)), 0o600); err != nil {
		return doc, err
	}
	goEnv := exec.CommandContext(ctx, "go", "env", "GOROOT", "GOMODCACHE")
	goEnv.Dir = root
	data, err := goEnv.Output()
	if err != nil {
		return doc, fmt.Errorf("checker go env: %w", err)
	}
	paths := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(paths) != 2 {
		return doc, fmt.Errorf("checker go env returned incomplete paths")
	}
	goroot, modcache := paths[0], paths[1]
	for _, name := range []string{"go.mod", "go.work"} {
		if _, err := os.Stat(filepath.Join(repo, name)); err == nil {
			sum := filepath.Join(repo, "go.sum")
			original, readErr := os.ReadFile(sum)
			warm := exec.CommandContext(ctx, filepath.Join(goroot, "bin", "go"), "mod", "download")
			warm.Dir = repo
			warm.Env = gitEnv
			if _, err := warm.CombinedOutput(); err != nil {
				fmt.Fprintf(out, "CI checker module warming failed: %v\n", err)
			}
			if readErr == nil {
				err = os.WriteFile(sum, original, 0o600)
			} else if os.IsNotExist(readErr) {
				err = os.Remove(sum)
				if os.IsNotExist(err) {
					err = nil
				}
			} else {
				err = readErr
			}
			if err != nil {
				return doc, fmt.Errorf("restore go.sum: %w", err)
			}
			break
		}
	}
	codex, err := exec.LookPath("codex")
	if err == nil {
		codex, err = filepath.EvalSymlinks(codex)
	}
	if err != nil {
		return doc, fmt.Errorf("find checker codex: %w", err)
	}
	readPaths := []string{gitDir, goroot, modcache, filepath.Dir(filepath.Dir(codex))}
	// Codex loads its system config even with a fresh CODEX_HOME.
	if _, err := os.Stat("/etc/codex"); err == nil {
		readPaths = append(readPaths, "/etc/codex")
	} else if !os.IsNotExist(err) {
		return doc, err
	}
	// npm's launcher needs node as well as the Codex package.
	if header, err := os.ReadFile(codex); err == nil && strings.HasPrefix(string(header), "#!") {
		node, err := exec.LookPath("node")
		if err == nil {
			node, err = filepath.EvalSymlinks(node)
		}
		if err != nil {
			return doc, fmt.Errorf("find checker node: %w", err)
		}
		readPaths = append(readPaths, filepath.Dir(filepath.Dir(node)))
	}
	// nono protects HOME/.nono, so HOME must sit outside its writable grant.
	if err := os.Mkdir(filepath.Join(root, "home"), 0o700); err != nil {
		return doc, err
	}
	for _, dir := range []string{"tmp", "gocache"} {
		if err := os.Mkdir(filepath.Join(work, dir), 0o700); err != nil {
			return doc, err
		}
	}
	sandboxJob := sandbox.Job{
		WorkDir: work, ReadPaths: readPaths, ProxyPort: 8317,
		// Matches the checker evaluation's 8 GiB process budget.
		MemoryMax: 8 << 30,
		Env:       []string{"PATH=" + filepath.Join(goroot, "bin") + ":" + os.Getenv("PATH"), "HOME=" + filepath.Join(root, "home"), "TMPDIR=" + filepath.Join(work, "tmp"), "GOCACHE=" + filepath.Join(work, "gocache"), "GOMODCACHE=" + modcache, "GOPROXY=off", "LANG=C.UTF-8", "TERM=dumb"},
	}
	a := agent.NewCodexAgent(codex).WithSandbox(&agent.CodexSandbox{Job: sandboxJob, ProxyKey: strings.TrimSpace(string(key))})
	a.IgnoreUserConfig = true
	a.Model = cfg.Model
	a.Reasoning = agent.ParseReasoningLevel(cfg.Reasoning)
	raw, err := a.ReviewWithSchema(ctx, repo, job.GitRef, fmt.Sprintf(ciCheckPrompt, job.CIBaseBranch), ciCheckSchema, out)
	if err != nil {
		return doc, fmt.Errorf("run CI checker: %w", err)
	}
	return applyCICheck(doc, raw, job.MinSeverity, out)
}

func applyCICheck(doc structuredreview.Document, raw jsontext.Value, minSeverity string, out io.Writer) (structuredreview.Document, error) {
	var response struct {
		Verdicts []struct {
			Finding  int    `json:"finding"`
			Keep     *bool  `json:"keep"`
			Evidence string `json:"evidence"`
			Reason   string `json:"reason"`
		} `json:"verdicts"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return doc, fmt.Errorf("decode checker verdicts: %w", err)
	}
	counts, drops := make(map[int]int), make(map[int]bool)
	for _, v := range response.Verdicts {
		encoded, _ := json.Marshal(v)
		fmt.Fprintf(out, "CI checker verdict: %s\n", encoded)
		counts[v.Finding]++
		drops[v.Finding] = v.Keep != nil && !*v.Keep && strings.TrimSpace(v.Evidence) != "" && strings.TrimSpace(v.Reason) != ""
	}
	kept := make([]structuredreview.Finding, 0, len(doc.Findings))
	for i, f := range doc.Findings {
		if counts[i+1] != 1 || !drops[i+1] {
			kept = append(kept, f)
		}
	}
	if len(kept) == len(doc.Findings) {
		return doc, nil
	}
	doc.Findings = kept
	if len(kept) == 0 {
		doc.Summary = "No issues found."
		doc.Verdict = structuredreview.VerdictPass
	} else {
		doc.Verdict = structuredreview.VerdictPass
		if !doc.Passed(minSeverity) {
			doc.Verdict = structuredreview.VerdictFail
		}
	}
	return doc, nil
}
