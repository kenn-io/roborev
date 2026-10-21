// Command probe reviews one ReviewBench pull request with roborev's review
// pipeline and writes the findings in ReviewBench's judging input format.
//
// The eval script builds this program inside a worktree of each roborev ref
// it compares, so the prompt, schema, and post-processing under test are that
// ref's own. It therefore uses only APIs that every compared ref has; keep it
// that way when changing it. The one exception is pull request context, which
// refs without it skip through the nopullrequest build tag.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/review"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}

// reviewer is one agent configuration: the single reviewer of an ordinary
// run, or one member of a panel.
type reviewer struct {
	name, agent, model, provider, reasoning, reviewType, instructions string
	panel                                                             string
}

// probe holds what every review of one pull request shares.
type probe struct {
	cfg       *config.Config
	repo      string
	gitRef    string
	pr        pullRequest
	producer  string
	artifacts string
	changed   map[string][][2]int
}

func run() error {
	repo := flag.String("repo", "", "checkout of the pull request at its head commit")
	prJSON := flag.String("pr-json", "", "ReviewBench pr.json for the pull request")
	agentName := flag.String("agent", "claude-code", "roborev agent to review with")
	model := flag.String("model", "", "model for the agent; empty uses the agent's default")
	provider := flag.String("provider", "", "pi provider for the model; ignored by other agents")
	reasoning := flag.String("reasoning", "thorough", "reasoning level: fast, standard, or thorough")
	panel := flag.String("panel", "", "review with this roborev panel from the loaded config instead of one agent")
	memberOut := flag.String("member-out", "", "with -panel, a directory for each member's own findings file")
	membersFrom := flag.String("members-from", "", "with -panel, comma-separated directories of saved MEMBER/review.json files to synthesize instead of running members")
	producer := flag.String("producer", "roborev", "name written as the findings' agent and producer")
	out := flag.String("out", "", "where to write the findings file")
	artifacts := flag.String("artifacts", "", "directory for the prompt, review document, and agent log")
	flag.Parse()
	if *repo == "" || *prJSON == "" || *out == "" {
		return fmt.Errorf("-repo, -pr-json, and -out are required")
	}

	raw, err := os.ReadFile(*prJSON)
	if err != nil {
		return err
	}
	var pr pullRequest
	if err := json.Unmarshal(raw, &pr); err != nil {
		return fmt.Errorf("read %s: %w", *prJSON, err)
	}
	cfg, err := config.LoadGlobal()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx := context.Background()
	diff, err := gitDiff(ctx, *repo, pr.Base, pr.Head)
	if err != nil {
		return err
	}
	p := probe{
		cfg: cfg, repo: *repo, gitRef: pr.Base + ".." + pr.Head, pr: pr,
		producer: *producer, artifacts: *artifacts, changed: changedLines(diff),
	}

	if *panel != "" {
		var dirs []string
		if *membersFrom != "" {
			dirs = strings.Split(*membersFrom, ",")
		}
		return p.runPanel(ctx, *panel, *memberOut, dirs, *out)
	}
	result, elapsed, err := p.review(ctx, reviewer{
		agent: *agentName, model: *model, provider: *provider, reasoning: *reasoning,
	}, *artifacts)
	if err != nil {
		return err
	}
	return p.writeFindings(*out, result.StructuredOutput, elapsed)
}

func configure(r reviewer, agentic bool) (agent.Agent, error) {
	a, err := agent.Get(r.agent)
	if err != nil {
		return nil, err
	}
	reasoning := r.reasoning
	if reasoning == "" {
		reasoning = "thorough"
	}
	a = a.WithReasoning(agent.ParseReasoningLevel(reasoning)).WithAgentic(agentic).WithModel(r.model)
	if pi, ok := a.(*agent.PiAgent); ok && r.provider != "" {
		a = pi.WithProvider(r.provider)
	}
	// Codex user settings can enable connectors and web search, which can
	// read the upstream pull request's review comments: the answers.
	a = agent.WithCodexSkillsDisabled(a, true)
	return agent.WithCodexUserConfigIgnored(a, true), nil
}

// review runs one reviewer the way the daemon runs a review job, including a
// panel member's instruction block, and records its artifacts.
func (p probe) review(ctx context.Context, r reviewer, artifacts string) (review.ReviewResult, time.Duration, error) {
	a, err := configure(r, false)
	if err != nil {
		return review.ReviewResult{}, 0, err
	}
	excludes := config.ResolveExcludePatterns(ctx, p.repo, p.cfg, r.reviewType)
	builder := prompt.NewBuilderWithConfig(nil, p.cfg).
		WithContext(ctx).
		ForRepo(p.repo, 0).
		WithStructuredOutput(true)
	built, err := withPullRequest(builder, p.pr).
		BuildWithSnapshot(p.gitRef, 0, a.Name(), r.reviewType, "low", excludes)
	if built.Cleanup != nil {
		defer built.Cleanup()
	}
	if err != nil {
		return review.ReviewResult{}, 0, fmt.Errorf("build prompt: %w", err)
	}
	text := built.Prompt
	if r.instructions != "" {
		// The same block the daemon appends to a panel member's prompt.
		text += fmt.Sprintf("\n\n## Additional reviewer instructions (panel: %s / member: %s)\n%s\n",
			r.panel, r.name, r.instructions)
	}

	log := io.Discard
	if artifacts != "" {
		if err := os.MkdirAll(artifacts, 0o755); err != nil {
			return review.ReviewResult{}, 0, err
		}
		if err := os.WriteFile(filepath.Join(artifacts, "prompt.md"), []byte(text), 0o644); err != nil {
			return review.ReviewResult{}, 0, err
		}
		logFile, err := os.Create(filepath.Join(artifacts, "agent.log"))
		if err != nil {
			return review.ReviewResult{}, 0, err
		}
		defer logFile.Close()
		log = logFile
	}

	started := time.Now()
	result, err := review.RunAgentReview(ctx, a, p.repo, p.gitRef, text, r.reviewType, "low", log)
	if err != nil {
		return review.ReviewResult{}, 0, fmt.Errorf("review %s: %w", r.name, err)
	}
	elapsed := time.Since(started)
	result.Agent, result.ReviewType, result.Status = a.Name(), r.reviewType, review.ResultDone
	if artifacts != "" {
		if err := os.WriteFile(filepath.Join(artifacts, "review.json"), result.StructuredOutput, 0o644); err != nil {
			return review.ReviewResult{}, 0, err
		}
		if err := os.WriteFile(filepath.Join(artifacts, "review.md"), []byte(result.Output), 0o644); err != nil {
			return review.ReviewResult{}, 0, err
		}
	}
	return result, elapsed, nil
}

func (p probe) writeFindings(path string, document []byte, elapsed time.Duration) error {
	file, dropped, err := convert(p.pr, p.producer, document, p.changed, elapsed)
	if err != nil {
		return err
	}
	if dropped > 0 {
		fmt.Fprintf(os.Stderr, "probe: %d finding(s) name no file and cannot be scored\n", dropped)
	}
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}
