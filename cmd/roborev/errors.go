package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func helpHint(cmd *cobra.Command) string {
	return fmt.Sprintf("run '%s --help' for usage and examples", cmd.CommandPath())
}

func commandError(cmd *cobra.Command, err error, code int, kind string) error {
	if err == nil {
		return nil
	}
	if exit, ok := errors.AsType[*exitError](err); ok {
		if exit.cause == nil || exit.silent || exit.hint != "" {
			return err
		}
		return &exitError{code: exit.code, cause: err, kind: exit.kind, hint: helpHint(cmd)}
	}
	return &exitError{code: code, cause: err, kind: kind, hint: helpHint(cmd)}
}

func validationErr(cmd *cobra.Command, err error) error {
	return commandError(cmd, err, 4, "validation")
}

func invalidValue(err error) error {
	return &exitError{code: 4, cause: err, kind: "validation"}
}

func withHint(err error, hint string) error {
	if err == nil {
		return nil
	}
	result := &exitError{code: 1, cause: err, kind: "runtime", hint: hint}
	if exit, ok := errors.AsType[*exitError](err); ok {
		if exit.cause == nil || exit.silent || exit.hint != "" {
			return err
		}
		result.code, result.kind = exit.code, exit.kind
	}
	return result
}

func isCLIHook(cmd *cobra.Command) bool {
	switch cmd.CommandPath() {
	case "roborev post-commit", "roborev agent-hook run", "roborev agent-hook fix-done":
		return true
	}
	return false
}

// configureCLIErrors wraps invocation validation, not command implementations.
// Runtime errors are given a fallback hint by the single renderer below.
func configureCLIErrors(root *cobra.Command) {
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if isCLIHook(cmd) {
			return err
		}
		return usageErr(cmd, err)
	})
	helpGroups := make(map[*cobra.Command]bool)
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if !isCLIHook(cmd) {
			if cmd.Parent() == root && cmd.Name() == "help" {
				cmd.Run = nil
				cmd.RunE = func(cmd *cobra.Command, args []string) error {
					target, rest, err := root.Find(args)
					if target == nil {
						target = root
					}
					if err != nil {
						return usageErr(target, err)
					}
					if len(rest) != 0 {
						return usageErr(target, fmt.Errorf("unknown command %q for %q", rest[0], target.CommandPath()))
					}
					target.InitDefaultHelpFlag()
					return target.Help()
				}
			}
			// Cobra otherwise treats a misspelled group child as successful help.
			if cmd != root && !cmd.Runnable() && cmd.HasSubCommands() {
				helpGroups[cmd] = true
				cmd.Args = func(cmd *cobra.Command, args []string) error {
					if len(args) != 0 {
						return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
					}
					return nil
				}
				cmd.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
			}
			validate := cmd.Args
			if validate != nil {
				cmd.Args = func(cmd *cobra.Command, args []string) error { return usageErr(cmd, validate(cmd, args)) }
			}
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
	original := root.PersistentPreRunE
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if helpGroups[cmd] {
			return nil
		}
		if !isCLIHook(cmd) {
			if err := cmd.ValidateRequiredFlags(); err != nil {
				return usageErr(cmd, err)
			}
			if err := cmd.ValidateFlagGroups(); err != nil {
				return usageErr(cmd, err)
			}
		}
		if original != nil {
			return original(cmd, args)
		}
		return nil
	}
}

// executeCLI is also the integration-test entry point, including real Cobra
// parsing, output routing and the exit status used by main.
func executeCLI(root *cobra.Command, args []string) int {
	root.SetArgs(args)
	selected, _, findErr := root.Find(args)
	if selected == nil {
		selected = root
	}
	if findErr != nil {
		return renderCLIError(selected, args, usageErr(selected, findErr))
	}
	var diagnostics bytes.Buffer
	if cliBoolFlag(selected, args, "json") {
		oldLifecycle, oldWarning := lifecycleOut, cliWarningOut
		lifecycleOut, cliWarningOut = &diagnostics, &diagnostics
		defer func() { lifecycleOut, cliWarningOut = oldLifecycle, oldWarning }()
	}
	cmd, err := root.ExecuteC()
	if cmd == nil {
		cmd = selected
	}
	if err == nil {
		_, _ = io.Copy(root.ErrOrStderr(), &diagnostics)
		return 0
	}
	return renderCLIError(cmd, args, err)
}

func renderCLIError(cmd *cobra.Command, args []string, err error) int {
	if err == nil {
		return 0
	}
	code, kind, hint := 1, "runtime", helpHint(cmd)
	if exit, ok := errors.AsType[*exitError](err); ok {
		code = exit.code
		if exit.cause == nil || exit.silent {
			return code
		}
		if exit.kind != "" {
			kind = exit.kind
		}
		if exit.hint != "" {
			hint = exit.hint
		}
	}
	if isCLIHook(cmd) {
		// These hook failures were brief Cobra diagnostics before shared
		// rendering. Only post-commit suppresses its returned errors.
		if cmd.CommandPath() != "roborev post-commit" {
			fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
		}
		return code
	}
	quiet := cliBoolFlag(cmd, args, "quiet")
	if quiet {
		hint = ""
	}
	if cliBoolFlag(cmd, args, "json") {
		envelope := struct {
			Error struct {
				Code     string `json:"code"`
				Message  string `json:"message"`
				Hint     string `json:"hint"`
				ExitCode int    `json:"exit_code"`
			} `json:"error"`
		}{}
		envelope.Error.Code, envelope.Error.Message = kind, strings.ToValidUTF8(err.Error(), "\uFFFD")
		envelope.Error.Hint, envelope.Error.ExitCode = strings.ToValidUTF8(hint, "\uFFFD"), code
		_ = json.MarshalWrite(cmd.ErrOrStderr(), envelope)
		fmt.Fprintln(cmd.ErrOrStderr())
		return code
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
	if hint != "" {
		label := "hint:"
		if cliBoolFlag(cmd, args, "agent") {
			label = "Hint:"
		}
		fmt.Fprintln(cmd.ErrOrStderr(), label, hint)
	}
	return code
}

// cliBoolFlag also reads selectors after a flag-parse failure. Only flags
// actually owned by this command count: provider --agent strings are not modes.
// Respect -- and values of other flags so a prompt "--json" is still a prompt.
func cliBoolFlag(cmd *cobra.Command, args []string, name string) bool {
	lookup := func(name string) *pflag.Flag {
		if flag := cmd.Flags().Lookup(name); flag != nil {
			return flag
		}
		return cmd.InheritedFlags().Lookup(name)
	}
	flag := lookup(name)
	if flag == nil || flag.Value.Type() != "bool" {
		return false
	}
	value, _ := strconv.ParseBool(flag.DefValue)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "--") {
			key, raw, hasValue := strings.Cut(arg[2:], "=")
			f := lookup(key)
			if f == nil {
				continue
			}
			if key == name {
				if !hasValue {
					value = true
				} else if parsed, err := strconv.ParseBool(raw); err == nil {
					value = parsed
				}
			}
			if !hasValue && f.NoOptDefVal == "" {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			for j := 1; j < len(arg); j++ {
				f := cmd.Flags().ShorthandLookup(string(arg[j]))
				if f == nil {
					f = cmd.InheritedFlags().ShorthandLookup(string(arg[j]))
				}
				if f == nil {
					continue
				}
				if f.Name == name {
					value = true
					if j+1 < len(arg) && arg[j+1] == '=' {
						if parsed, err := strconv.ParseBool(arg[j+2:]); err == nil {
							value = parsed
						}
						break
					}
				}
				if f.NoOptDefVal == "" {
					if j == len(arg)-1 {
						i++
					}
					break
				}
			}
		}
	}
	return value
}
