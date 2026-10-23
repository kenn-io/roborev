//go:build !linux && !darwin && !windows

package kata

import "net"

func peerCredentialsSupported() bool { return false }

func verifyPeerCredentials(net.Conn, uint32) error { return nil }
