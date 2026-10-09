import contextlib
import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

from scripts import image, publication


class PublicationCliTests(unittest.TestCase):
    verify_publication: MagicMock
    check_promotion: MagicMock

    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="autoscan-publication-cli-")
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        previous = Path.cwd()
        os.chdir(self.root)
        self.addCleanup(os.chdir, previous)
        self.digest = "sha256:" + "a" * 64
        self.output = self.root / "github-output"
        self.writes = []
        self.fail_create = ""
        self.fail_inspect = False
        for name, options in (
            ("verify_publication", {"return_value": "verified"}),
            ("check_promotion", {"return_value": True}),
            ("_run", {"side_effect": self.registry}),
        ):
            mock = patch.object(image, name, **options)
            setattr(self, name, mock.start())
            self.addCleanup(mock.stop)

    def registry(self, command):
        self.assertEqual(command[:3], ["docker", "buildx", "imagetools"])
        if command[3] == "create":
            alias = command[command.index("--tag") + 1]
            if alias == self.fail_create:
                raise image.ImageError("registry write failed")
            self.writes.append(alias)
            return ""
        if command[3] == "inspect":
            if self.fail_inspect:
                raise image.ImageError("registry inspection failed after write")
            return json.dumps({"digest": self.digest})
        self.fail("unexpected registry operation")

    def call(self, arguments):
        with (
            contextlib.redirect_stdout(io.StringIO()),
            contextlib.redirect_stderr(io.StringIO()),
        ):
            return publication.main(arguments)

    def promotion_arguments(self):
        return [
            "promote",
            "--recipe",
            "release",
            "--variant",
            "standard",
            "--source-ref",
            "refs/tags/v1.4.5",
            "--source-sha",
            "b" * 40,
            "--release-tag",
            "v1.4.5",
            "--binary-sha",
            "b" * 40,
            "--image-digest",
            self.digest,
            "--version",
            "v1.4.5",
            "--tags",
            '["1.4.5","latest"]',
            "--github-output",
            str(self.output),
        ]

    def test_promotes_verified_aliases_and_records_success(self):
        self.assertEqual(self.call(self.promotion_arguments()), 0)
        self.assertEqual(
            self.writes, ["saltydk/autoscan:1.4.5", "saltydk/autoscan:latest"]
        )
        self.assertEqual(self.output.read_text(), "published=true\n")
        self.assertEqual(
            (self.root / "published.txt").read_text(),
            (self.root / "publication-attempts.txt").read_text(),
        )
        self.verify_publication.assert_called_once()

    def test_superseded_promotion_succeeds_without_writing(self):
        self.check_promotion.return_value = False
        self.assertEqual(self.call(self.promotion_arguments()), 0)
        self.assertEqual(self.writes, [])
        self.assertEqual(self.output.read_text(), "published=false\n")
        self.assertFalse((self.root / "publication-attempts.txt").exists())

    def test_real_guard_or_verification_failure_remains_failure(self):
        for operation in (self.check_promotion, self.verify_publication):
            with self.subTest(operation=operation):
                operation.side_effect = image.ImageError("cannot validate publication")
                self.assertEqual(self.call(self.promotion_arguments()), 1)
                operation.side_effect = None
                self.assertEqual(self.writes, [])
                self.assertFalse(self.output.exists())

    def test_partial_write_retains_attempts_and_verified_alias(self):
        self.fail_create = "saltydk/autoscan:latest"
        self.assertEqual(self.call(self.promotion_arguments()), 1)
        self.assertEqual(self.writes, ["saltydk/autoscan:1.4.5"])
        self.assertEqual(
            len((self.root / "publication-attempts.txt").read_text().splitlines()), 2
        )
        self.assertEqual(len((self.root / "published.txt").read_text().splitlines()), 1)
        self.assertFalse(self.output.exists())

    def test_inspection_failure_retains_evidence_of_attempted_write(self):
        self.fail_inspect = True
        self.assertEqual(self.call(self.promotion_arguments()), 1)
        self.assertEqual(self.writes, ["saltydk/autoscan:1.4.5"])
        self.assertEqual(
            len((self.root / "publication-attempts.txt").read_text().splitlines()), 1
        )
        self.assertFalse((self.root / "published.txt").exists())
        self.assertFalse(self.output.exists())

    def test_unexpected_alias_is_rejected_before_registry_writes(self):
        arguments = self.promotion_arguments()
        arguments[arguments.index("--tags") + 1] = '["unrelated"]'
        self.assertEqual(self.call(arguments), 1)
        self.assertEqual(self.writes, [])
        self.verify_publication.assert_not_called()

    def test_candidate_report_records_unavailable_steps(self):
        summary = self.root / "summary"
        self.assertEqual(
            self.call(
                [
                    "candidate",
                    "--platform",
                    "linux/amd64",
                    "--source-sha",
                    "b" * 40,
                    "--binary-sha",
                    "b" * 40,
                    "--variant",
                    "standard",
                    "--runtime",
                    "success",
                    "--security-report",
                    "",
                    "--kev",
                    "",
                    "--summary",
                    str(summary),
                ]
            ),
            0,
        )
        report = json.loads((self.root / "candidate.json").read_text())
        self.assertEqual(report["security_report"], "unavailable")
        self.assertEqual(report["runtime"], "success")
        self.assertIn("linux/amd64", summary.read_text())

    def test_acceptance_reports_skipped_publication_without_failure(self):
        summary = self.root / "summary"
        arguments = [
            "summarize",
            "--prepare",
            "success",
            "--tests",
            "success",
            "--publish",
            "success",
            "--published",
            "false",
            "--tags",
            '["master"]',
            "--summary",
            str(summary),
        ]
        self.assertEqual(self.call(arguments), 0)
        self.assertIn("| Publication | skipped |", summary.read_text())
        arguments[arguments.index("--publish") + 1] = "failure"
        self.assertEqual(self.call(arguments), 1)

    def test_build_acceptance_requires_both_stages(self):
        self.assertEqual(
            self.call(["summarize-build", "--build", "success", "--images", "success"]),
            0,
        )
        self.assertEqual(
            self.call(["summarize-build", "--build", "success", "--images", "failure"]),
            1,
        )


class PublicationWorkflowTests(unittest.TestCase):
    def test_acceptance_jobs_check_out_scripts_before_invoking_them(self):
        root = Path(__file__).resolve().parents[1]
        for name in ("build.yml", "docker.yml"):
            with self.subTest(workflow=name):
                content = (root / ".github" / "workflows" / name).read_text()
                acceptance = content.split("  acceptance:\n", 1)[1]
                self.assertLess(
                    acceptance.index("uses: actions/checkout@v7"),
                    acceptance.index("python3 -m scripts.publication"),
                )

    def test_publication_workflow_calls_files_without_inline_script_logic(self):
        root = Path(__file__).resolve().parents[1]
        content = (root / ".github" / "workflows" / "docker.yml").read_text()
        self.assertNotIn("python3 - <<", content)
        self.assertNotRegex(content, r"(?m)^\s+(?:for|while|if) ")
        self.assertIn("python3 -m scripts.publication promote", content)


if __name__ == "__main__":
    unittest.main()
