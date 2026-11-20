import fcntl
import os
import sys

try:
    lease = os.environ["TEST_SERVICE_LEASE"]
    fd = int(os.environ["TEST_SERVICE_LEASE_FD"])
    path = os.path.join(os.environ["HOME"], ".local/state/test-service-leases", lease)
    if not os.path.samestat(os.fstat(fd), os.stat(path)):
        raise ValueError("lease does not match descriptor")
    with open(path) as probe:
        try:
            fcntl.flock(probe, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            pass
        else:
            raise ValueError("lease is unlocked")
    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
except (KeyError, ValueError, OSError):
    sys.exit("use make test-postgres")
