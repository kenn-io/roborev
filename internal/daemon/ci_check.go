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

	"go.kenn.io/roborev/internal/agent"
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

func (wp *WorkerPool) checkAndCompleteSynthesis(ctx context.Context, workerID string, job *storage.ReviewJob, res synthesisResult) {
	var snapshot synthesisJobConfig
	if err := json.Unmarshal([]byte(job.PanelMemberConfigJSON), &snapshot); err == nil && snapshot.Check != nil {
		doc, err := reviewpkg.DecodeStructuredReview(res.review.StructuredOutput)
		if err == nil && doc.Legacy == nil && !doc.UnableToReview() && !doc.Passed(job.MinSeverity) {
			jobLog := newAppendingJobLogWriter(job.ID)
			defer jobLog.Close()
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
			checked, checkErr := checker(ctx, job, *snapshot.Check, doc, agent.CodexSandbox{}, env, jobLog)
			if checkErr != nil {
				fmt.Fprintf(jobLog, "CI checker failed; keeping original review: %v\n", checkErr)
			} else {
				passed := checked.Passed(job.MinSeverity)
				checked.Verdict = structuredreview.VerdictPass
				if !passed {
					checked.Verdict = structuredreview.VerdictFail
				}
				raw, encodeErr := json.Marshal(checked)
				if encodeErr == nil {
					res.review.StructuredOutput = raw
					res.review.Output = checked.Markdown(job.MinSeverity)
					res.review.Verdict = storage.VerdictFromPassed(passed)
				}
			}
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			wp.handleUpdateInterruption(ctx, workerID, job)
			return
		}
	}
	wp.completeSynthesisContext(workerID, job, res)
}

// runCICheck owns the disposable checkout and every process used by the checker.
func (wp *WorkerPool) runCICheck(ctx context.Context, job *storage.ReviewJob, cfg ciCheckConfig, doc structuredreview.Document, codexSandbox agent.CodexSandbox, gitEnv []string, out io.Writer) (structuredreview.Document, error) {
	repo, codex, spec, cleanup, err := wp.prepareCICheck(ctx, job, gitEnv, out)
	if err != nil {
		return structuredreview.Document{}, err
	}
	defer cleanup()
	if err := os.WriteFile(filepath.Join(spec.WorkDir, "findings.md"), []byte(doc.Markdown(job.MinSeverity)), 0o600); err != nil {
		return structuredreview.Document{}, err
	}
	codexSandbox.Job = spec
	a := agent.NewCodexAgent(codex).WithSandbox(&codexSandbox)
	a.IgnoreUserConfig = true
	a.Model = cfg.Model
	a.Reasoning = agent.ParseReasoningLevel(cfg.Reasoning)
	base, _, _ := strings.Cut(job.GitRef, "..")
	raw, err := a.ReviewWithSchema(ctx, repo, job.GitRef, fmt.Sprintf(ciCheckPrompt, base), ciCheckSchema, out)
	if err != nil {
		return structuredreview.Document{}, fmt.Errorf("run CI checker: %w", err)
	}
	return applyCICheck(doc, raw, out)
}

// prepareCICheck copies the committed input before warming can update metadata.
// Its caller owns both the copy and the exact checkout until cleanup returns.
func (wp *WorkerPool) prepareCICheck(ctx context.Context, job *storage.ReviewJob, gitEnv []string, out io.Writer) (repo, codex string, spec sandbox.Job, cleanup func(), err error) {
	root, err := os.MkdirTemp("", "roborev-ci-check-*")
	if err != nil {
		return
	}
	cleanup = func() { _ = os.RemoveAll(root) }
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	codex, err = resolveCICheckCommand(ctx, root, "codex")
	if err != nil {
		err = fmt.Errorf("find checker codex: %w", err)
		return
	}
	// Native Codex loads sibling helpers. npm launchers load their package.
	readPaths := []string{filepath.Dir(codex)}
	toolPath := os.Getenv("PATH")
	launcher, openErr := os.Open(codex)
	if openErr != nil {
		err = openErr
		return
	}
	var header [2]byte
	_, readErr := io.ReadFull(launcher, header[:])
	closeErr := launcher.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		err = readErr
		return
	}
	if closeErr != nil {
		err = closeErr
		return
	}
	if string(header[:]) == "#!" {
		var node string
		node, err = resolveCICheckCommand(ctx, root, "node")
		if err != nil {
			err = fmt.Errorf("find checker node: %w", err)
			return
		}
		if filepath.Ext(codex) == ".js" && filepath.Base(filepath.Dir(codex)) == "bin" {
			readPaths[0] = filepath.Dir(filepath.Dir(codex))
			// Resolve the optional native package as the npm launcher does.
			// It may be a sibling package or a package-manager symlink.
			resolve := exec.CommandContext(ctx, node, "--input-type=module", "-e", `
import { createRequire } from 'node:module';
import { dirname } from 'node:path';
const require = createRequire(process.argv[1]);
try {
  console.log(dirname(require.resolve('@openai/codex-' + process.platform + '-' + process.arch + '/package.json')));
} catch (error) {
  if (error.code !== 'MODULE_NOT_FOUND') throw error;
  // The launcher's bundled vendor directory is already granted.
}
`, codex)
			resolve.Dir = root
			var nativePackage []byte
			nativePackage, err = resolve.Output()
			if err != nil {
				err = fmt.Errorf("resolve checker native package: %w", err)
				return
			}
			if path := strings.TrimSpace(string(nativePackage)); path != "" {
				readPaths = append(readPaths, path)
			}
		}
		readPaths = append(readPaths, filepath.Dir(node))
		toolPath = filepath.Dir(node) + string(os.PathListSeparator) + toolPath
	}
	if _, statErr := os.Stat("/etc/codex"); statErr == nil {
		readPaths = append(readPaths, "/etc/codex")
	} else if !os.IsNotExist(statErr) {
		err = statErr
		return
	}
	var checkout string
	var closeCheckout func()
	checkout, closeCheckout, err = wp.createExactCheckout(ctx, "ci-checker", job)
	if err != nil {
		return
	}
	cleanup = func() { closeCheckout(); _ = os.RemoveAll(root) }
	spec.WorkDir = filepath.Join(root, "work")
	repo = filepath.Join(spec.WorkDir, "repo")
	if err = os.MkdirAll(repo, 0o700); err != nil {
		return
	}
	copyCmd := exec.CommandContext(ctx, "cp", "-a", checkout+"/.", repo)
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
	env := []string{"PATH=" + toolPath, "HOME=" + filepath.Join(root, "home"), "TMPDIR=" + filepath.Join(spec.WorkDir, "tmp"), "GOCACHE=" + filepath.Join(spec.WorkDir, "gocache"), "GOPROXY=off", "LANG=C.UTF-8", "TERM=dumb"}
	if len(goPaths) == 2 {
		env[0] = "PATH=" + filepath.Join(goPaths[0], "bin") + string(os.PathListSeparator) + toolPath
		env = append(env, "GOMODCACHE="+goPaths[1])
	}
	// nono protects HOME/.nono, so HOME sits outside its writable grant.
	for _, dir := range []string{filepath.Join(root, "home"), filepath.Join(spec.WorkDir, "tmp"), filepath.Join(spec.WorkDir, "gocache")} {
		if err = os.Mkdir(dir, 0o700); err != nil {
			return
		}
	}
	spec.ReadPaths = readPaths
	// Matches the checker evaluation's 8 GiB process budget.
	spec.MemoryMax = 8 << 30
	spec.Env = env
	return
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

func resolveCICheckCommand(ctx context.Context, dir, name string) (string, error) {
	command, err := exec.LookPath(name)
	if err == nil {
		command, err = filepath.EvalSymlinks(command)
	}
	// mise's symlink shims dispatch by argv[0]. Resolve the installed command
	// outside the checkout, where repository-owned mise config cannot select it.
	if err == nil && filepath.Base(command) == "mise" {
		which := exec.CommandContext(ctx, command, "which", name)
		which.Dir = dir
		path, err := which.Output()
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(strings.TrimSpace(string(path)))
	}
	return command, err
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
