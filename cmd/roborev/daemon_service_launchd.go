package main

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"unicode"
)

// parseLaunchctlDomain reads running jobs from the domain's services table.
func parseLaunchctlDomain(output []byte) (map[int]string, error) {
	jobs := map[int]string{}
	inServices := false
	for line := range strings.SplitSeq(string(output), "\n") {
		if line == "\tservices = {" {
			inServices = true
			continue
		}
		if !inServices {
			continue
		}
		if line == "\t}" {
			return jobs, nil
		}
		fields := strings.Fields(line)
		if !strings.HasPrefix(line, "\t\t") || len(fields) < 3 {
			return nil, errors.New("cannot read launchd jobs")
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || strings.ContainsAny(fields[0], "+-") {
			return nil, errors.New("cannot read launchd jobs")
		}
		if pid > 0 {
			label := strings.TrimSpace(line)
			label = strings.TrimSpace(label[len(fields[0]):])
			jobs[pid] = strings.TrimSpace(label[len(fields[1]):])
		}
	}
	return nil, errors.New("cannot read launchd jobs")
}

// parseLaunchctlService reads job fields indented by exactly one tab followed by a non-whitespace character.
func parseLaunchctlService(output []byte) (*daemonService, string, error) {
	service := &daemonService{serviceType: "simple"}
	jobType := ""
	for line := range strings.SplitSeq(string(output), "\n") {
		field, ok := strings.CutPrefix(line, "\t")
		if !ok || field == "" || strings.TrimLeftFunc(field, unicode.IsSpace) != field {
			continue
		}
		key, value, _ := strings.Cut(strings.TrimSpace(line), " = ")
		switch key {
		case "type":
			jobType = value
		case "program":
			service.executable = value
		case "pid":
			service.mainPID, _ = strconv.Atoi(value)
		}
	}
	if jobType == "" {
		return nil, "", errors.New("launchd job type unavailable")
	}
	return service, jobType, nil
}

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
