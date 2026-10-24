//go:build windows

package kata

import "net"

func currentUserID() uint32 { return 0 }

// validateSecureSocket cannot verify ownership or mode on Windows, so a Unix
// socket endpoint is refused rather than trusted.
func validateSecureSocket(string, uint32) error {
	return ErrPlatformSecurityLimit
}

func peerCredentialsSupported() bool { return false }

func verifyPeerCredentials(net.Conn, uint32) error { return nil }
