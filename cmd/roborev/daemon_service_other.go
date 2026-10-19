//go:build !linux

package main

import (
	"context"
	"errors"
)

func lookupDaemonService(context.Context, int) (*daemonService, error) { return nil, nil }

func userManagerAvailable(context.Context) bool { return false }

func userServiceMainPID(context.Context, string) (int, error) { return 0, errors.ErrUnsupported }

func startUserService(context.Context, string) error { return errors.ErrUnsupported }
