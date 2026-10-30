package main

import (
	"log"
	"os"
	"strings"

	"go.kenn.io/roborev/internal/procutil"
)

// extendDaemonPATH lets the daemon find agents installed in the user's tool
// directories even when the process that started it had a minimal PATH, such
// as a systemd or launchd service or another program's supervisor. Agents
// inherit the extended PATH too.
func extendDaemonPATH() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path, added := procutil.AppendPathDirs(os.Getenv("PATH"), procutil.UserToolDirs(home, os.Getenv))
	if len(added) == 0 {
		return
	}
	if err := os.Setenv("PATH", path); err != nil {
		log.Printf("Warning: could not add user tool directories to PATH: %v", err)
		return
	}
	log.Printf("Added user tool directories to PATH: %s", strings.Join(added, string(os.PathListSeparator)))
}
