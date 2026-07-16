#!/usr/bin/env python3
from __future__ import annotations

import argparse
import pathlib
import re

import mdformat

from public_markdown_sources import public_markdown_sources

ROOT = pathlib.Path(__file__).resolve().parents[1]
TABLE_DELIMITER = re.compile(
    r"^[ \t]*\|?[ \t]*:?-{3,}:?[ \t]*(?:\|[ \t]*:?-{3,}:?[ \t]*)+\|?[ \t]*$",
)


def format_text(source: str) -> str:
    masked, tables = mask_tables(source)
    formatted = mdformat.text(
        masked,
        options={"wrap": 80},
        extensions={"front_matters", "gfm", "mkdocs"},
    )
    for marker, table in tables:
        formatted = formatted.replace(f"{marker}\n", table, 1)
    return formatted


def mask_tables(source: str) -> tuple[str, list[tuple[str, str]]]:
    lines = source.splitlines(keepends=True)
    masked: list[str] = []
    tables: list[tuple[str, str]] = []
    index = 0

    while index < len(lines):
        if index + 1 >= len(lines) or not is_table_start(lines, index):
            masked.append(lines[index])
            index += 1
            continue

        end = index + 2
        while end < len(lines):
            row = lines[end].rstrip("\r\n")
            if not row.strip() or "|" not in row:
                break
            end += 1

        table = "".join(lines[index:end])
        marker = table_marker(source, len(tables))
        masked.append(f"{marker}\n")
        tables.append((marker, table))
        index = end

    return "".join(masked), tables


def is_table_start(lines: list[str], index: int) -> bool:
    header = lines[index].rstrip("\r\n")
    delimiter = lines[index + 1].rstrip("\r\n")
    return "|" in header and TABLE_DELIMITER.fullmatch(delimiter) is not None


def table_marker(source: str, index: int) -> str:
    marker = f"<!-- roborev-mdformat-table-{index} -->"
    while marker in source:
        marker = f"<!-- roborev-{marker[5:]} -->"
    return marker


def format_sources(root: pathlib.Path, config: pathlib.Path, check: bool) -> int:
    changed = False
    for relative_path in public_markdown_sources(config):
        path = root / relative_path
        source = path.read_text(encoding="utf-8")
        formatted = format_text(source)
        if formatted == source:
            continue

        changed = True
        if check:
            print(f"would reformat: {path.relative_to(root)}")
        else:
            path.write_text(formatted, encoding="utf-8")
            print(f"reformatted: {path.relative_to(root)}")

    return int(check and changed)


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Format Zensical Markdown sources.",
    )
    parser.add_argument(
        "--check",
        action="store_true",
        help="report non-canonical files without modifying them",
    )
    parser.add_argument(
        "--root",
        type=pathlib.Path,
        default=ROOT,
        help="documentation root containing Markdown sources",
    )
    parser.add_argument(
        "--config",
        type=pathlib.Path,
        help="Zensical configuration (defaults to ROOT/zensical.toml)",
    )
    args = parser.parse_args()
    root = args.root.resolve()
    config = args.config.resolve() if args.config else root / "zensical.toml"
    return format_sources(root, config, args.check)


if __name__ == "__main__":
    raise SystemExit(main())
