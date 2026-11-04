package main

import (
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/clisearch"
)

func cliSearchCmd(root *cobra.Command) *cobra.Command {
	jobs := directCommand(root, "job")
	clisearch.SetKeywords(directCommand(jobs, "list"), "recent", "failed")
	clisearch.SetKeywords(directCommand(jobs, "show"), "read", "findings")
	clisearch.SetKeywords(directCommand(jobs, "comment"), "log", "note")
	var jsonOutput bool
	opts := cliSearchOptions()
	opts.UsageError = func(err error) error {
		return &exitError{code: 2, cause: err, kind: "usage"}
	}
	opts.Render = func(cmd *cobra.Command, response clisearch.Response) error {
		response.Query = strings.TrimSpace(response.Query)
		if !jsonOutput {
			return clisearch.RenderText(cmd, response)
		}
		// Match the error renderer's replacement policy for byte-invalid input.
		response.Query = strings.ToValidUTF8(response.Query, "\uFFFD")
		if err := json.MarshalWrite(cmd.OutOrStdout(), response); err != nil {
			return err
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout())
		return err
	}
	cmd := clisearch.NewCommand(root, opts)
	// Discovery reads command metadata only, independent of server validation
	// and the root hook's daemon telemetry setup.
	cmd.PersistentPreRunE = func(*cobra.Command, []string) error { return nil }
	cmd.Long = "Find roborev commands by describing a task. Command discovery runs locally without configuration, a repository or a daemon."
	cmd.Example = "  roborev cli search \"review uncommitted changes\""
	search := directCommand(cmd, "search")
	search.Long = "Find commands for a task using local command metadata. Results use preferred command paths and omit interactive UIs and internal hook/server entry points. To search completed review content, use roborev job search."
	search.Example = "  roborev cli search \"review uncommitted changes\"\n  roborev cli search add a note to a review --json"
	search.Flags().BoolVar(&jsonOutput, "json", false, "emit command suggestions as JSON")
	return cmd
}

func cliSearchOptions() clisearch.Options {
	return clisearch.Options{
		Synonyms: map[string][]string{
			"finding":  {"review"},
			"note":     {"comment"},
			"resolve":  {"close"},
			"provider": {"agent"},
			"spending": {"cost"},
			"history":  {"search"},
		},
		Skip: skipCLISearchCommand,
	}
}

func skipCLISearchCommand(cmd *cobra.Command) bool {
	if cmd.Annotations[helpOnlyAnnotation] == "true" {
		return true
	}
	switch cmd.CommandPath() {
	case "roborev tui", "roborev ui", "roborev daemon run", "roborev mcp serve",
		"roborev post-commit", "roborev agent hook run", "roborev agent hook fix-done":
		return true
	}
	return false
}
