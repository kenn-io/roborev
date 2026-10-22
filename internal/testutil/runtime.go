package testutil

import (
	"math"

	kitdaemon "go.kenn.io/kit/daemon"
)

// UnjudgedRuntimeRecord describes a process whose identity the caller cannot judge.
func UnjudgedRuntimeRecord(network, address string) kitdaemon.RuntimeRecord {
	return kitdaemon.RuntimeRecord{
		PID: math.MaxInt32, Network: network, Address: address,
		ProcessIdentityV2: "future-v1:unavailable",
	}
}
