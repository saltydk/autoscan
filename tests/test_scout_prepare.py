import hashlib
import importlib.util
import json
import os
from contextlib import redirect_stderr, redirect_stdout
from io import StringIO
from pathlib import Path
import subprocess
from tempfile import TemporaryDirectory
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[1]
ACTION = ROOT / ".github/actions/scout"
SPEC = importlib.util.spec_from_file_location("scout_prepare", ACTION / "prepare.py")
scout = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(scout)


class ScoutPreparationTests(unittest.TestCase):
    def setUp(self):
        temporary = TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.workspace = self.root / "work/repository"
        self.workspace.mkdir(parents=True)
        self.metadata = self.root / "action.yml"
        self.metadata.write_text((ACTION / "action.yml").read_text())
        self.calls = []
        self.delays = []
        self.failures = []
        self.payload = b"verified fixture binary\n"
        self.bad_checksum = False
        self.missing_checksum = False
        self.build_filename = True
        self.stage(scout.scout_tag(self.metadata))

    def stage(self, tag):
        action = self.workspace.parent / "_actions/docker/scout-action" / tag
        action.mkdir(parents=True, exist_ok=True)
        (action / "index.js").write_text("fixture")
        (action / "action.yaml").write_text("fixture")
        return action

    def download(self, command):
        self.calls.append(command)
        if self.failures:
            return subprocess.CompletedProcess(command, 1, "", self.failures.pop(0))
        directory = Path(command[command.index("--dir") + 1])
        names = [command[index + 1] for index, value in enumerate(command) if value == "--pattern"]
        binary, checksums = names
        (directory / binary).write_bytes(self.payload)
        if not self.missing_checksum:
            digest = "0" * 64 if self.bad_checksum else hashlib.sha256(self.payload).hexdigest()
            filename = binary
            if self.build_filename:
                platform = binary.removeprefix("docker-scout-action_")
                filename = f"{binary}_{command[3][1:]}_{platform}"
            (directory / checksums).write_text(f"{digest}  {filename}\n")
        return subprocess.CompletedProcess(command, 0, "", "")

    def prepare(self, architecture="X64"):
        with redirect_stdout(StringIO()):
            return scout.prepare(self.metadata, self.workspace, "Linux", architecture,
                                 runner=self.download, sleep=self.delays.append)

    def test_renovate_action_bump_also_changes_release_download_and_cache_path(self):
        old = scout.scout_tag(self.metadata)
        new = "v1.26.0" if old != "v1.26.0" else "v1.24.0"
        self.metadata.write_text(self.metadata.read_text().replace(f"@{old}", f"@{new}"))
        self.stage(new)

        binary = self.prepare()

        self.assertEqual(self.calls[0][3], new)
        self.assertIn(f"docker-scout_{new[1:]}_checksums.txt", self.calls[0])
        self.assertEqual(binary.parent.parent.name, new)
        self.assertEqual(binary.read_bytes(), self.payload)
        self.assertIn("--repo", self.calls[0])
        self.assertIn("docker/scout-action", self.calls[0])

    def test_binary_selection_uses_runner_architecture(self):
        for architecture, suffix in (("X64", "amd64"), ("ARM64", "arm64")):
            with self.subTest(architecture=architecture):
                binary = self.prepare(architecture)
                self.assertEqual(binary.name, f"docker-scout-action_linux_{suffix}")

    def test_asset_filenames_in_checksum_files_are_also_supported(self):
        self.build_filename = False
        self.assertEqual(self.prepare().read_bytes(), self.payload)

    def test_verified_binary_is_reused_and_corrupt_cache_is_replaced(self):
        binary = self.prepare()
        self.assertEqual(self.prepare(), binary)
        self.assertEqual(len(self.calls), 1)
        binary.write_bytes(b"partial download")
        self.assertEqual(self.prepare().read_bytes(), self.payload)
        self.assertEqual(len(self.calls), 2)

    def test_bad_or_missing_checksum_never_installs_a_binary(self):
        for missing in (False, True):
            with self.subTest(missing=missing):
                self.bad_checksum = True
                self.missing_checksum = missing
                with self.assertRaisesRegex(scout.PreparationError, "published checksum"):
                    self.prepare()
                dist = self.stage(scout.scout_tag(self.metadata)) / "dist"
                self.assertEqual(list(dist.iterdir()), [])

    def test_temporary_download_errors_retry_with_backoff(self):
        self.failures = ["HTTP 502 Bad Gateway", "connection reset by peer"]
        self.assertEqual(self.prepare().read_bytes(), self.payload)
        self.assertEqual(len(self.calls), 3)
        self.assertEqual(self.delays, [5, 15])

    def test_retries_are_bounded_and_permission_failures_are_not_retried(self):
        self.failures = ["HTTP 503 unavailable"] * 3
        with self.assertRaises(scout.PreparationError):
            self.prepare()
        self.assertEqual(len(self.calls), 3)
        self.calls.clear()
        self.delays.clear()
        self.failures = ["HTTP 403 Forbidden"]
        with self.assertRaises(scout.PreparationError):
            self.prepare()
        self.assertEqual(len(self.calls), 1)
        self.assertEqual(self.delays, [])

    def test_unsupported_runner_or_missing_action_stops_before_download(self):
        with self.assertRaisesRegex(scout.PreparationError, "unsupported Scout runner"):
            self.prepare("ARM")
        (self.stage(scout.scout_tag(self.metadata)) / "index.js").unlink()
        with self.assertRaisesRegex(scout.PreparationError, "runner has not prepared"):
            self.prepare()
        self.assertEqual(self.calls, [])

    def test_missing_token_never_falls_back_to_anonymous_download(self):
        error = StringIO()
        with patch.dict(os.environ, {"GH_TOKEN": ""}), patch.object(scout, "prepare") as prepare, \
                redirect_stderr(error):
            self.assertEqual(scout.main(), 1)
            prepare.assert_not_called()
        self.assertIn("refusing an anonymous Scout download", error.getvalue())

    def test_moving_refs_and_ambiguous_action_pins_are_rejected(self):
        for text in ("uses: docker/scout-action@main", "uses: docker/scout-action@v1",
                     "uses: docker/scout-action@" + "a" * 40,
                     "uses: docker/scout-action@v1.24.0\nuses: docker/scout-action@v1.26.0"):
            with self.subTest(text=text):
                self.metadata.write_text(text)
                with self.assertRaises(scout.PreparationError):
                    self.prepare()
        self.assertEqual(self.calls, [])

    def test_repository_has_one_scout_pin_and_renovate_keeps_release_tags(self):
        references = []
        for path in (ROOT / ".github").rglob("*"):
            if path.suffix in {".yml", ".yaml"}:
                references.extend((path, line) for line in path.read_text().splitlines()
                                  if "uses: docker/scout-action@" in line)
        self.assertEqual([path for path, _ in references], [ACTION / "action.yml"])
        config = json.loads((ROOT / ".github/renovate.json").read_text())
        self.assertTrue(any(rule.get("pinDigests") is False and
                            "docker/scout-action" in rule.get("matchPackageNames", [])
                            for rule in config["packageRules"]))


if __name__ == "__main__":
    unittest.main()
