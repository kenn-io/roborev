package testutil

import (
	"math"

	kitdaemon "go.kenn.io/kit/daemon"
)

// UnjudgedRuntimeRecord is a stand-in for a record from another PID namespace.
// Its malformed identity and nonexistent default PID force an unjudged result;
// production does not write these values. Callers may override the PID.
func UnjudgedRuntimeRecord(network, address string) kitdaemon.RuntimeRecord {
	return kitdaemon.RuntimeRecord{
		PID: math.MaxInt32, Network: network, Address: address,
		ProcessIdentityV2: "future-v1:unavailable",
	}
}
