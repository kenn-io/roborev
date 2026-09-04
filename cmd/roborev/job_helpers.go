// ABOUTME: Shared helper functions for job polling and completion
// ABOUTME: Consolidates duplicate polling logic from compact, analyze, fix, and run commands
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

// waitForJobCompletion polls a job until it completes, streaming output if provided.
// This consolidates polling logic used across compact, analyze, fix, and run commands.
func waitForJobCompletion(ctx context.Context, serverAddr string, jobID int64, output io.Writer) (*storage.Review, error) {
	ep, err := daemon.ParseEndpoint(serverAddr)
	if err != nil {
		return nil, err
	}
	return waitForJobCompletionAt(ctx, ep, jobID, output)
}

func waitForJobCompletionAt(ctx context.Context, ep daemon.DaemonEndpoint, jobID int64, output io.Writer) (*storage.Review, error) {
	api := newDaemonReviewAPI(ep.BaseURL(), ep.HTTPClient(30*time.Second))
	pollInterval := 1 * time.Second
	maxInterval := 5 * time.Second
	lastOutputLen := 0
	lastStatus := ""
	waitDots := 0

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}

		job, err := api.getJob(ctx, jobID)
		if err != nil {
			if errors.Is(err, ErrJobNotFound) {
				return nil, err
			}
			continue
		}

		// Show status progress indicator while waiting
		if output != nil && lastStatus != string(job.Status) {
			lastStatus = string(job.Status)
			switch job.Status {
			case storage.JobStatusQueued:
				fmt.Fprint(output, "Waiting for agent to start")
			case storage.JobStatusRunning:
				fmt.Fprint(output, "\nAgent processing")
			}
			waitDots = 0
		}

		// Show dots while waiting (before any output appears)
		if output != nil && job.Status == storage.JobStatusQueued {
			waitDots++
			if waitDots%2 == 0 { // Show dot every 2 seconds
				fmt.Fprint(output, ".")
			}
		}

		if output != nil && job.Status == storage.JobStatusRunning && lastOutputLen == 0 {
			waitDots++
			if waitDots%2 == 0 {
				fmt.Fprint(output, ".")
			}
		}

		// Stream partial output while running
		if output != nil && job.Status == storage.JobStatusRunning {
			review, err := api.getReview(ctx, jobID, "review")
			if err == nil && len(review.Output) > lastOutputLen {
				// First output - add newline after waiting dots
				if lastOutputLen == 0 && waitDots > 0 {
					fmt.Fprintln(output)
				}
				fmt.Fprint(output, review.Output[lastOutputLen:])
				lastOutputLen = len(review.Output)
			}
		}

		switch job.Status {
		case storage.JobStatusDone:
			review, err := api.getReview(ctx, jobID, "review")
			if err != nil {
				return nil, err
			}

			// Print any remaining output
			if output != nil && len(review.Output) > lastOutputLen {
				fmt.Fprint(output, review.Output[lastOutputLen:])
			}

			return review, nil

		case storage.JobStatusFailed:
			return nil, fmt.Errorf("job failed: %s", job.Error)

		case storage.JobStatusCanceled:
			return nil, fmt.Errorf("job was canceled")
		}

		// Exponential backoff
		if pollInterval < maxInterval {
			pollInterval = min(pollInterval*3/2, maxInterval)
		}
	}
}
