package main

import (
	"errors"
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
