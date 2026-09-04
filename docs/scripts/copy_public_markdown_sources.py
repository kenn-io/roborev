#!/usr/bin/env python3
from __future__ import annotations

import argparse
import pathlib
import shutil

from public_markdown_sources import fail, public_markdown_sources

ROOT = pathlib.Path(__file__).resolve().parents[1]


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Copy public docs Markdown sources into the generated site.",
    )
    parser.add_argument(
        "--docs-dir",
        required=True,
        help="public docs source directory relative to docs root",
    )
    parser.add_argument(
        "--site-dir",
        default="site",
        help="generated site directory relative to docs root",
    )
    parser.add_argument(
        "--config",
        default="zensical.toml",
        help="Zensical config path relative to docs root",
    )
    args = parser.parse_args()

    docs_dir = ROOT / args.docs_dir
    site_dir = ROOT / args.site_dir
    config = ROOT / args.config
    if not docs_dir.is_dir():
        fail(f"missing public docs source directory: {docs_dir.relative_to(ROOT)}")
    if not site_dir.is_dir():
        fail(f"missing generated site directory: {site_dir.relative_to(ROOT)}")

    for path in public_markdown_sources(config):
        source = docs_dir / path
        destination = site_dir / path
        if not source.is_file():
            fail(f"nav source is missing from public docs tree: {source.relative_to(ROOT)}")
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, destination)
        # Section index pages advertise a flattened twin (/docs/<dir>.md, the
        # same shape every non-index page uses), so publish that alias too.
        if path != "index.md" and path.endswith("/index.md"):
            alias = site_dir / f"{path.removesuffix('/index.md')}.md"
            shutil.copyfile(source, alias)

    print("public Markdown sources copied")


if __name__ == "__main__":
    main()
