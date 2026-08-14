package main

import (
	"os/exec"
	"runtime"
	"time"
)

const browserOpenerFailureWindow = 250 * time.Millisecond

var (
	browserGOOS         = runtime.GOOS
	startBrowserCommand = func(name string, args ...string) (func() error, error) {
		command := exec.Command(name, args...)
		if err := command.Start(); err != nil {
			return nil, err
		}
		return command.Wait, nil
	}
	openBrowserURL = platformOpenBrowserURL
)

func platformOpenBrowserURL(target string) error {
	var name string
	var args []string
	switch browserGOOS {
	case "darwin":
		name, args = "open", []string{target}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		name, args = "xdg-open", []string{target}
	}

	wait, err := startBrowserCommand(name, args...)
	if err != nil {
		return err
	}
	result := make(chan error, 1)
	go func() { result <- wait() }()
	select {
	case err := <-result:
		return err
	case <-time.After(browserOpenerFailureWindow):
		return nil
	}
}
