//go:build windows

package agenthook

import "golang.org/x/sys/windows"

func replaceAgentHookConfigFile(staging, target string) error {
	stagingPath, err := windows.UTF16PtrFromString(staging)
	if err != nil {
		return err
	}
	targetPath, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		stagingPath,
		targetPath,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}
