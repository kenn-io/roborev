#!/usr/bin/env python3
"""Make the assembled website tier paint in its own fonts from the first frame.

The hand-written product page, guide, and 404 page load /styles/site.css,
which declared its fonts as separate /fonts/*.woff2 files. A browser finds
those only after it parses the stylesheet, so it painted text in a fallback
face first. This step rewrites the built copies:

- every /fonts/*.woff2 reference in the website stylesheets becomes a data
  URL, so each font arrives with the render-blocking stylesheet;
- every website page gets hold-first-paint.js inline, directly after its
  <meta charset>, because Chromium still paints one frame before it decodes
  an embedded font.

The sources under website/ stay unchanged; only the built site is rewritten.
"""

from __future__ import annotations

import argparse
import base64
import pathlib
import re

from public_markdown_sources import fail

ROOT = pathlib.Path(__file__).resolve().parents[1]
WEBSITE = ROOT / "website"
HOLD_FIRST_PAINT = pathlib.Path(__file__).resolve().parent / "hold-first-paint.js"

FONT_URL = re.compile(r"""url\((["']?)(/fonts/[^"')]+\.woff2)\1\)""")
CHARSET_META = re.compile(r"<meta\s+charset=[^>]*>", re.IGNORECASE)
# Browsers look for the charset declaration only in the first 1024 bytes.
CHARSET_PREFIX_BYTES = 1024


def embed_fonts(site_dir: pathlib.Path, stylesheet: pathlib.Path) -> None:
    text = stylesheet.read_text(encoding="utf-8")

    def data_url(match: re.Match[str]) -> str:
        font = site_dir / match.group(2).lstrip("/")
        if not font.is_file():
            fail(f"{stylesheet.relative_to(site_dir)} references missing font {match.group(2)}")
        encoded = base64.b64encode(font.read_bytes()).decode("ascii")
        return f'url("data:font/woff2;base64,{encoded}")'

    embedded, count = FONT_URL.subn(data_url, text)
    if count:
        stylesheet.write_text(embedded, encoding="utf-8")


def hold_first_paint(page: pathlib.Path, script: str) -> None:
    text = page.read_text(encoding="utf-8")
    charset = CHARSET_META.search(text)
    if charset is None or len(text[: charset.end()].encode("utf-8")) > CHARSET_PREFIX_BYTES:
        fail(f"{page} needs a <meta charset> within its first {CHARSET_PREFIX_BYTES} bytes")
        return
    page.write_text(
        f"{text[: charset.end()]}\n    <script>{script}</script>{text[charset.end() :]}",
        encoding="utf-8",
    )


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Embed website fonts and inline the first-paint hold script.",
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

    for source in sorted(WEBSITE.glob("styles/**/*.css")):
        embed_fonts(site_dir, site_dir / source.relative_to(WEBSITE))

    script = HOLD_FIRST_PAINT.read_text(encoding="utf-8").strip()
    for source in sorted(WEBSITE.glob("**/*.html")):
        hold_first_paint(site_dir / source.relative_to(WEBSITE), script)


if __name__ == "__main__":
    main()
