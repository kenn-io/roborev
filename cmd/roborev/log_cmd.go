package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
	roborevclient "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

func logCmd() *cobra.Command {
	var (
		showPath  bool
		rawOutput bool
	)

	cmd := &cobra.Command{
		Use:   "log <job-id>",
		Short: "Show agent output log for a job",
		Long: `Show the agent output log for a completed or running job.

By default, JSONL agent output is rendered as human-readable
progress lines (tool calls, agent text). Non-JSON logs are
printed as-is.

Use --raw to print the original log bytes unchanged.

Examples:
  roborev log 42          # Human-friendly rendered output
  roborev log --raw 42    # Raw log bytes (JSONL)
  roborev log --path 42   # Print the log file path`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job ID: %w", err)
			}

			if err := ensureDaemon(); err != nil {
				return err
			}
			api := getDaemonEndpoint().APIClient(0)
			if !rawOutput && !showPath {
				err := renderJobLog(cmd.Context(), jobID, cmd.OutOrStdout(), streamfmt.WriterIsTerminal(cmd.OutOrStdout()), api)
				if isBrokenPipe(err) {
					return nil
				}
				return err
			}
			resp, err := api.GetJobLogRaw(cmd.Context(), &generated.GetJobLogRequestOptions{Query: &generated.GetJobLogQuery{
				JobID: new(strconv.FormatInt(jobID, 10)), Raw: &rawOutput, Path: &showPath,
			}})
			if err != nil {
				return fmt.Errorf("fetch job log: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("no log for job %d; daemon returned %s (use --raw for an orphaned log)", jobID, resp.Status)
			}
			out := cmd.OutOrStdout()
			if showPath {
				_, err = fmt.Fprintln(out, resp.Header.Get("X-Log-Path"))
				return err
			}
			_, err = io.Copy(out, resp.Body)
			if isBrokenPipe(err) {
				return nil
			}
			return err
		},
	}

	cmd.Flags().BoolVar(
		&showPath, "path", false,
		"print the log file path instead of contents",
	)
	cmd.Flags().BoolVar(
		&rawOutput, "raw", false,
		"print raw log bytes without formatting",
	)
	cmd.AddCommand(logCleanCmd())
	return cmd
}

func renderJobLog(ctx context.Context, jobID int64, out io.Writer, isTTY bool, api *roborevclient.Client) error {
	resp, err := api.GetJobLogRaw(ctx, &generated.GetJobLogRequestOptions{Query: &generated.GetJobLogQuery{JobID: new(strconv.FormatInt(jobID, 10))}})
	if err != nil {
		return fmt.Errorf("fetch job log: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("no log for job %d; load metadata for formatted log (use --raw for an orphaned log): daemon returned %s", jobID, resp.Status)
	}
	return renderLogResponse(resp, out, isTTY)
}

func renderLogResponse(resp *http.Response, out io.Writer, isTTY bool) error {
	name := resp.Header.Get("X-Job-Agent")
	decoder := streamfmt.DecoderForAgent(name)
	if resp.Header.Get("X-Job-Source") == storage.JobSourceAutoDesign {
		decoder = streamfmt.LegacyMixedDecoder(name)
	}
	return streamfmt.RenderLogWith(resp.Body, streamfmt.New(out, isTTY, decoder))
}

// isBrokenPipe returns true if err is a broken pipe (EPIPE) error,
// which happens when output is piped to tools like head that close
// the read end early.
func isBrokenPipe(err error) bool {
	return err != nil && errors.Is(err, syscall.EPIPE)
}

func logCleanCmd() *cobra.Command {
	var maxDays int

	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Remove old job log files",
		Long: `Remove job log files older than the specified age.

Examples:
  roborev log clean          # Remove logs older than 7 days
  roborev log clean --days 3 # Remove logs older than 3 days`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if maxDays < 0 || maxDays > 3650 {
				return fmt.Errorf(
					"--days must be between 0 and 3650",
				)
			}
			if err := ensureDaemon(); err != nil {
				return err
			}
			result, err := getDaemonEndpoint().APIClient(0).CleanJobLogs(cmd.Context(), &generated.CleanJobLogsRequestOptions{
				Body: &generated.CleanJobLogsBody{Days: int64(maxDays)},
			})
			if err != nil {
				return daemonRequestError("clean job logs", err)
			}
			fmt.Printf("Removed %d log file(s)\n", result.Removed)
			return nil
		},
	}

	cmd.Flags().IntVar(
		&maxDays, "days", 7,
		"remove logs older than this many days",
	)

	return cmd
}
