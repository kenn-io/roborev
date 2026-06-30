#!/usr/bin/env python3
from __future__ import annotations

import argparse
import pathlib

from public_markdown_sources import fail, public_markdown_sources

ROOT = pathlib.Path(__file__).resolve().parents[1]
CONFIG = ROOT / "zensical.toml"


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Check generated docs include public Markdown source files.",
    )
    parser.add_argument(
        "--site-dir",
        default="site",
        help="generated site directory relative to docs root",
    )
    args = parser.parse_args()

    site_dir = ROOT / args.site_dir
    if not site_dir.is_dir():
        fail(f"missing generated site directory: {site_dir.relative_to(ROOT)}")

    for path in public_markdown_sources(CONFIG):
        source = ROOT / path
        generated = site_dir / path
        if not source.is_file():
            fail(f"nav source is missing: {path}")
        if not generated.is_file():
            fail(f"generated site is missing Markdown source: {generated.relative_to(ROOT)}")
        if generated.read_bytes() != source.read_bytes():
            fail(
                "generated Markdown source differs from docs source: "
                f"{generated.relative_to(ROOT)}",
            )

    print("public Markdown source checks passed")


if __name__ == "__main__":
    main()
