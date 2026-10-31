package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func walkVisibleHelp(cmd *cobra.Command, visit func(*cobra.Command)) {
	if cmd.Hidden {
		return
	}
	visit(cmd)
	for _, child := range cmd.Commands() {
		walkVisibleHelp(child, visit)
	}
}

func TestHelpCoverage(t *testing.T) {
	assert := assert.New(t)
	root := newRootCmd()
	walkVisibleHelp(root, func(cmd *cobra.Command) {
		assert.NotEmpty(cmd.Long, "%s needs conceptual help", cmd.CommandPath())
		assert.NotEqual(cmd.Short, cmd.Long, "%s needs more than its summary", cmd.CommandPath())
		assert.NotEmpty(cmd.Example, "%s needs a runnable example", cmd.CommandPath())
		examples, err := advertisedCommands(helpShellLines(cmd.Example, true))
		assert.NoError(err, "%s example shell syntax", cmd.CommandPath())
		assert.NotEmpty(examples, "%s needs an executable example", cmd.CommandPath())
		assert.NotContains(cmd.Long, "\nExamples:", "%s should use Cobra Example", cmd.CommandPath())
		if cmd.Parent() == root {
			assert.NotEmpty(cmd.GroupID, "%s needs a group", cmd.CommandPath())
		}
	})
	for _, alias := range []string{"ls", "resolve"} {
		cmd, _, err := root.Find([]string{alias})
		require.NoError(t, err)
		assert.NotSame(root, cmd)
	}
}

func TestHelpCompactInheritedFlags(t *testing.T) {
	assert := assert.New(t)
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"show", "--help"})
	require.NoError(t, root.Execute())
	assert.Contains(out.String(), "Global: --server <string>, -v/--verbose. Details: roborev --help")
	assert.NotContains(out.String(), "daemon address (loopback")
	assert.NotContains(out.String(), "--from-skill")
	out.Reset()
	root = newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	require.NoError(t, root.Execute())
	assert.Contains(out.String(), "daemon address (loopback")
	for _, group := range []string{"Review code:", "Fix findings:", "Set up and diagnose:", "Manage daemon and data:", "Maintain roborev:"} {
		assert.Contains(out.String(), group)
	}
}

// advertisedCommands tokenizes documented shell snippets without starting a
// shell. Quoted words, placeholders, comments, pipelines, and continuations are
// preserved; only roborev invocations are returned. No expansion is performed.
func advertisedCommands(source string) ([][]string, error) {
	var commands [][]string
	var words []string
	var word strings.Builder
	var quote rune
	started, comment, escaped := false, false, false
	flushWord := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}
	flushCommand := func() {
		flushWord()
		if len(words) > 0 && words[0] == "roborev" {
			commands = append(commands, words[1:])
		}
		words = nil
	}
	runes := []rune(source)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if comment {
			if r == '\n' {
				comment = false
				flushCommand()
			}
			continue
		}
		if escaped {
			if r != '\n' {
				word.WriteRune(r)
				started = true
			}
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			started = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if r == '#' && !started {
			comment = true
			continue
		}
		if r == '<' && !started { // Angle-bracket placeholders are argument text, not redirections.
			end := i + 1
			for end < len(runes) && runes[end] != '>' && !unicode.IsSpace(runes[end]) {
				end++
			}
			if end >= len(runes) || runes[end] != '>' {
				return nil, fmt.Errorf("unsupported shell redirection")
			}
			word.WriteString(string(runes[i : end+1]))
			started = true
			i = end
			continue
		}
		if r == '\n' || r == ';' || r == '|' || r == '&' {
			flushCommand()
			continue
		}
		if unicode.IsSpace(r) {
			flushWord()
			continue
		}
		word.WriteRune(r)
		started = true
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("unterminated shell quote or escape")
	}
	flushCommand()
	return commands, nil
}

func validateHelpInvocation(args []string) error {
	root := newRootCmd()
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	cmd, tail, err := root.Find(args)
	if err != nil {
		return err
	}
	if cmd == root && len(tail) > 0 && !strings.HasPrefix(tail[0], "-") {
		return fmt.Errorf("unknown command %q", tail[0])
	}
	cmd.InitDefaultHelpFlag()
	if err = cmd.ParseFlags(tail); err != nil {
		return err
	}
	positional := cmd.Flags().Args()
	if err = cmd.ValidateArgs(positional); err != nil {
		return err
	}
	if err = cmd.ValidateRequiredFlags(); err != nil {
		return err
	}
	return cmd.ValidateFlagGroups()
}

func shellFences(source string) string {
	var result strings.Builder
	active := false
	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if active {
				active = false
			} else {
				language := strings.TrimPrefix(trimmed, "```")
				active = language == "bash" || language == "sh" || language == "shell"
			}
			continue
		}
		if active {
			result.WriteString(trimmed)
			result.WriteByte('\n')
		}
	}
	return result.String()
}

// Descriptions may contain ordinary prose and quoted command names. Only
// shell lines advertise executable snippets; preserve their continuation lines.
func helpShellLines(source string, unindented bool) string {
	var out strings.Builder
	continuation, previousCommand := false, false
	previous := ""
	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimSpace(line)
		candidate := strings.HasPrefix(trimmed, "roborev ") || trimmed == "roborev" || strings.Contains(trimmed, "| roborev ")
		boundary := previous == "" || strings.HasSuffix(previous, ":") || strings.HasPrefix(previous, "#") || previousCommand
		isCommand := continuation || (candidate && (unindented || (line != trimmed && boundary)))
		if isCommand {
			out.WriteString(trimmed)
			out.WriteByte('\n')
		}
		continuation = isCommand && strings.HasSuffix(trimmed, "\\")
		previousCommand = isCommand
		previous = trimmed
	}
	return out.String()
}

func TestHelpAdvertisedCommands(t *testing.T) {
	sources := map[string]string{}
	walkVisibleHelp(newRootCmd(), func(cmd *cobra.Command) {
		sources[cmd.CommandPath()] = helpShellLines(cmd.Long, false) + helpShellLines(cmd.Example, true)
	})
	for _, path := range []string{"quickstart_guide.md", "../../docs/commands.md"} {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		sources[path] = shellFences(string(data))
	}
	for name, source := range sources {
		t.Run(name, func(t *testing.T) {
			commands, err := advertisedCommands(source)
			require.NoError(t, err)
			for _, args := range commands {
				assert.NoError(t, validateHelpInvocation(args), "roborev %s", strings.Join(args, " "))
			}
		})
	}
}

func TestHelpShellParsing(t *testing.T) {
	assert := assert.New(t)
	snippets := []string{
		"roborev comment 42 'fixed the race' # explanation\nroborev show --job 42",
		"roborev list --json | jq '.jobs'; roborev show \\\n --job 42",
		"cat checklist.txt | roborev run --wait; roborev show <job_id>",
		"roborev show <job_id|sha>",
	}
	expected := [][][]string{
		{{"comment", "42", "fixed the race"}, {"show", "--job", "42"}},
		{{"list", "--json"}, {"show", "--job", "42"}},
		{{"run", "--wait"}, {"show", "<job_id>"}},
		{{"show", "<job_id|sha>"}},
	}
	for i, source := range snippets {
		got, err := advertisedCommands(source)
		require.NoError(t, err)
		assert.Equal(expected[i], got)
	}
	for _, source := range []string{"roborev search 'broken", "roborev show \\"} {
		_, err := advertisedCommands(source)
		assert.Error(err)
	}
	for _, args := range [][]string{{"missing"}, {"config", "missing"}, {"show", "--missing"}, {"close"}, {"cancel", "42", "43"}, {"review", "--wait", "--missing"}, {"ci", "review", "--gh-repo", "example/repo", "--gl-repo", "example/repo"}} {
		assert.Error(validateHelpInvocation(args), "%v", args)
	}
}

func TestHelpPreservesConceptsAroundExamples(t *testing.T) {
	assert := assert.New(t)
	root := &cobra.Command{Use: "roborev"}
	review := &cobra.Command{
		Use: "review", Short: "Review code",
		Long: "Explain the existing panel policy.\nExamples:\n  # A specific revision\n  roborev review HEAD\n\nPanel selection remains configured per repository.\n\n  roborev review --branch",
	}
	root.AddCommand(review)
	applyRootHelp(root)
	assert.Contains(review.Long, "Explain the existing panel policy.")
	assert.Contains(review.Long, "Panel selection remains configured per repository.")
	assert.Contains(review.Long, "The daemon records a job ID")
	assert.NotContains(review.Example, "Panel selection")
	assert.Contains(review.Example, "# A specific revision")
	assert.Contains(review.Example, "roborev review HEAD")
	assert.Contains(review.Example, "roborev review --branch")
}

func TestHelpShellLinesSeparatesProseFromCode(t *testing.T) {
	source := "Repository context:\n  After renaming a directory,\n  roborev creates a new entry.\n\n  Use merge to combine them:\n    roborev repo merge old new\n\nWorkflow:\n  roborev doctor\n  roborev show --job 42"
	assert.Equal(t, "roborev repo merge old new\nroborev doctor\nroborev show --job 42\n", helpShellLines(source, false))
}
