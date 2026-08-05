//go:build !windows

package agenthook

import "os"

func replaceAgentHookConfigFile(staging, target string) error {
	return os.Rename(staging, target)
}
