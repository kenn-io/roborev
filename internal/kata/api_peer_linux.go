//go:build linux

package kata

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func peerCredentialsSupported() bool { return true }

// verifyPeerCredentials requires the process on the other end of the socket to
// run as the expected user.
func verifyPeerCredentials(conn net.Conn, owner uint32) error {
	raw, ok := conn.(syscall.Conn)
	if !ok {
		return fmt.Errorf("%w: peer credentials unavailable", ErrInsecureEndpoint)
	}
	control, err := raw.SyscallConn()
	if err != nil {
		return fmt.Errorf("%w: peer credentials unavailable", ErrInsecureEndpoint)
	}
	var cred *unix.Ucred
	var sockErr error
	if err := control.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || sockErr != nil || cred == nil || cred.Uid != owner {
		return fmt.Errorf("%w: peer credentials do not match", ErrInsecureEndpoint)
	}
	return nil
}
