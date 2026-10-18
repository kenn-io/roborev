//go:build !linux

package main

import "context"

func lookupDaemonService(context.Context, int) (*daemonService, error) { return nil, nil }
func startDaemonService(context.Context, string) error                 { return nil }
