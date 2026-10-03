package main

import (
	"errors"
	"fmt"
	"os"

	"go.kenn.io/roborev/internal/config"
)

func checkDoctorGlobalConfig(env *doctorEnv) []doctorCheck {
	c := doctorCheck{ID: "config.global", Category: "config"}
	_, statErr := os.Stat(env.globalPath)
	switch {
	case env.globalErr != nil:
		c.Status = doctorFail
		c.Summary = "global config does not load; roborev falls back to defaults or refuses to run"
		c.Details = []string{env.globalPath, env.globalErr.Error()}
		c.Fix = "fix the error, then run 'roborev config validate --global'"
		return []doctorCheck{c}
	case errors.Is(statErr, os.ErrNotExist):
		c.Status = doctorOK
		c.Summary = "no global config file; using defaults"
	default:
		err := env.global.Validate()
		if err == nil {
			err = config.ValidateExperimentConfigs(env.global, nil, nil)
		}
		if err != nil {
			c.Status = doctorFail
			c.Summary = "global config has invalid values"
			c.Details = []string{env.globalPath, err.Error()}
			c.Fix = "fix the value, then run 'roborev config validate --global'"
		} else {
			c.Status = doctorOK
			c.Summary = "global config is valid (" + env.globalPath + ")"
		}
	}
	out := []doctorCheck{c}
	if unknown, err := config.UnknownGlobalKeys(env.globalPath); err == nil && len(unknown) > 0 {
		out = append(out, unknownKeysCheck("config.global_unknown_keys", env.globalPath, unknown))
	}
	return out
}

func unknownKeysCheck(id, path string, keys []string) doctorCheck {
	return doctorCheck{
		ID: id, Category: "config", Status: doctorWarn,
		Summary: fmt.Sprintf("%s in %s %s ignored", plural(len(keys), "unknown key"), path,
			map[bool]string{true: "is", false: "are"}[len(keys) == 1]),
		Details: keys,
		Fix:     "check the spelling against the configuration docs; roborev silently ignores keys it does not know",
	}
}

func checkDoctorRepoConfig(env *doctorEnv) []doctorCheck {
	if env.repoPath == "" {
		return []doctorCheck{{
			ID: "config.repo", Category: "config", Status: doctorInfo,
			Summary: "not in a git repository; repository checks skipped",
			Fix:     "run 'roborev doctor' inside a repository, or pass --repo",
		}}
	}
	path := config.RepoConfigPath(env.repoPath)
	c := doctorCheck{ID: "config.repo", Category: "config"}
	switch {
	case env.repoErr != nil:
		c.Status = doctorFail
		c.Summary = "repository .roborev.toml does not load"
		c.Details = []string{path, env.repoErr.Error()}
		c.Fix = "fix the error, then run 'roborev config validate --local'"
		return []doctorCheck{c}
	case env.repoCfg == nil:
		c.Status = doctorOK
		c.Summary = "no .roborev.toml; this repository uses global settings"
	default:
		err := config.ValidateEffectiveReviewConfig(env.global, env.repoCfg)
		if err == nil {
			err = config.ValidateExperimentConfigs(env.global, env.repoCfg, env.repoRaw)
		}
		if err != nil {
			c.Status = doctorFail
			c.Summary = "repository config is invalid when merged with global config"
			c.Details = []string{path, err.Error()}
			c.Fix = "fix the value, then run 'roborev config validate'"
		} else {
			c.Status = doctorOK
			c.Summary = "repository config is valid (" + path + ")"
		}
	}
	out := []doctorCheck{c}
	if env.repoCfg != nil {
		if unknown, err := config.UnknownRepoKeys(env.repoPath); err == nil && len(unknown) > 0 {
			out = append(out, unknownKeysCheck("config.repo_unknown_keys", path, unknown))
		}
	}
	if check, ok := checkDoctorDefaultBranchConfig(env); ok {
		out = append(out, check)
	}
	return out
}

// checkDoctorDefaultBranchConfig checks .roborev.toml on the default branch,
// which is where reviews read repository config and guidelines from.
func checkDoctorDefaultBranchConfig(env *doctorEnv) (doctorCheck, bool) {
	if env.repoBranch == "" {
		return doctorCheck{}, false
	}
	_, err := config.LoadRepoConfigFromRef(env.repoPath, env.repoBranch)
	if err == nil {
		return doctorCheck{}, false
	}
	c := doctorCheck{ID: "config.repo_default_branch", Category: "config", Details: []string{err.Error()}}
	if config.IsConfigParseError(err) || config.IsExperimentConfigError(err) {
		c.Status = doctorFail
		c.Summary = fmt.Sprintf(".roborev.toml on %s is invalid", env.repoBranch)
		c.Details = append(c.Details, "reviews read repository config from this branch")
		c.Fix = fmt.Sprintf("fix .roborev.toml and merge the fix into %s", env.repoBranch)
		return c, true
	}
	c.Status = doctorWarn
	c.Summary = fmt.Sprintf("could not read .roborev.toml from %s", env.repoBranch)
	c.Fix = "check that the repository is readable with 'git show " + env.repoBranch + ":.roborev.toml'"
	return c, true
}
