package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (j Job) check(ctx context.Context, wrapper, env []string) error {
	canary, err := os.MkdirTemp("", "roborev-sandbox-canary-*")
	if err != nil {
		return fmt.Errorf("sandbox self-check setup: %w", err)
	}
	defer os.RemoveAll(canary)
	secret := filepath.Join(canary, "secret")
	if err := os.WriteFile(secret, []byte("canary"), 0o600); err != nil {
		return fmt.Errorf("sandbox self-check setup: %w", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("sandbox self-check setup: %w", err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return fmt.Errorf("sandbox self-check setup: %w", err)
	}
	const probe = `
failed=0
bad() { echo "$1"; failed=1; }
touch "$1/.sandbox-write-$$" && rm "$1/.sandbox-write-$$" || bad workdir-write
cat "$2" >/dev/null 2>&1 && bad canary-read
( : >/dev/tcp/127.0.0.1/"$3" ) 2>/dev/null && bad loopback-connect
( : >/dev/tcp/1.1.1.1/443 ) 2>/dev/null && bad external-connect
/usr/bin/python3 -c 'import urllib.request,urllib.error,sys
try:
 urllib.request.urlopen("https://example.com", timeout=5)
except urllib.error.HTTPError:
 pass
except Exception:
 sys.exit(1)' 2>/dev/null && bad proxy-external
[[ ! -v ROBOREV_SANDBOX_CANARY ]] || bad environment-canary
[[ ! -v XDG_RUNTIME_DIR ]] || bad environment-runtime
[[ ! -v DBUS_SESSION_BUS_ADDRESS ]] || bad environment-dbus
/usr/bin/python3 -c 'import socket' 2>/dev/null || bad dbus-probe
/usr/bin/python3 -c 'import socket,sys; s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1])' "$4" 2>/dev/null && bad dbus-connect
shift 4
for path in "$@"; do
  if [ -d "$path" ]; then
    ( touch "$path/.sandbox-write-$$" ) 2>/dev/null && { rm -f "$path/.sandbox-write-$$"; bad "read-path-write: $path"; }
  fi
done
exit "$failed"
`
	args := append([]string{}, wrapper[1:]...)
	args = append(args, "bash", "--norc", "--noprofile", "-c", probe, "sandbox-check", j.WorkDir, secret, port, filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "bus"))
	args = append(args, j.ReadPaths...)
	cmd := exec.CommandContext(ctx, wrapper[0], args...)
	cmd.Dir = j.WorkDir
	cmd.Env = append(append([]string{}, env...), "ROBOREV_SANDBOX_CANARY=planted")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sandbox self-check failed: %s: %w", strings.TrimSpace(string(output)), err)
	}
	if names := strings.TrimSpace(string(output)); names != "" {
		return fmt.Errorf("sandbox self-check failed: %s", names)
	}
	return nil
}
