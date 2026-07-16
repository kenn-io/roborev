from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest
from contextlib import redirect_stdout
from io import StringIO

SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPT_DIR))

from format_markdown import format_sources, format_text


LONG_PARAGRAPH = (
    "This documentation paragraph contains enough ordinary prose to exceed "
    "the configured eighty-column limit and require syntax-aware wrapping."
)


class FormatMarkdownTest(unittest.TestCase):
    def test_format_text_preserves_zensical_extensions(self) -> None:
        source = (
            "---\n"
            "title: Example\n"
            "description: Extension fixture\n"
            "---\n\n"
            '!!! warning "Important"\n'
            "    This admonition contains enough prose to require wrapping "
            "without changing its Zensical block syntax or front matter.\n"
        )

        formatted = format_text(source)

        self.assertTrue(formatted.startswith("---\ntitle: Example\n"))
        self.assertIn('!!! warning "Important"', formatted)
        self.assertNotIn("_____", formatted)
        self.assertNotIn("````", formatted)

    def test_format_text_wraps_prose_and_preserves_table(self) -> None:
        table = "| Name|Value |\n|---|:---:|\n| alpha| beta |"
        source = f"{LONG_PARAGRAPH}\n\n{table}\n"

        formatted = format_text(source)

        prose = formatted.split("\n\n", 1)[0]
        self.assertLessEqual(max(map(len, prose.splitlines())), 80)
        self.assertIn(table, formatted)

    def test_check_reports_changes_without_writing(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            config = self.write_config(root, "index.md")
            source = root / "index.md"
            source.write_text(f"{LONG_PARAGRAPH}\n", encoding="utf-8")
            before = source.read_bytes()

            with redirect_stdout(StringIO()):
                result = format_sources(root, config, check=True)

            self.assertEqual(1, result)
            self.assertEqual(before, source.read_bytes())

    def test_write_formats_every_configured_source(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            config = self.write_config(root, "index.md", "guides/example.md")
            sources = [root / "index.md", root / "guides/example.md"]
            for source in sources:
                source.parent.mkdir(parents=True, exist_ok=True)
                source.write_text(f"{LONG_PARAGRAPH}\n", encoding="utf-8")

            with redirect_stdout(StringIO()):
                result = format_sources(root, config, check=False)

            self.assertEqual(0, result)
            for source in sources:
                self.assertLessEqual(
                    max(map(len, source.read_text(encoding="utf-8").splitlines())),
                    80,
                )
            with redirect_stdout(StringIO()):
                self.assertEqual(0, format_sources(root, config, check=True))

    @staticmethod
    def write_config(root: pathlib.Path, *sources: str) -> pathlib.Path:
        nav = ", ".join(f'{{"Page" = "{source}"}}' for source in sources[1:])
        config = root / "zensical.toml"
        config.write_text(
            "[project]\n"
            f"nav = [{nav}]\n",
            encoding="utf-8",
        )
        return config


if __name__ == "__main__":
    unittest.main()
