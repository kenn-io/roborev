#!/usr/bin/env python3
"""Remove completed PostgreSQL run reports while preserving active leases."""

import fcntl
import os
from pathlib import Path
import re
import sys

reports = Path(sys.argv[1])
leases = Path(os.environ["HOME"]) / ".local/state/test-service-leases"
names = {"postgres-storage.json", "postgres-storage-reruns.txt", "postgres-daemon.json", "postgres-daemon-reruns.txt"}
if reports.is_dir():
    for directory in reports.iterdir():
        if not re.fullmatch(r"roborev-\d+-\d+", directory.name) or directory.is_symlink() or not directory.is_dir():
            continue
        try:
            with (leases / directory.name).open("r") as lease:
                fcntl.flock(lease, fcntl.LOCK_EX | fcntl.LOCK_NB)
                if any(child.name not in names for child in directory.iterdir()):
                    continue
                for name in names:
                    (directory / name).unlink(missing_ok=True)
                directory.rmdir()
        except (FileNotFoundError, BlockingIOError):
            continue
