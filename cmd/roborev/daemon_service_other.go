//go:build !linux && !darwin

package main

import (
	"context"
	"errors"
	"os"
)

func lookupDaemonService(context.Context, int) (*daemonService, error) { return nil, nil }

func processServiceUnit(int) (string, error) { return "", nil }

func userManagerAvailable(context.Context) bool { return false }

func userServiceMainPID(context.Context, string) (int, error) { return 0, errors.ErrUnsupported }

func startUserService(context.Context, string) error { return errors.ErrUnsupported }

func serviceDescription(unit string) string { return "service " + unit }
func serviceRestartHint(unit string) string { return "restart service " + unit }
func serviceStartHint(unit string) string   { return "start service " + unit }
func serviceStopHint(string) string         { return "" }
func servicePermissionError(string) error   { return nil }
func currentExecutable() (string, error)    { return os.Executable() }
