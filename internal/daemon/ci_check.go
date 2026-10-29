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

	"go.kenn.io/kit/pathresolve"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	gitpkg "go.kenn.io/roborev/internal/git"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/sandbox"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

type ciCheckConfig struct {
	Model     string `json:"model,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
}

var ciCheckSchema = jsontext.Value(`{"type":"object","additionalProperties":false,"required":["verdicts"],"properties":{"verdicts":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["finding","keep","evidence","reason"],"properties":{"finding":{"type":"integer"},"keep":{"type":"boolean"},"evidence":{"type":"string"},"reason":{"type":"string"}}}}}}`)

const ciCheckPrompt = `An automated reviewer posted the findings in ../findings.md on this pull request. You are its last step before the comment posts: keep each finding only if it holds up. The worktree you're in is the PR head; %s is its frozen base commit.

Work in a scratch copy (cp -a . ../check-$$). Leave this worktree unchanged.

For each finding:
1. Make it fail. Write a test or command that shows the failure, starting from a state the product, its users, or an outside system can create: a file a user imports, a config value they type, and a server's reply all count. For an order of events, force that order.
2. Keep it if it fails that way and either a user on a normal workflow hits it, or the harm is lost data, a silently saved wrong result, or a security hole. Keep a finding about the project's own tests or CI when a test can't catch the bug it exists for and the fix is small.
3. Drop it if it doesn't fail, if nothing but a hand-built state reaches it, or if only rare timing or unusual input reaches it and the fix would add new machinery. Count everyone the failure reaches: in a library, a rare input in one consumer is still a broken build or wrong result in every repo that uses it. A bug that predates this PR still counts when the PR's code runs into it. Also drop it if the repo's AGENTS.md, CLAUDE.md, or .roborev.toml review guidelines say not to flag it.

If the sandbox stops a finding from running, keep it.
Return a JSON object with verdicts: [{"finding": 1, "keep": true, "evidence": "the command and its output, or what refutes it", "reason": "one sentence"}].
Work alone: no sub-agents or background jobs.`

func (wp *WorkerPool) checkAndCompleteSynthesis(ctx context.Context, workerID string, job *storage.ReviewJob, res synthesisResult, doc structuredreview.Document, check *ciCheckConfig) {
	res, ran := wp.checkSynthesis(ctx, job, res, doc, check)
	if ran && errors.Is(ctx.Err(), context.Canceled) {
		wp.handleUpdateInterruption(ctx, workerID, job)
		return
	}
	wp.completeSynthesisContext(workerID, job, res)
}

func (wp *WorkerPool) checkSynthesis(ctx context.Context, job *storage.ReviewJob, res synthesisResult, doc structuredreview.Document, check *ciCheckConfig) (synthesisResult, bool) {
	if check != nil && doc.Legacy == nil && !doc.UnableToReview() && len(doc.Findings) > 0 {
		jobLog := newAppendingJobLogWriter(job.ID)
		defer jobLog.Close()
		outputWriter := wp.outputBuffers.Writer(job.ID, GetNormalizer("codex"))
		defer outputWriter.Flush()
		out := io.MultiWriter(jobLog, outputWriter)
		var env []string
		if wp.ciGitEnv != nil {
			if panel, err := wp.db.GetCIPanelBySynthesisJobID(job.ID); err == nil && panel != nil {
				env = wp.ciGitEnv(panel.GithubRepo)
			}
		}
		checker := wp.ciCheck
		if checker == nil {
			checker = wp.runCICheck
		}
		checked, checkErr := checker(ctx, job, *check, doc, env, out)
		if checkErr != nil {
			fmt.Fprintf(out, "CI checker failed; keeping original review: %v\n", checkErr)
		} else if len(checked.Findings) < len(doc.Findings) {
			verdict := reviewpkg.SynthesisVerdict(checked, job.MinSeverity)
			checked.Verdict = structuredreview.VerdictPass
			if verdict == storage.VerdictFail {
				checked.Verdict = structuredreview.VerdictFail
			}
			raw, encodeErr := json.Marshal(checked)
			if encodeErr == nil {
				res.review.StructuredOutput = raw
				res.review.Output = checked.Markdown(job.MinSeverity)
				res.review.Verdict = verdict
			}
		}
		return res, true
	}
	return res, false
}

// runCICheck owns the disposable checkout and every process used by the checker.
func (wp *WorkerPool) runCICheck(ctx context.Context, job *storage.ReviewJob, cfg ciCheckConfig, doc structuredreview.Document, gitEnv []string, out io.Writer) (structuredreview.Document, error) {
	baseAgent, err := agent.GetAvailableExactWithConfigFromConfig(nil, "codex", wp.cfgGetter.Config())
	if err != nil {
		return structuredreview.Document{}, fmt.Errorf("get checker codex: %w", err)
	}
	codex, ok := baseAgent.(*agent.CodexAgent)
	if !ok {
		return structuredreview.Document{}, fmt.Errorf("checker requires Codex, got %T", baseAgent)
	}
	run, err := wp.prepareCICheck(ctx, job, codex.Command, gitEnv, out)
	if err != nil {
		return structuredreview.Document{}, err
	}
	defer run.cleanup()
	if err := os.WriteFile(filepath.Join(run.Job.WorkDir, "findings.md"), []byte(doc.Markdown(job.MinSeverity)), 0o600); err != nil {
		return structuredreview.Document{}, err
	}
	a := codex.WithSandbox(&agent.CodexSandbox{Job: run.Job, Upstream: wp.ciCheckUpstream})
	a.Command = run.Command
	a.IgnoreUserConfig = true
	a.Model = cfg.Model
	a.Reasoning = agent.ParseReasoningLevel(cfg.Reasoning)
	base, _, _ := strings.Cut(job.GitRef, "..")
	raw, err := a.ReviewWithSchema(ctx, run.Repo, job.GitRef, fmt.Sprintf(ciCheckPrompt, base), ciCheckSchema, out)
	if err != nil {
		return structuredreview.Document{}, fmt.Errorf("run CI checker: %w", err)
	}
	return applyCICheck(doc, raw, out)
}

type ciCheckRun struct {
	Command string
	Repo    string
	Job     sandbox.Job
	cleanup func()
}

// prepareCICheck copies the committed input before warming can update metadata.
// Its caller owns both the copy and the exact checkout until cleanup returns.
func (wp *WorkerPool) prepareCICheck(ctx context.Context, job *storage.ReviewJob, command string, gitEnv []string, out io.Writer) (run ciCheckRun, err error) {
	root, err := os.MkdirTemp("", "roborev-ci-check-*")
	if err != nil {
		return
	}
	run.cleanup = func() { _ = os.RemoveAll(root) }
	defer func() {
		if err != nil {
			run.cleanup()
		}
	}()
	resolved, err := resolveCICheckCodex(ctx, root, command)
	if err != nil {
		return
	}
	run.Command = resolved.Command
	readPaths := resolved.ReadPaths
	toolPath := os.Getenv("PATH")
	if resolved.PathDir != "" {
		toolPath = resolved.PathDir + string(os.PathListSeparator) + toolPath
	}
	var checkout string
	var closeCheckout func()
	checkout, closeCheckout, err = wp.createExactCheckout(ctx, "ci-checker", job)
	if err != nil {
		return
	}
	run.cleanup = func() { closeCheckout(); _ = os.RemoveAll(root) }
	run.Job.WorkDir = filepath.Join(root, "work")
	run.Repo = filepath.Join(run.Job.WorkDir, "repo")
	if err = os.MkdirAll(run.Repo, 0o700); err != nil {
		return
	}
	copyCmd := exec.CommandContext(ctx, "cp", "-a", checkout+"/.", run.Repo)
	if output, copyErr := copyCmd.CombinedOutput(); copyErr != nil {
		err = fmt.Errorf("copy checker checkout: %w: %s", copyErr, output)
		return
	}
	var commonDir string
	commonDir, err = gitpkg.ResolveGitCommonDir(job.RepoPath)
	if err != nil {
		return
	}
	readPaths = append(readPaths, commonDir)
	var goPaths []string
	goPaths, err = warmGoModules(ctx, checkout, root, gitEnv, out)
	if err != nil {
		return
	}
	readPaths = append(readPaths, goPaths...)
	if err = validateCICheckReadPaths(readPaths); err != nil {
		return
	}
	env := []string{"PATH=" + toolPath, "HOME=" + filepath.Join(root, "home"), "TMPDIR=" + filepath.Join(run.Job.WorkDir, "tmp"), "GOCACHE=" + filepath.Join(run.Job.WorkDir, "gocache"), "GOPROXY=off", "LANG=C.UTF-8", "TERM=dumb"}
	if len(goPaths) == 2 {
		env[0] = "PATH=" + filepath.Join(goPaths[0], "bin") + string(os.PathListSeparator) + toolPath
		env = append(env, "GOMODCACHE="+goPaths[1])
	}
	// nono protects HOME/.nono, so HOME sits outside its writable grant.
	for _, dir := range []string{filepath.Join(root, "home"), filepath.Join(run.Job.WorkDir, "tmp"), filepath.Join(run.Job.WorkDir, "gocache")} {
		if err = os.Mkdir(dir, 0o700); err != nil {
			return
		}
	}
	run.Job.ReadPaths = readPaths
	// Matches the checker evaluation's 8 GiB process budget.
	run.Job.MemoryMax = 8 << 30
	run.Job.Env = env
	return
}

// Every read grant must leave host homes outside the checker's filesystem.
func validateCICheckReadPaths(paths []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	codexHome, err := config.CodexHomeDir()
	if err != nil {
		return err
	}
	for _, hostHome := range []string{home, codexHome} {
		hostHome, err = filepath.Abs(hostHome)
		if err == nil {
			hostHome, err = pathresolve.EvalSymlinksAllowMissing(hostHome)
		}
		if err != nil {
			return err
		}
		for _, path := range paths {
			grant, err := filepath.Abs(path)
			if err == nil {
				grant, err = pathresolve.EvalSymlinksAllowMissing(grant)
			}
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(grant, hostHome)
			if err != nil {
				return err
			}
			if rel == "." || filepath.IsLocal(rel) {
				return fmt.Errorf("checker read grant contains host home: %q", path)
			}
		}
	}
	return nil
}

func warmGoModules(ctx context.Context, checkout, root string, gitEnv []string, out io.Writer) ([]string, error) {
	hasModule := false
	for _, name := range []string{"go.mod", "go.work", "go.sum", "go.work.sum"} {
		path := filepath.Join(checkout, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			if err := os.RemoveAll(path); err != nil {
				return nil, err
			}
		} else if name == "go.mod" || name == "go.work" {
			hasModule = true
		}
	}
	if !hasModule {
		return nil, nil
	}
	goEnv := exec.CommandContext(ctx, "go", "env", "GOROOT", "GOMODCACHE")
	goEnv.Dir = root
	data, err := goEnv.Output()
	if err != nil {
		fmt.Fprintf(out, "CI checker go env failed: %v\n", err)
		return nil, nil
	}
	paths := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(paths) != 2 || paths[0] == "" || paths[1] == "" {
		return nil, fmt.Errorf("checker go env returned incomplete paths")
	}
	warm := exec.CommandContext(ctx, filepath.Join(paths[0], "bin", "go"), "mod", "download")
	warm.Dir = checkout
	warm.Env = gitEnv
	if output, err := warm.CombinedOutput(); err != nil {
		fmt.Fprintf(out, "CI checker module warming failed: %v: %s\n", err, output)
	}
	return paths, nil
}

func applyCICheck(doc structuredreview.Document, raw jsontext.Value, out io.Writer) (structuredreview.Document, error) {
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
	doc.Summary = fmt.Sprintf("%d findings.", len(kept))
	if len(kept) == 1 {
		doc.Summary = "1 finding."
	}
	return doc, nil
}
