// Command probe reviews one ReviewBench pull request with roborev's review
// pipeline and writes the findings in ReviewBench's judging input format.
//
// The eval script builds this program inside a worktree of each roborev ref
// it compares, so the prompt, schema, and post-processing under test are that
// ref's own. It therefore uses only APIs that every compared ref has; keep it
// that way when changing it.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func run() error {
	repo := flag.String("repo", "", "checkout of the pull request at its head commit")
	prJSON := flag.String("pr-json", "", "ReviewBench pr.json for the pull request")
	agentName := flag.String("agent", "claude-code", "roborev agent to review with")
	model := flag.String("model", "", "model for the agent; empty uses the agent's default")
	provider := flag.String("provider", "", "pi provider for the model; ignored by other agents")
	reasoning := flag.String("reasoning", "thorough", "reasoning level: fast, standard, or thorough")
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
	gitRef := pr.Base + ".." + pr.Head

	cfg, err := config.LoadGlobal()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	a, err := agent.Get(*agentName)
	if err != nil {
		return err
	}
	a = a.WithReasoning(agent.ParseReasoningLevel(*reasoning)).WithModel(*model)
	if pi, ok := a.(*agent.PiAgent); ok && *provider != "" {
		a = pi.WithProvider(*provider)
	}
	// Codex user settings can enable connectors and web search, which can
	// read the upstream pull request's review comments: the answers.
	a = agent.WithCodexSkillsDisabled(a, true)
	a = agent.WithCodexUserConfigIgnored(a, true)

	ctx := context.Background()
	excludes := config.ResolveExcludePatterns(ctx, *repo, cfg, "")
	built, err := prompt.NewBuilderWithConfig(nil, cfg).
		WithContext(ctx).
		ForRepo(*repo, 0).
		WithStructuredOutput(true).
		BuildWithSnapshot(gitRef, 0, a.Name(), "", "low", excludes)
	if built.Cleanup != nil {
		defer built.Cleanup()
	}
	if err != nil {
		return fmt.Errorf("build prompt: %w", err)
	}

	log := io.Discard
	if *artifacts != "" {
		if err := os.MkdirAll(*artifacts, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*artifacts, "prompt.md"), []byte(built.Prompt), 0o644); err != nil {
			return err
		}
		logFile, err := os.Create(filepath.Join(*artifacts, "agent.log"))
		if err != nil {
			return err
		}
		defer logFile.Close()
		log = logFile
	}

	started := time.Now()
	result, err := review.RunAgentReview(ctx, a, *repo, gitRef, built.Prompt, "", "low", log)
	if err != nil {
		return fmt.Errorf("review: %w", err)
	}
	elapsed := time.Since(started)
	if *artifacts != "" {
		if err := os.WriteFile(filepath.Join(*artifacts, "review.json"), result.StructuredOutput, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*artifacts, "review.md"), []byte(result.Output), 0o644); err != nil {
			return err
		}
	}

	diff, err := gitDiff(ctx, *repo, pr.Base, pr.Head)
	if err != nil {
		return err
	}
	file, dropped, err := convert(pr, *producer, result.StructuredOutput, changedLines(diff), elapsed)
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
	return os.WriteFile(*out, append(encoded, '\n'), 0o644)
}
