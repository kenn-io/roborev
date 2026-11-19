#!/bin/sh
# Test service lease contract:
# L is `<repo>-<epoch>-<pid>` and matches `^[a-z0-9][a-z0-9_-]{0,63}$`.
# Every run container carries `test-service.lease=L`; Compose runs use project `L`.
# The lease file is `$HOME/.local/state/test-service-leases/L`, created with
# `O_EXCL` in a `0700` directory as a `0600` file before any Docker resource starts.
# The producer holds `flock(LOCK_EX)` until all descendants exit. The descriptor is
# inheritable and stays open in every descendant. Cleanup runs only after an unlocked probe.
# Producers never delete lease files.
# Missing or invalid labels/files and held locks preserve resources. A scheduled
# cleaner uses the lease to reclaim run containers and project networks after unlock.
set -eu
if [ -z "${HOME:-}" ]; then
    echo 'HOME must be set to create a test service lease' >&2
    exit 2
fi
if ! command -v python3 >/dev/null 2>&1; then
    echo 'python3 is required; use make postgres-up for a manual database' >&2
    exit 2
fi
exec python3 - "$@" <<'PY'
import os
import signal
import subprocess
import sys
import time

try:
    import fcntl
except ImportError:
    sys.exit("test service leases require a platform with Python fcntl support")

name, cleanup, *command = sys.argv[1:]
lease = f"{name}-{int(time.time())}-{os.getpid()}"
directory = os.path.join(os.environ["HOME"], ".local", "state", "test-service-leases")
os.makedirs(directory, mode=0o700, exist_ok=True)
os.chmod(directory, 0o700)
fd = os.open(os.path.join(directory, lease), os.O_CREAT | os.O_EXCL | os.O_RDWR, 0o600)
os.set_inheritable(fd, True)
fcntl.flock(fd, fcntl.LOCK_EX)
os.environ["TEST_SERVICE_LEASE"] = lease
child = subprocess.Popen(command, pass_fds=(fd,))
for sig in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
    signal.signal(sig, lambda signum, frame: child.send_signal(signum) if child.poll() is None else None)
status = child.wait()
os.close(fd)
try:
    probe = open(os.path.join(directory, lease), "r")
except FileNotFoundError:
    pass  # The scheduled cleaner already reclaimed this lease.
else:
    with probe:
        try:
            fcntl.flock(probe, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            print("lease still held; scheduled cleanup will remove resources after unlock", file=sys.stderr)
        else:
            result = subprocess.run(cleanup, shell=True)
            if result.returncode:
                print("cleanup failed; scheduled cleanup will retry", file=sys.stderr)
                if status == 0:
                    status = 1
sys.exit(128 - status if status < 0 else status)
PY
