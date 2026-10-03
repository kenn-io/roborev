package goalreview

import (
	"regexp"
	"strings"
)

var goalHeader = regexp.MustCompile(`^(?:\*\*Goal:\*\*|Goal:)\s*(.*?)\s*$`)

func Check(snapshot Snapshot) []Finding {
	findings := []Finding{}
	for _, artifact := range snapshot.Artifacts {
		if artifact.Kind != "plan" {
			continue
		}
		goals, tasks := 0, 0
		goalLine := 1
		goalValue := ""
		numbers := map[string]bool{}
		add := func(line int, message string) {
			findings = append(findings, Finding{Severity: "medium", Message: message, Location: Location{File: artifact.Path, Line: line}})
		}
		proseLines(artifact.Content, func(line int, text string) bool {
			if task := taskHeading.FindStringSubmatch(text); task != nil {
				tasks++
				if numbers[task[1]] {
					add(line, "Duplicate task number "+task[1]+" in implementation plan.")
				}
				numbers[task[1]] = true
			}
			if tasks == 0 {
				if goal := goalHeader.FindStringSubmatch(text); goal != nil {
					goals++
					goalLine = line
					goalValue = goal[1]
				}
			}
			return true
		})
		if goals == 0 || strings.TrimSpace(goalValue) == "" {
			add(goalLine, "Implementation plan needs a usable Goal before its tasks.")
		} else if goals > 1 {
			add(goalLine, "Implementation plan declares more than one Goal.")
		}
		if tasks == 0 {
			add(1, "Implementation plan has no Task headings.")
		}
	}
	return findings
}
