package testutil

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

// UnjudgedRuntimeRecord uses a live foreign PID namespace so host-side
// discovery cannot prove that its process is gone.
func UnjudgedRuntimeRecord(t *testing.T, network, address string) kitdaemon.RuntimeRecord {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux PID namespaces")
	}
	identityPath := filepath.Join(t.TempDir(), "identity")
	// Read the shell's start ticks before holding it alive on stdin.
	runPIDNamespaceCommand(t, []string{"sh", "-c", `
boot=$(cat /proc/sys/kernel/random/boot_id)
namespace=$(readlink /proc/$$/ns/pid)
namespace=${namespace#pid:[}
namespace=${namespace%]}
ticks=$(awk '{print $22}' /proc/$$/stat)
printf 'linux-v1:%s:%s:%s' "$boot" "$namespace" "$ticks" > "$1"
: > "$1.ready"
read -r done || :
`, "sh", identityPath}, identityPath+".ready")
	identity, err := os.ReadFile(identityPath)
	require.NoError(t, err)
	rec := kitdaemon.RuntimeRecord{
		PID: 2, Network: network, Address: address,
		ProcessIdentityV2: kitdaemon.ProcessIdentity(strings.TrimSpace(string(identity))),
	}
	require.Equal(t, kitdaemon.ProcessScopeForeign, kitdaemon.RuntimeProcessScope(rec))
	return rec
}

// UnknownRuntimeRecord represents an identity this version cannot judge.
func UnknownRuntimeRecord(network, address string) kitdaemon.RuntimeRecord {
	return kitdaemon.RuntimeRecord{
		PID: math.MaxInt32, Network: network, Address: address,
		ProcessIdentityV2: "future-v1:unavailable",
	}
}
