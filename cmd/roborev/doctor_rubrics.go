package main

import (
	"context"
	"os/exec"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
)

// probeRubricKata uses Kata's read-only health command, which does not start a daemon.
func probeRubricKata(ctx context.Context, repo string) (kata.Binding, kata.DocbankHealth, error, error, error) {
	if _, err := exec.LookPath("kata"); err != nil {
		return kata.Binding{}, kata.DocbankHealth{}, err, nil, nil
	}
	if repo == "" {
		return kata.Binding{}, kata.DocbankHealth{}, nil, kata.ErrNoBinding, nil
	}
	c := kata.NewCLIClient(repo)
	binding, err := c.Binding(ctx)
	if err != nil {
		return kata.Binding{}, kata.DocbankHealth{}, nil, err, nil
	}
	// Match the existing doctor daemon probe's two-second deadline.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	health, err := c.DocbankHealth(ctx)
	return binding, health, nil, nil, err
}

func checkDoctorKataRubrics(env *doctorEnv) []doctorCheck {
	result := doctorCheck{ID: "integrations.kata_rubrics", Category: "integrations"}
	if config.ResolveKataRubricsMode(env.repoCfg, env.global) == "off" {
		result.Status, result.Summary = doctorInfo, "Kata rubric archiving is off"
		return []doctorCheck{result}
	}
	probe := env.rubricKata
	if probe == nil {
		probe = probeRubricKata
	}
	binding, health, cliErr, bindingErr, healthErr := probe(env.ctx, env.repoPath)
	switch {
	case cliErr != nil:
		result.Status, result.Summary, result.Fix = doctorFail, "Kata rubric archiving: kata CLI not found on this shell PATH", "install kata on PATH"
	case bindingErr != nil:
		result.Status, result.Summary, result.Fix = doctorFail, "Kata rubric archiving: project binding unavailable", "bind this repository to a Kata project"
	case healthErr != nil || (health.Credential != "ok" && health.Credential != "missing"):
		result.Status, result.Summary, result.Fix = doctorInfo, "Kata rubric archiving: Docbank state unknown", "check Kata daemon health and Docbank setup"
	case health.Credential == "missing":
		result.Status, result.Summary, result.Fix = doctorFail, "Kata rubric archiving: Docbank credential unavailable", "check kata docbank setup and the daemon's credential"
	default:
		result.Status, result.Summary = doctorOK, "Kata rubric archiving: Docbank key resolves for "+binding.Project
	}
	if result.Status != doctorOK {
		result.Details = []string{"Mode: current. Vault rubrics are readable by all issue readers and metadata writers."}
	}
	return []doctorCheck{result}
}
