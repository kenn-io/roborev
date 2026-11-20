#!/bin/sh
# Test service lease contract:
# L is `<repo>-<epoch>-<pid>` and matches `^[a-z0-9][a-z0-9_-]{0,63}$`.
# Every run container carries `test-service.lease=L`; Compose runs use project `L`.
# The lease file is `$HOME/.local/state/test-service-leases/L`, created with
# `O_EXCL` in a `0700` directory as a `0600` file before any Docker resource starts.
# The producer holds `flock(LOCK_EX)` until all descendants exit. The descriptor is
# inheritable and stays open in every descendant.
# A Compose producer deletes its lease after successful cleanup while holding its lock.
# Missing or invalid labels/files and held locks preserve resources. A scheduled
# cleaner uses the lease to reclaim run containers and project networks after unlock.
set -eu
if [ -z "${HOME:-}" ]; then
    echo 'HOME must be set to create a test service lease' >&2
    exit 2
fi
exec python3 -c '
import fcntl
import os
import signal
import subprocess
import sys
import time

name, *command = sys.argv[1:]
lease = f"{name}-{int(time.time())}-{os.getpid()}"
directory = os.path.join(os.environ["HOME"], ".local", "state", "test-service-leases")
os.makedirs(directory, mode=0o700, exist_ok=True)
fd = os.open(os.path.join(directory, lease), os.O_CREAT | os.O_EXCL | os.O_RDWR, 0o600)
os.set_inheritable(fd, True)
fcntl.flock(fd, fcntl.LOCK_EX)
os.environ["TEST_SERVICE_LEASE"] = lease
os.environ["TEST_SERVICE_LEASE_FD"] = str(fd)

# A separate open file description can wait for every inherited lock holder.
wait_fd = os.open(os.path.join(directory, lease), os.O_RDWR)
process_group = None
interrupted = 0
def interrupt(signum, frame):
    global interrupted
    interrupted = signum
    if process_group is not None:
        try:
            os.killpg(process_group, signum)
        except ProcessLookupError:
            pass
signal.signal(signal.SIGINT, interrupt)
signal.signal(signal.SIGTERM, interrupt)
process = subprocess.Popen(command, pass_fds=(fd,), start_new_session=True)
process_group = process.pid
if interrupted:
    interrupt(interrupted, None)
status = process.wait()
os.close(fd)
fcntl.flock(wait_fd, fcntl.LOCK_EX)
process_group = None
try:
    cleanup = subprocess.run(
        [
            "docker", "compose", "-p", lease,
            "-f", "docker-compose.test.yml",
            "-f", "docker-compose.test-lease.yml", "down",
        ],
        start_new_session=True,
    ).returncode
    if cleanup == 0:
        os.unlink(os.path.join(directory, lease))
except OSError as error:
    print(f"test service cleanup failed: {error}", file=sys.stderr)
    cleanup = 1
if interrupted:
    status = 128 + interrupted
elif status < 0:
    status = 128 - status
sys.exit(status or (1 if cleanup else 0))
' "$@"
