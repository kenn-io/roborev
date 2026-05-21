package agent

import (
	"fmt"
	"strings"
)

type detailedCLIWaitErrorOptions struct {
	AgentName      string
	Stderr         string
	FallbackOutput string
	FallbackLabel  string
	PartialOutput  string
}

func formatStreamingCLIWaitError(agentName string, runResult streamingCLIResult, stderr string) error {
	if runResult.ParseErr != nil {
		return fmt.Errorf("%s failed: %w (parse error: %w)\nstderr: %s", agentName, runResult.WaitErr, runResult.ParseErr, stderr)
	}
	return fmt.Errorf("%s failed: %w\nstderr: %s", agentName, runResult.WaitErr, stderr)
}

func formatDetailedCLIWaitError(runResult streamingCLIResult, opts detailedCLIWaitErrorOptions) error {
	var detail strings.Builder
	fmt.Fprintf(&detail, "%s failed", opts.AgentName)

	if runResult.ParseErr != nil {
		fmt.Fprintf(&detail, "\nstream: %v", runResult.ParseErr)
	}
	if opts.Stderr != "" {
		fmt.Fprintf(&detail, "\nstderr: %s", opts.Stderr)
	} else if opts.FallbackOutput != "" {
		fmt.Fprintf(&detail, "\n%s: %s", opts.FallbackLabel, opts.FallbackOutput)
	}
	if partial := truncateCLIWaitErrorOutput(opts.PartialOutput); partial != "" {
		fmt.Fprintf(&detail, "\npartial output: %s", partial)
	}

	return fmt.Errorf("%s: %w", detail.String(), runResult.WaitErr)
}

func truncateCLIWaitErrorOutput(output string) string {
	if output == "" {
		return ""
	}
	if len(output) <= 500 {
		return output
	}
	return output[:500] + "..."
}
