package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
)

func logCmd() *cobra.Command {
	var (
		showPath  bool
		rawOutput bool
		dbPath    string
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

			out := cmd.OutOrStdout()

			if showPath {
				fmt.Fprintln(out, daemon.JobLogPath(jobID))
				return nil
			}

			if rawOutput {
				f, err := os.Open(daemon.JobLogPath(jobID))
				if err != nil {
					return noJobLogError(jobID)
				}
				_, copyErr := io.Copy(out, f)
				closeErr := f.Close()
				if isBrokenPipe(copyErr) {
					if closeErr != nil {
						return fmt.Errorf("closing log: %w", closeErr)
					}
					return nil
				}
				err = errors.Join(copyErr, closeErr)
				if err != nil {
					return fmt.Errorf("reading log: %w", err)
				}
				return nil
			}

			err = renderJobLog(
				jobID, out, streamfmt.WriterIsTerminal(out), dbPath,
			)
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
	cmd.Flags().StringVar(
		&dbPath, "db", storage.DefaultDBPath(),
		"path to sqlite database used for log metadata",
	)

	cmd.AddCommand(logCleanCmd())
	return cmd
}

func noJobLogError(jobID int64) error {
	return fmt.Errorf(
		"no log for job %d (file: %s)",
		jobID, daemon.JobLogPath(jobID),
	)
}

func renderJobLog(
	jobID int64, out io.Writer, isTTY bool, dbPath string,
) (err error) {
	f, err := os.Open(daemon.JobLogPath(jobID))
	if err != nil {
		return noJobLogError(jobID)
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	db, err := storage.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("load metadata for formatted log (use --raw for an orphaned log): %w", err)
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	job, err := db.GetJobByID(jobID)
	if err != nil {
		return fmt.Errorf("load metadata for formatted log (use --raw for an orphaned log): %w", err)
	}

	identity, err := daemon.ResolveJobLogIdentity(job)
	if err != nil {
		return fmt.Errorf("load formatted log identity: %w", err)
	}
	decoder := streamfmt.DecoderForAgent(identity.Agent)
	if identity.Source == storage.JobSourceAutoDesign {
		decoder = streamfmt.LegacyMixedDecoder(identity.Agent)
	}
	fmtr := streamfmt.New(out, isTTY, decoder)
	return streamfmt.RenderLogWith(f, fmtr)
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
			maxAge := time.Duration(maxDays) * 24 * time.Hour
			n := daemon.CleanJobLogs(maxAge)
			fmt.Printf("Removed %d log file(s)\n", n)
			return nil
		},
	}

	cmd.Flags().IntVar(
		&maxDays, "days", 7,
		"remove logs older than this many days",
	)

	return cmd
}
