//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
)

type launchdDeps struct {
	ancestors func(context.Context, int) ([]int, error)
	run       func(context.Context, ...string) ([]byte, error)
	uid       int
}

func lookupLaunchdService(ctx context.Context, pid int, deps launchdDeps) (*daemonService, error) {
	fail := func(unit string, err error) (*daemonService, error) { return nil, &managedRecoveryError{unit, err} }
	chain, err := deps.ancestors(ctx, pid)
	if err != nil {
		return fail("unknown", err)
	}
	for _, prefix := range []string{"gui/", "user/"} {
		domain := prefix + strconv.Itoa(deps.uid)
		output, err := deps.run(ctx, "print", domain)
		if err != nil {
			if exit, ok := errors.AsType[*exec.ExitError](err); ok && (exit.ExitCode() == 113 || exit.ExitCode() == 125) {
				continue
			}
			return fail(domain, err)
		}
		jobs, err := parseLaunchctlDomain(output)
		if err != nil {
			return fail(domain, err)
		}
		for _, ancestor := range chain {
			label, ok := jobs[ancestor]
			if !ok {
				continue
			}
			unit := domain + "/" + label
			output, err := deps.run(ctx, "print", unit)
			if err != nil {
				return fail(unit, err)
			}
			owner, jobType, err := parseLaunchctlService(output)
			if err != nil {
				return fail(unit, err)
			}
			if owner.mainPID != ancestor {
				return fail(unit, errors.New("launchd job pid changed"))
			}
			if jobType != "LaunchAgent" {
				continue
			}
			owner.unit = unit
			return owner, nil
		}
	}
	return nil, nil
}
