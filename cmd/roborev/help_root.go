package main

import (
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// applyRootHelp configures documentation after the entire command tree exists.
func applyRootHelp(root *cobra.Command) {
	cobra.AddTemplateFunc("roborevGlobalFlags", roborevGlobalFlags)
	root.Long = roborevRootHelp
	root.Example = "  roborev doctor\n  roborev review HEAD\n  roborev show --job 42"
	for _, group := range []cobra.Group{
		{ID: "review", Title: "Review code:"},
		{ID: "fix", Title: "Fix findings:"},
		{ID: "setup", Title: "Set up and diagnose:"},
		{ID: "data", Title: "Manage daemon and data:"},
		{ID: "maintenance", Title: "Maintain roborev:"},
	} {
		root.AddGroup(&group)
	}
	root.SetHelpCommandGroupID("setup")
	root.CompletionOptions.HiddenDefaultCmd = true
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	var configure func(*cobra.Command)
	configure = func(cmd *cobra.Command) {
		if cmd.Parent() == root {
			cmd.GroupID = roborevCommandGroups[cmd.Name()]
		}
		// Retain the original examples and explanatory text while giving Cobra a
		// dedicated Examples section. Some descriptions continue after the examples.
		if before, after, ok := strings.Cut(cmd.Long, "\nExamples:\n"); ok {
			var descriptions, examples []string
			continuation := false
			for _, line := range strings.Split(after, "\n") {
				trimmed := strings.TrimSpace(line)
				isExample := continuation || trimmed == "roborev" || strings.HasPrefix(trimmed, "roborev ") ||
					strings.Contains(trimmed, "| roborev ") || strings.HasPrefix(trimmed, "#")
				if isExample || trimmed == "" {
					examples = append(examples, line)
				}
				if !isExample {
					descriptions = append(descriptions, line)
				}
				continuation = isExample && strings.HasSuffix(trimmed, "\\")
			}
			cmd.Long = strings.TrimSpace(before + "\n\n" + strings.Join(descriptions, "\n"))
			cmd.Example = strings.Trim(strings.Join(examples, "\n"), "\n")
		}
		if help, ok := roborevCommandHelp[strings.TrimPrefix(cmd.CommandPath(), "roborev ")]; ok {
			if help.long != "" {
				if cmd.Long == "" || cmd.Long == cmd.Short {
					cmd.Long = help.long
				} else {
					cmd.Long = strings.TrimSpace(cmd.Long) + "\n\n" + help.long
				}
			}
			if help.example != "" {
				cmd.Example = help.example
			}
		}
		if cmd.Name() == "list" && cmd.Parent() == root {
			cmd.Aliases = append(cmd.Aliases, "ls")
		}
		if cmd.Name() == "close" && cmd.Parent() == root {
			cmd.Aliases = append(cmd.Aliases, "resolve")
		}
		for _, child := range cmd.Commands() {
			configure(child)
		}
	}
	for _, child := range root.Commands() {
		configure(child)
	}
	root.SetUsageTemplate(roborevUsageTemplate)
}

var roborevCommandGroups = map[string]string{
	"review": "review", "wait": "review", "list": "review", "show": "review", "search": "review", "analyze": "review", "insights": "review", "tui": "review", "ui": "review", "summary": "review", "cost": "review", "stream": "review", "log": "review", "enqueue": "review", "prompt": "review",
	"fix": "fix", "refine": "fix", "run": "fix", "comment": "fix", "respond": "fix", "close": "fix", "compact": "fix",
	"init": "setup", "doctor": "setup", "quickstart": "setup", "check-agents": "setup", "config": "setup", "install-hook": "setup", "uninstall-hook": "setup", "agent-hook": "setup", "skills": "setup", "mcp": "setup", "signing-init": "setup", "help": "setup", "completion": "setup",
	"daemon": "data", "status": "data", "pause": "data", "unpause": "data", "cancel": "data", "snooze": "data", "repo": "data", "export": "data", "sync": "data", "ci": "data", "post-commit": "data", "remap": "data",
	"update": "maintenance", "version": "maintenance", "backfill-tokens": "maintenance", "backfill-verdicts": "maintenance", "legacy-reviews": "maintenance", "verify-web-assets": "maintenance",
}

func roborevGlobalFlags(cmd *cobra.Command) string {
	var names []string
	cmd.InheritedFlags().VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden {
			return
		}
		name := "--" + flag.Name
		if flag.Shorthand != "" && flag.ShorthandDeprecated == "" {
			name = "-" + flag.Shorthand + "/" + name
		}
		if flag.Value.Type() != "bool" {
			name += " <" + flag.Value.Type() + ">"
		}
		names = append(names, name)
	})
	return strings.Join(names, ", ")
}

const roborevRootHelp = `roborev reviews git commits with AI agents and stores findings in a local daemon.
Use the CLI directly or install optional coding-agent skills and hooks.

Agent workflow (replace job 42 with the ID returned by review or list):
  roborev doctor                      # 1. diagnose configuration and agents
  roborev init                        # 2. initialize this repo and its git hook
  roborev review HEAD                 # 3. enqueue a review; records a job ID
  roborev wait --job 42                # 4. wait for that existing job
  roborev show --job 42                # 5. read findings
  roborev fix 42                      # 6. run a foreground agent to edit and commit
  roborev comment --job 42 "Fixed and verified the findings"  # 7. record evidence
  roborev close 42                    # 8. resolve any remaining open review
A fix closes its completed job automatically. Review the resulting commit again
when you need fresh verification. Use fix --list to preview candidate jobs;
refine automates repeated review and fix cycles. These agent flows can edit code.

IDs and refs:
  Job IDs are positive integers from review/list, scoped to the selected daemon.
  show, wait and comment try an argument as a local git ref first, then as a
  numeric job ID; use --job to force a job ID. show/wait default to HEAD.
  fix, close and cancel take job IDs. review takes git refs, not job IDs.

Repository and server:
  Start in the git checkout. Commands with --repo can target another repository;
  its meaning is command-specific (path, stored identity, or filter). list also
  discovers repositories in immediate child directories outside a checkout.
  Without --server, the CLI discovers the local daemon from ROBOREV_DATA_DIR
  (default ~/.roborev) and auto-starts it for commands that require it.
  --server explicitly selects a running loopback address, unix:// socket, or
  remote HTTPS base URL; it does not start or replace that target.
  Remote HTTPS supports list, show --job, wait --job and stream with signing
  credentials and repository grants. Local git refs are not resolved remotely.

Output and errors:
  Output modes belong to individual commands: check each command's --help.
  list/show/search/status/doctor and other commands offer --json; it is not a
  global flag. Export subcommands emit JSON using their own --format option.
  search --agent emits concise text; other --agent flags select an AI provider
  or hook profile. show --json returns the review record and stored document;
  list --json includes job status and verdict. Errors go to stderr with a hint.
  Commands with --json emit structured errors on stderr; search --agent uses
  concise errors. Quiet commands retain their existing reduced output.
  Exit codes: 0 success; 1 runtime failure or a completed FAIL review;
  2 invocation/usage error; 3 export cursor belongs to a different database
  (discard it and backfill); 4 invalid user-supplied value. Displaying a FAIL
  with show succeeds; doctor/quickstart report failed diagnosis with exit 1.

Environment:
  ROBOREV_DATA_DIR changes the local config, runtime, database and log directory.
  ROBOREV_COLOR_MODE selects auto/dark/light/none; NO_COLOR disables color.
  ROBOREV_TELEMETRY_ENABLED=0 disables anonymous usage telemetry.
  EDITOR chooses the editor for comment when no message is provided.
  GH_TOKEN/GITHUB_TOKEN and GITLAB_TOKEN/GL_TOKEN authenticate CI forge access.
Run roborev quickstart for repository-specific setup and workflow guidance.`

// Cobra's usage template with compact inherited flags; root flag descriptions
// remain complete because root flags are local to the root command.
const roborevUsageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global: {{roborevGlobalFlags .}}. Details: roborev --help{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`
