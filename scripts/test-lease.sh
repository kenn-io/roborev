#!/bin/sh
# Test service lease contract:
# L is `<repo>-<epoch>-<pid>` and matches `^[a-z0-9][a-z0-9_-]{0,63}$`.
# Every run container carries `test-service.lease=L`; Compose runs use project `L`.
# The lease file is `$HOME/.local/state/test-service-leases/L`, created with
# `O_EXCL` in a `0700` directory as a `0600` file before any Docker resource starts.
# The producer holds `flock(LOCK_EX)` until all descendants exit. The descriptor is
# inheritable and stays open in every descendant.
# Producers never delete lease files.
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
compose = command[0] == "--compose"
if compose:
    command = command[1:]
lease = f"{name}-{int(time.time())}-{os.getpid()}"
directory = os.path.join(os.environ["HOME"], ".local", "state", "test-service-leases")
os.makedirs(directory, mode=0o700, exist_ok=True)
info = os.stat(directory)
if info.st_uid != os.getuid() or info.st_mode & 0o077:
    sys.exit("test service lease directory must be private and owned by the current user")
fd = os.open(os.path.join(directory, lease), os.O_CREAT | os.O_EXCL | os.O_RDWR, 0o600)
os.set_inheritable(fd, True)
fcntl.flock(fd, fcntl.LOCK_EX)
os.environ["TEST_SERVICE_LEASE"] = lease
os.environ["COMPOSE_PROJECT_NAME"] = lease
if not compose:
    os.execvp(command[0], command)

# A separate open file description can wait for every inherited lock holder.
wait_fd = os.open(os.path.join(directory, lease), os.O_RDONLY)
process = None
interrupted = 0
def interrupt(signum, frame):
    global interrupted
    interrupted = signum
    if process is not None and process.poll() is None:
        try:
            process.send_signal(signum)
        except ProcessLookupError:
            pass
signal.signal(signal.SIGINT, interrupt)
signal.signal(signal.SIGTERM, interrupt)
try:
    process = subprocess.Popen(command, pass_fds=(fd,))
    if interrupted:
        interrupt(interrupted, None)
    status = process.wait()
except OSError as error:
    print(error, file=sys.stderr)
    status = 1
os.close(fd)
fcntl.flock(wait_fd, fcntl.LOCK_EX)
try:
    cleanup = subprocess.run(["docker", "compose", "-p", lease, "-f", "docker-compose.test.yml", "-f", "docker-compose.test-lease.yml", "down"]).returncode
except OSError as error:
    print(error, file=sys.stderr)
    cleanup = 1
if interrupted:
    status = 128 + interrupted
elif status < 0:
    status = 128 - status
sys.exit(status or (1 if cleanup else 0))
' "$@"
