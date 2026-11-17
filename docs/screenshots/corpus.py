#!/usr/bin/env python3
"""Write or read an approved screenshot corpus: one sanitized reviews.db.

A corpus directory holds reviews.db and a manifest.json that records its
SHA-256. Automated capture pins the manifest's own SHA-256, so the manifest
binds the exact database the screenshots were made from.
"""

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import sys

DATABASE = "reviews.db"


def digest(path):
    hasher = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1 << 20), b""):
            hasher.update(block)
    return hasher.hexdigest()


def write(source, directory):
    directory.mkdir(parents=True)
    database = directory / DATABASE
    shutil.copyfile(source, database)
    manifest = {"schema": 1, "files": {DATABASE: {"sha256": digest(database),
                                                  "size": database.stat().st_size}}}
    path = directory / "manifest.json"
    path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    print(f"{path} sha256={digest(path)}")


def extract(manifest_path, destination):
    manifest = json.loads(manifest_path.read_text())
    if manifest.get("schema") != 1 or set(manifest.get("files", {})) != {DATABASE}:
        raise ValueError(f"corpus manifest must list exactly {DATABASE}")
    expected = manifest["files"][DATABASE]["sha256"]
    source = manifest_path.parent / DATABASE
    if digest(source) != expected:
        raise ValueError(f"{DATABASE} differs from the corpus manifest")
    destination.mkdir(parents=True)
    target = destination / DATABASE
    shutil.copyfile(source, target)
    if digest(target) != expected:
        raise ValueError(f"copied {DATABASE} differs from the corpus manifest")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    write_parser = commands.add_parser("write", help="write a corpus from a prepared reviews.db")
    write_parser.add_argument("database", type=Path)
    write_parser.add_argument("directory", type=Path)
    extract_parser = commands.add_parser("extract", help="verify a corpus and copy its reviews.db")
    extract_parser.add_argument("manifest", type=Path)
    extract_parser.add_argument("destination", type=Path)
    args = parser.parse_args()
    try:
        if args.command == "write":
            write(args.database, args.directory)
        else:
            extract(args.manifest, args.destination)
    except (OSError, ValueError, KeyError) as error:
        print(f"corpus: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
