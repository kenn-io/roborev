//go:build !linux

package main

import "context"

func lookupDaemonService(context.Context, int) (*daemonService, error) { return nil, nil }
