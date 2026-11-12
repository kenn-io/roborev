package main

import "go.kenn.io/roborev/internal/config"

func checkDoctorKataRubrics(env *doctorEnv) []doctorCheck {
	if env.repoPath == "" {
		return nil
	}
	result := doctorCheck{ID: "integrations.kata_rubrics", Category: "integrations"}
	if env.daemonAgents == nil || env.daemonAgents.RubricSetup == nil {
		mode := config.ResolveKataRubricsMode(env.repoCfg, env.global)
		if mode == "off" {
			result.Status, result.Summary = doctorInfo, "Kata rubric archiving: mode off"
			return []doctorCheck{result}
		}
		result.Status, result.Summary = doctorInfo, "Kata rubric archiving: mode "+mode+", daemon setup unverified"
		result.Details = []string{"Vault rubrics are readable by all issue readers and metadata writers."}
		result.Fix = "start or restart the roborev daemon to verify its Kata setup"
		return []doctorCheck{result}
	}
	probe := env.daemonAgents.RubricSetup
	prefix := "Kata rubric archiving: mode " + probe.Mode
	result.Details = []string{"Vault rubrics are readable by all issue readers and metadata writers."}
	switch probe.State {
	case "off":
		result.Status, result.Summary = doctorInfo, prefix
		result.Details = nil
	case "kata_missing":
		result.Status, result.Summary, result.Fix = doctorFail, prefix+", kata CLI unavailable on daemon PATH", "install kata on the daemon's PATH and restart the daemon"
	case "binding_missing":
		result.Status, result.Summary, result.Fix = doctorFail, prefix+", project binding missing", "bind this repository to a Kata project"
	case "binding_error":
		result.Status, result.Summary, result.Fix = doctorFail, prefix+", project binding invalid", "repair the Kata binding configuration"
	case "missing":
		result.Status, result.Summary, result.Fix = doctorFail, prefix+", Docbank credential missing", "check kata docbank setup and the daemon's credential"
	case "ok":
		result.Status, result.Summary = doctorOK, prefix+", Docbank key resolves for "+probe.Project+"; vault rubrics are readable by all issue readers and metadata writers"
	default:
		result.Status, result.Summary = doctorInfo, prefix+", Docbank state unknown"
	}
	if probe.Project != "" && probe.State != "ok" {
		result.Details = append(result.Details, "Kata project: "+probe.Project)
	}
	if probe.Error != "" {
		result.Details = append(result.Details, probe.Error)
	}
	return []doctorCheck{result}
}
