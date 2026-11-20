from __future__ import annotations

import io
import json
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path

from scripts.runner_images import RunnerImagesError, main, ubuntu_runners

BADGE = "![Endpoint Badge](https://img.shields.io/endpoint?url=example)"
PREVIEW = (
    "[![preview](https://img.shields.io/badge/preview-0969DA?style=flat)]"
    "(https://github.com/actions/runner-images/issues/1)"
)
HEADER = (
    "| Image | Architecture | YAML Label | Included Software |\n"
    "| --------------------|--------------|---------------------|------------------|\n"
)


def readme(rows: str, header: str = HEADER) -> str:
    return (
        "# GitHub Actions Runner Images\n\n"
        "## Available Images\n\n"
        f"{header}{rows}\n"
        "> [!IMPORTANT]\n> Notes follow the table.\n\n"
        "### Label scheme\n\n| Not | The | Table |\n| --- | --- | --- |\n"
    )


TABLE_ROWS = (
    f"| Ubuntu 26.04<br>{BADGE} | x64 | `ubuntu-26.04` | [ubuntu-26.04] |\n"
    f"| Ubuntu 26.04 Arm64<br>{BADGE} | arm64 | `ubuntu-26.04-arm` | [arm] |\n"
    f"| Ubuntu 24.04<br>{BADGE} | x64 | `ubuntu-latest` or `ubuntu-24.04` | [u24] |\n"
    f"| Ubuntu Slim<br>{BADGE} | x64 | `ubuntu-slim` | [ubuntu-slim] |\n"
    f"| Ubuntu 28.04 {PREVIEW}<br>{BADGE} | x64 | `ubuntu-28.04` | [u28] |\n"
    f"| macOS 26<br>{BADGE} | x64 | `macos-latest-large`, `macos-26-intel` | [m] |\n"
    f"| Windows Server 2025<br>{BADGE} | x64 | `windows-latest` | [w] |\n"
)


class UbuntuRunnerTests(unittest.TestCase):
    def test_lists_one_versioned_label_per_ubuntu_image(self) -> None:
        runners = ubuntu_runners(readme(TABLE_ROWS))

        self.assertEqual(
            runners,
            [
                {
                    "runner": "ubuntu-26.04",
                    "image": "Ubuntu 26.04",
                    "architecture": "x64",
                    "status": "available",
                },
                {
                    "runner": "ubuntu-26.04-arm",
                    "image": "Ubuntu 26.04 Arm64",
                    "architecture": "arm64",
                    "status": "available",
                },
                {
                    "runner": "ubuntu-24.04",
                    "image": "Ubuntu 24.04",
                    "architecture": "x64",
                    "status": "available",
                },
                {
                    "runner": "ubuntu-slim",
                    "image": "Ubuntu Slim",
                    "architecture": "x64",
                    "status": "available",
                },
                {
                    "runner": "ubuntu-28.04",
                    "image": "Ubuntu 28.04",
                    "architecture": "x64",
                    "status": "preview",
                },
            ],
        )

    def test_latest_alias_is_used_when_it_is_the_only_label(self) -> None:
        rows = f"| Ubuntu 30.04<br>{BADGE} | x64 | `ubuntu-latest` | [u30] |\n"

        runners = ubuntu_runners(readme(rows))

        self.assertEqual([runner["runner"] for runner in runners], ["ubuntu-latest"])

    def test_empty_or_missing_inputs_are_rejected(self) -> None:
        cases = {
            "empty document": "",
            "no section": "# Runner images\n\nNothing here.\n",
            "no rows": readme(""),
            "no ubuntu rows": readme(
                f"| macOS 26<br>{BADGE} | arm64 | `macos-26` | [m] |\n"
            ),
        }
        for name, document in cases.items():
            with self.subTest(name), self.assertRaises(RunnerImagesError):
                ubuntu_runners(document)

    def test_malformed_tables_are_rejected(self) -> None:
        cases = {
            "missing label column": readme(
                f"| Ubuntu 26.04<br>{BADGE} | x64 | [u26] |\n",
                header="| Image | Architecture | Included Software |\n| --- | --- | --- |\n",
            ),
            "missing separator": readme(
                f"| Ubuntu 26.04<br>{BADGE} | x64 | `ubuntu-26.04` | [u] |\n",
                header="| Image | Architecture | YAML Label | Included Software |\n",
            ),
            "short row": readme(f"| Ubuntu 26.04<br>{BADGE} | x64 |\n"),
            "row without label": readme(
                f"| Ubuntu 26.04<br>{BADGE} | x64 | ubuntu-26.04 | [u] |\n"
            ),
            "unexpected label": readme(
                f"| Ubuntu 26.04<br>{BADGE} | x64 | `self-hosted; rm` | [u] |\n"
            ),
            "duplicate label": readme(
                f"| Ubuntu 26.04<br>{BADGE} | x64 | `ubuntu-26.04` | [u] |\n"
                f"| Ubuntu 26.04 again<br>{BADGE} | x64 | `ubuntu-26.04` | [u] |\n"
            ),
        }
        for name, document in cases.items():
            with self.subTest(name), self.assertRaises(RunnerImagesError):
                ubuntu_runners(document)


class MainTests(unittest.TestCase):
    def test_writes_matrix_output_and_summary(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "README.md").write_text(readme(TABLE_ROWS), encoding="utf-8")
            stdout = io.StringIO()

            with redirect_stdout(stdout):
                status = main(
                    [
                        str(root / "README.md"),
                        "--github-output",
                        str(root / "output"),
                        "--summary",
                        str(root / "summary.md"),
                    ]
                )

            self.assertEqual(status, 0)
            output = (root / "output").read_text(encoding="utf-8")
            self.assertTrue(output.startswith("matrix="))
            self.assertEqual(output.count("\n"), 1)
            matrix = json.loads(output.removeprefix("matrix="))
            self.assertEqual(
                [entry["runner"] for entry in matrix["include"]],
                [
                    "ubuntu-26.04",
                    "ubuntu-26.04-arm",
                    "ubuntu-24.04",
                    "ubuntu-slim",
                    "ubuntu-28.04",
                ],
            )
            self.assertIn(
                "| `ubuntu-28.04` | Ubuntu 28.04 | x64 | preview |",
                (root / "summary.md").read_text(encoding="utf-8"),
            )
            self.assertEqual(stdout.getvalue().split()[0], "ubuntu-26.04")

    def test_unparseable_readme_fails_without_writing_a_matrix(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "README.md").write_text("not a table\n", encoding="utf-8")
            stderr = io.StringIO()

            with redirect_stderr(stderr):
                status = main(
                    [str(root / "README.md"), "--github-output", str(root / "out")]
                )

            self.assertEqual(status, 2)
            self.assertFalse((root / "out").exists())
            self.assertIn("::error::Runner discovery failed", stderr.getvalue())

    def test_missing_readme_fails(self) -> None:
        with tempfile.TemporaryDirectory() as directory, redirect_stderr(io.StringIO()):
            self.assertEqual(main([str(Path(directory) / "missing.md")]), 2)


if __name__ == "__main__":
    unittest.main()
