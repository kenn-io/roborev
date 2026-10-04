package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

// goalReviewRunner keeps CLI flow tests independent of an installed agent CLI.
type goalReviewRunner func(context.Context, agent.Agent, string, goalreview.Snapshot, prompt.SnapshotResult, io.Writer) ([]goalreview.Finding, error)

func runGoalReview(cmd *cobra.Command, root string, options goalreview.AgentOptions, local, wait, quiet bool, spec, plan *string) error {
	return runGoalReviewWithRunner(cmd, root, options, local, wait, quiet, spec, plan, goalreview.RunPrepared)
}

func runGoalReviewWithRunner(cmd *cobra.Command, root string, options goalreview.AgentOptions, local, wait, quiet bool, spec, plan *string, runner goalReviewRunner) error {
	if !local {
		if err := ensureDaemon(); err != nil {
			return err
		}
		ep := getDaemonEndpoint()
		data, err := json.Marshal(daemon.EnqueueRequest{RepoPath: root, ReviewType: config.ReviewTypeGoal, Agent: options.Agent, Model: options.Model, Provider: options.Provider, Reasoning: options.Reasoning, SpecFile: spec, PlanFile: plan})
		if err != nil {
			return err
		}
		resp, err := ep.APIClient(0).EnqueueJobRaw(cmd.Context(), nil, roborevclient.WithBody(data))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("goal review failed: %s", body)
		}
		var job storage.ReviewJob
		if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
			return err
		}
		if !quiet {
			cmd.Println(describeEnqueue(job, 0, false))
		}
		if wait {
			return silenceIfExit(cmd, waitForJob(cmd, ep, job.ID, quiet))
		}
		return nil
	}
	cfg, err := config.LoadGlobal()
	if err != nil {
		return err
	}
	repo, err := config.LoadRepoConfig(root)
	if err != nil {
		return err
	}
	selection, err := goalreview.Select(repo, spec, plan)
	if err != nil {
		return err
	}
	snapshot, err := goalreview.Capture(cmd.Context(), root, selection, kata.NewCLIClient(root))
	if err != nil {
		return err
	}
	prepared, err := prompt.NewBuilderWithConfig(nil, cfg).ForRepo(root, 0).Prepare(
		goalreview.BuildPrompt(snapshot),
		prompt.SnapshotTarget{RepoPath: root, ConfigRepoPath: root},
	)
	if err != nil {
		return err
	}
	if prepared.Cleanup != nil {
		defer prepared.Cleanup()
	}
	a, _, err := goalreview.ResolveAgent(root, cfg, options)
	if err != nil {
		return err
	}
	findings, err := runner(cmd.Context(), a, root, snapshot, prepared, io.Discard)
	if err != nil {
		return err
	}
	if !quiet {
		cmd.Println(goalreview.Render(findings))
	}
	if len(findings) > 0 {
		return silenceIfExit(cmd, &exitError{code: 1})
	}
	return nil
}
