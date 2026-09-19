//go:build !windows

package agentconfig

import "os"

func replaceFile(staging, target string) error {
	return os.Rename(staging, target)
}
