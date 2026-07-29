package agenthook

import kitagenthook "go.kenn.io/kit/agenthook"

func Installed(agent kitagenthook.Agent, path string) (bool, error) {
	result, err := kitagenthook.PlanUninstall(agent, path, agentHookMarker)
	if err != nil {
		return false, err
	}
	return result.Changed, nil
}
