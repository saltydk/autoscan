import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from contextlib import redirect_stdout
from unittest.mock import patch

from scripts import go_vulnerabilities


CONFIG = {"config": {"protocol_version": "v1.0.0", "scan_level": "symbol", "scan_mode": "source"}}


def finding(number=1, *, called=True, module="stdlib", version="v1.27.1", fixed="v1.27.2"):
    frame = {"module": module, "version": version, "package": "net/http"}
    if called:
        frame["function"] = "ServeHTTP"
    return {"finding": {"osv": f"GO-2026-{6600 + number}", "fixed_version": fixed, "trace": [frame]}}


def stream(messages):
    return "\n".join(json.dumps(message, indent=2) for message in messages)


class GoVulnerabilityReportingTests(unittest.TestCase):
    def run_report(self, messages, *, returncode=0, stderr=""):
        raw = stream(messages)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            summary = root / "summary.md"
            output = io.StringIO()
            commands = []

            def runner(command, **kwargs):
                commands.append(command)
                return subprocess.CompletedProcess(command, returncode, raw, stderr)

            with patch.dict("os.environ", {"GITHUB_STEP_SUMMARY": str(summary),
                            "GH_TOKEN": "private-test-token"}, clear=True), redirect_stdout(output):
                status = go_vulnerabilities.main([
                    "--tool", "golang.org/x/vuln/cmd/govulncheck@v1.8.0",
                    "--output-directory", str(root / "report"),
                ], runner=runner)
            files = {p.name: p.read_text() for p in (root / "report").iterdir()}
            return status, output.getvalue(), summary.read_text(), files, commands

    def test_real_failure_shape_has_one_actionable_annotation_and_keeps_full_evidence(self):
        messages = [CONFIG]
        for number in range(1, 10):
            messages.extend([finding(number, called=False)] + [finding(number)] * 20)
        status, log, summary, files, commands = self.run_report(messages)
        self.assertEqual(status, 1)
        self.assertEqual(log.count("::error"), 1)
        self.assertIn("9 reachable vulnerabilities", log)
        self.assertIn("Upgrade Go from 1.27.1 to 1.27.2", log)
        self.assertNotIn("ServeHTTP", log)
        self.assertIn("Upgrade Go from 1.27.1 to 1.27.2", summary)
        self.assertEqual(summary.count("https://pkg.go.dev/vuln/GO-2026-"), 9)
        self.assertEqual(files["govulncheck.json"], stream(messages))
        self.assertIn("-json", commands[0])
        self.assertIn("-scan=symbol", commands[0])

    def test_uncalled_findings_do_not_fail_the_gate(self):
        status, log, summary, _, _ = self.run_report([CONFIG, finding(called=False)])
        self.assertEqual(status, 0)
        self.assertNotIn("::error", log)
        self.assertIn("No reachable vulnerabilities", summary)
        self.assertIn("Additional advisories without a reachable call: 1", summary)

    def test_clean_scan_has_a_clear_summary(self):
        status, log, summary, _, _ = self.run_report([CONFIG])
        self.assertEqual(status, 0)
        self.assertNotIn("::error", log)
        self.assertIn("No reachable vulnerabilities", summary)

    def test_third_party_module_without_a_fix_is_reported(self):
        status, log, summary, _, _ = self.run_report([
            CONFIG, finding(module="example/module", version="v1.0.0", fixed="")])
        self.assertEqual(status, 1)
        self.assertIn("No fix advertised", summary)
        self.assertIn("example/module", summary)
        self.assertNotIn("Upgrade Go", log)

    def test_failed_scan_is_distinguished_from_vulnerability_findings(self):
        status, log, summary, files, _ = self.run_report([CONFIG], returncode=1, stderr="database unavailable")
        self.assertEqual(status, 2)
        self.assertEqual(log.count("::error"), 1)
        self.assertIn("Go vulnerability scan failed", log)
        self.assertIn("database unavailable", summary)
        self.assertIn("database unavailable", files["stderr.log"])

    def test_invalid_or_incomplete_output_never_passes(self):
        for messages in ([], [{"config": {"protocol_version": "v1.0.0", "scan_level": "package"}}],
                         [CONFIG, {"finding": {"osv": "GO-2026-6601", "trace": []}}]):
            with self.subTest(messages=messages):
                status, log, summary, _, _ = self.run_report(messages)
                self.assertEqual(status, 2)
                self.assertEqual(log.count("::error"), 1)
                self.assertNotIn("No reachable vulnerabilities", summary)

    def test_scanner_errors_cannot_add_annotations_or_expose_credentials(self):
        status, log, summary, files, _ = self.run_report([CONFIG], returncode=1,
            stderr="failed private-test-token\n::error::forged annotation\r\nsecond line")
        self.assertEqual(status, 2)
        self.assertEqual(sum(line.startswith("::error") for line in log.splitlines()), 1)
        for value in (log, summary, files["stderr.log"]):
            self.assertNotIn("private-test-token", value)


if __name__ == "__main__":
    unittest.main()
