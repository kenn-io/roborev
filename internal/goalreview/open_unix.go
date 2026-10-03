//go:build unix

package goalreview

import "golang.org/x/sys/unix"

const artifactOpenFlags = unix.O_NONBLOCK
