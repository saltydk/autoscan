"""Run each workflow script entry point on the interpreter running the tests.

Workflows call these scripts with the runner's own ``python3``. Each check
starts a fresh interpreter with warnings as errors and development mode, the
way the workflows invoke the script, using local inputs only.
"""

import json
import os
import subprocess  # nosec B404
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
STRICT_ENV = {"PYTHONWARNINGS": "error", "PYTHONDEVMODE": "1"}
CLI_SCRIPTS = (
    ["scripts/image.py"],
    ["-m", "scripts.publication"],
    ["scripts/go_vulnerabilities.py"],
    ["scripts/release_go_issues.py"],
    ["scripts/runner_images.py"],
    ["scripts/test-nonroot-image.py"],
)


def run_python(*arguments, cwd=ROOT, env=None):
    # The interpreter is this test's own; arguments are fixed repository paths.
    return subprocess.run(  # nosec B603
        [sys.executable, "-W", "error", "-X", "dev", *arguments],
        cwd=cwd,
        env={**os.environ, **STRICT_ENV, **(env or {})},
        capture_output=True,
        text=True,
        check=False,
    )


class EntrypointTests(unittest.TestCase):
    def assert_clean(self, result, status=0):
        self.assertEqual(result.returncode, status, result.stderr)
        self.assertNotIn("Traceback", result.stderr)
        self.assertNotIn("Warning", result.stderr)

    def test_every_script_has_a_checked_entry_point(self):
        scripts = sorted(path.name for path in (ROOT / "scripts").glob("*.py"))
        covered = sorted(
            Path(arguments[-1].replace(".", "/")).name
            if arguments[0] == "-m"
            else Path(arguments[0]).name
            for arguments in CLI_SCRIPTS
        )
        self.assertEqual(
            [name.removesuffix(".py") for name in scripts],
            [name.removesuffix(".py") for name in covered],
        )

    def test_every_script_compiles_and_shows_help(self):
        for arguments in CLI_SCRIPTS:
            with self.subTest(script=" ".join(arguments)):
                result = run_python(*arguments, "--help")
                self.assert_clean(result)
                self.assertIn("usage:", result.stdout)

    def test_release_recipe_check_runs_on_the_repository(self):
        result = run_python("scripts/image.py", "check-release")

        self.assert_clean(result)
        self.assertIn("Release and master runtime recipes match", result.stdout)

    def test_publication_summaries_run(self):
        with tempfile.TemporaryDirectory() as directory:
            summary = Path(directory) / "summary.md"
            acceptance = run_python(
                "-m",
                "scripts.publication",
                "summarize",
                "--prepare",
                "success",
                "--tests",
                "success",
                "--publish",
                "skipped",
                "--tags",
                "[]",
                "--summary",
                str(summary),
            )
            written = summary.read_text(encoding="utf-8")
            build = run_python(
                "-m",
                "scripts.publication",
                "summarize-build",
                "--build",
                "success",
                "--images",
                "failure",
            )

        self.assert_clean(acceptance)
        self.assertIn("| Publication | skipped |", written)
        self.assert_clean(build, status=1)
        self.assertIn("did not succeed", build.stderr)

    def test_vulnerability_report_feeds_issue_reconciliation(self):
        config = {
            "config": {
                "protocol_version": "v1.0.0",
                "scan_level": "symbol",
                "scan_mode": "source",
            }
        }
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            tools = root / "bin"
            tools.mkdir()
            # A stand-in for `go run govulncheck` that reports a clean scan.
            go = tools / "go"
            go.write_text(f"#!/bin/sh\nprintf '%s\\n' '{json.dumps(config)}'\n")
            go.chmod(0o755)
            report = root / "report"
            env = {
                "PATH": f"{tools}{os.pathsep}{os.environ['PATH']}",
                "GITHUB_STEP_SUMMARY": str(root / "summary.md"),
                # The repository is validated before any token use or request.
                "GITHUB_REPOSITORY": "",
            }
            scan = run_python(
                str(ROOT / "scripts/go_vulnerabilities.py"),
                "--tool",
                "golang.org/x/vuln/cmd/govulncheck@v1.10.0",
                "--output-directory",
                str(report),
                cwd=root,
                env=env,
            )
            issues = run_python(
                "scripts/release_go_issues.py",
                "--report-directory",
                str(report),
                "--release-tag",
                "v1.2.3",
                "--dry-run",
                "true",
                "--output",
                str(root / "issues.json"),
                env=env,
            )
            result = json.loads((root / "issues.json").read_text())
            scan_report = json.loads((report / "report.json").read_text())

        self.assert_clean(scan)
        self.assertEqual(scan_report["status"], "passed")
        # Without a repository the reconciler stops before any GitHub request.
        self.assertEqual(issues.returncode, 2, issues.stderr)
        self.assertNotIn("Traceback", issues.stderr)
        self.assertEqual(result["error"], "invalid repository")


if __name__ == "__main__":
    unittest.main()
