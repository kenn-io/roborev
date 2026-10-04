package main

import (
	"errors"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/requestsigning"
)

// Remote access is a separate read surface. Refuse foreground agent flows and
// lifecycle commands before they can interpret remote paths as local work.
func validateRemoteCommandFor(cmd *cobra.Command) error {
	if parent := cmd.Parent(); parent != nil && parent.Name() == "completion" && parent.Parent() == cmd.Root() {
		return validateRemoteCommand("completion")
	}
	return validateRemoteCommand(cmd.Name())
}

func validateRemoteCommand(name string) error {
	if serverAddr == "" {
		return nil
	}
	ep, err := daemon.ParseEndpoint(serverAddr)
	if err != nil {
		return err
	}
	if !ep.IsRemote() {
		return nil
	}
	switch name {
	case "list", "show", "wait", "stream", "help", "completion", "version":
		return nil
	}
	return errors.New("remote HTTPS supports list, show --job, wait and stream; other commands require a local daemon")
}

func signingInitCmd() *cobra.Command {
	var secretFile string
	cmd := &cobra.Command{Use: "signing-init", Short: "Create a private signing key file for a remote reader", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if secretFile == "" {
			return errors.New("--secret-file is required")
		}
		if err := requestsigning.GenerateKey(secretFile); err != nil {
			return err
		}
		cmd.Println("Created a private signing key. Configure a new key ID and explicit history grants before enabling the remote listener.")
		return nil
	}}
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "new private secret file (secret is never printed)")
	return cmd
}
