from __future__ import annotations

import pathlib
import sys
import tomllib
from collections.abc import Iterable
from typing import Any


def fail(message: str) -> None:
    print(f"FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def iter_nav_markdown(value: Any) -> Iterable[str]:
    if isinstance(value, str):
        if value.endswith(".md"):
            yield value
        return
    if isinstance(value, list):
        for item in value:
            yield from iter_nav_markdown(item)
        return
    if isinstance(value, dict):
        for item in value.values():
            yield from iter_nav_markdown(item)


def public_markdown_sources(config: pathlib.Path) -> list[str]:
    try:
        data = tomllib.loads(config.read_text(encoding="utf-8"))
    except FileNotFoundError:
        fail(f"missing {config.name}")
    except tomllib.TOMLDecodeError as error:
        fail(f"invalid {config.name}: {error}")

    project = data.get("project")
    if not isinstance(project, dict):
        fail(f"{config.name} missing [project]")
    nav = project.get("nav")
    if not isinstance(nav, list):
        fail(f"{config.name} missing project.nav")

    seen: set[str] = set()
    sources: list[str] = []
    for path in ["index.md", *iter_nav_markdown(nav)]:
        if path not in seen:
            seen.add(path)
            sources.append(path)
    if not sources:
        fail(f"{config.name} nav does not list any Markdown documents")
    return sources
