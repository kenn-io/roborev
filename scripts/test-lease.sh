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
import sys
import time

name, *command = sys.argv[1:]
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
os.execvp(command[0], command)
' "$@"
