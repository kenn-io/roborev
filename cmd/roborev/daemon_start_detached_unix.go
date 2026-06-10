//go:build !windows

package main

import (
	"context"

	kitdaemon "go.kenn.io/kit/daemon"
)

func startDetachedDaemon(ctx context.Context, opts detachedDaemonOptions) error {
	return kitdaemon.StartDetached(ctx, kitdaemon.StartDetachedOptions{
		Executable:      opts.Executable,
		Args:            opts.Args,
		Env:             opts.Env,
		Stdout:          opts.Stdout,
		Stderr:          opts.Stderr,
		RefuseEphemeral: opts.RefuseEphemeral,
	})
}
