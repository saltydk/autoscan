import os
from pathlib import Path
import subprocess
from tempfile import TemporaryDirectory
import textwrap
import unittest


WORKFLOW = Path(__file__).resolve().parents[1] / ".github/workflows/base-update.yml"


def request_script():
    step = WORKFLOW.read_text().split("      - name: Request base image refresh\n", 1)[1]
    lines = []
    for line in step.split("        run: |\n", 1)[1].splitlines():
        if line and not line.startswith("          "):
            break
        lines.append(line)
    return textwrap.dedent("\n".join(lines))


class BaseRefreshRequestTests(unittest.TestCase):
    def setUp(self):
        directory = TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.log = self.root / "gh-arguments"
        self.summary = self.root / "summary"
        gh = self.root / "gh"
        gh.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$FAKE_GH_LOG"\nexit "$FAKE_GH_EXIT"\n')
        gh.chmod(0o755)
        self.environment = {**os.environ, "PATH": str(self.root) + os.pathsep + os.environ["PATH"],
                            "GH_TOKEN": "fixture", "GITHUB_STEP_SUMMARY": str(self.summary),
                            "FAKE_GH_LOG": str(self.log), "FAKE_GH_EXIT": "0"}

    def run_request(self):
        return subprocess.run(["bash", "-e", "-o", "pipefail", "-c", request_script()],
                              cwd=self.root, env=self.environment, capture_output=True, text=True)

    def test_request_uses_the_shared_base_coordinator(self):
        result = self.run_request()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.log.read_text().splitlines(), [
            "workflow", "run", "request-refresh.yml", "--repo", "saltydk/docker-alpine-s6overlay",
            "--ref", "master",
        ])
        self.assertIn("next scheduled or manual image update will retry", self.summary.read_text())

    def test_missing_token_does_not_dispatch(self):
        self.environment["GH_TOKEN"] = ""
        result = self.run_request()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Actions write access", result.stdout)
        self.assertFalse(self.log.exists())

    def test_rejected_request_does_not_report_success(self):
        self.environment["FAKE_GH_EXIT"] = "1"
        result = self.run_request()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.summary.exists())
        self.assertNotIn("::notice::", result.stdout)


if __name__ == "__main__":
    unittest.main()
