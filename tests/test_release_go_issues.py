import copy
import io
import json
import tempfile
import unittest
import urllib.error
from email.message import Message
from pathlib import Path

from scripts import release_go_issues as rgi

CONFIG = {
    "config": {
        "protocol_version": "v1.0.0",
        "scan_level": "symbol",
        "scan_mode": "source",
        "scanner_name": "govulncheck",
        "scanner_version": "v1.10.0",
        "go_version": "go1.27.3",
    }
}
OSV = {
    "osv": {
        "id": "GO-2026-6859",
        "aliases": ["CVE-2026-97054"],
        "summary": "HTTP/1 server crash due to MaxBytesReader header race in net/http",
    }
}
ROW = {
    "id": "GO-2026-6859",
    "module": "stdlib",
    "version": "v1.27.3",
    "fixed": "v1.27.4",
    "packages": ["net/http"],
}


class FakeStore:
    def __init__(self, issues=None, *, bot_closed=True):
        self.issues = issues or []
        self.bot_closed = bot_closed
        self.writes = []

    def list_issues(self):
        return copy.deepcopy(self.issues)

    def closed_by_bot(self, number):
        return self.bot_closed

    def write(self, action):
        self.writes.append(copy.deepcopy(action))
        number = action["number"] or len(self.issues) + 1
        issue = next((row for row in self.issues if row["number"] == number), None)
        if issue is None:
            issue = {"number": number, "state": "open", "user": {"login": rgi.BOT}}
            self.issues.append(issue)
        issue.update(
            {
                key: value
                for key, value in action["payload"].items()
                if key != "state_reason"
            }
        )
        return {"number": number, "html_url": f"https://example.test/issues/{number}"}


class ReleaseGoIssueTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)

    def scan(self, rows, *, complete=True):
        root = Path(self.directory.name) / "scan"
        root.mkdir(exist_ok=True)
        (root / "report.json").write_text(
            json.dumps({"scan_complete": complete, "headline": "x", "reachable": rows})
        )
        (root / "govulncheck.json").write_text(
            "\n".join(json.dumps(m, indent=2) for m in (CONFIG, OSV))
        )
        return root

    def run_release(
        self, store, rows, release="v1.4.7", *, dry_run="false", complete=True
    ):
        return rgi.main(
            [
                "--report-directory",
                str(self.scan(rows, complete=complete)),
                "--release-tag",
                release,
                "--dry-run",
                dry_run,
            ],
            store=store,
        )

    def test_reachable_release_advisory_opens_one_issue_and_repeat_runs_are_quiet(self):
        store = FakeStore()
        self.assertEqual(self.run_release(store, [ROW]), 0)
        self.assertEqual([w["operation"] for w in store.writes], ["create"])
        body = store.issues[0]["body"]
        self.assertIn("Reachable in the published release `v1.4.7`", body)
        self.assertIn("CVE-2026-97054", body)
        self.assertIn("| Go 1.27.3 | 1.27.4 | net/http |", body)
        self.assertEqual(
            rgi.marker(store.issues[0]),
            {"schema": 1, "id": "GO-2026-6859", "resolved": False},
        )
        self.assertEqual(self.run_release(store, [ROW]), 0)
        self.assertEqual(len(store.writes), 1)

    def test_issue_stays_open_until_a_newer_release_is_clean_then_closes_with_evidence(
        self,
    ):
        store = FakeStore()
        self.run_release(store, [ROW])
        self.run_release(store, [ROW], "v1.4.7")
        self.assertEqual(store.issues[0]["state"], "open")
        self.assertEqual(self.run_release(store, [], "v1.4.8"), 0)
        self.assertEqual(store.writes[-1]["operation"], "close")
        self.assertEqual(store.writes[-1]["payload"]["state_reason"], "completed")
        body = store.issues[0]["body"]
        self.assertIn(
            "No longer reachable in the published release `v1.4.8`; the fixed build is live.",
            body,
        )
        self.assertIn("| Go 1.27.3 | 1.27.4 | net/http |", body)
        self.assertEqual(
            rgi.marker(store.issues[0]),
            {"schema": 1, "id": "GO-2026-6859", "resolved": True},
        )
        self.run_release(store, [], "v1.4.8")
        self.assertEqual(len(store.writes), 2)

    def test_recurrence_reopens_an_automation_closed_issue_but_not_a_human_closed_one(
        self,
    ):
        for bot_closed, expected in (
            (True, ["create", "close", "reopen"]),
            (False, ["create", "close"]),
        ):
            with self.subTest(bot_closed=bot_closed):
                store = FakeStore(bot_closed=bot_closed)
                self.run_release(store, [ROW])
                self.run_release(store, [], "v1.4.8")
                self.run_release(store, [ROW], "v1.4.9")
                self.assertEqual([w["operation"] for w in store.writes], expected)

    def test_human_text_outside_the_managed_block_is_preserved(self):
        store = FakeStore()
        self.run_release(store, [ROW])
        store.issues[0]["body"] = (
            "Triage note.\n" + store.issues[0]["body"] + "\nFollow-up."
        )
        self.run_release(store, [ROW, {**ROW, "packages": ["mime/multipart"]}])
        body = store.issues[0]["body"]
        self.assertTrue(
            body.startswith("Triage note.\n") and body.endswith("\nFollow-up.")
        )
        self.assertIn("mime/multipart", body)

    def test_incomplete_scan_or_bad_release_tag_never_writes(self):
        store = FakeStore()
        self.assertEqual(self.run_release(store, [ROW], complete=False), 2)
        self.assertEqual(self.run_release(store, [ROW], "main"), 2)
        self.assertEqual(store.writes, [])

    def test_dry_run_plans_without_writing(self):
        store = FakeStore()
        self.assertEqual(self.run_release(store, [ROW], dry_run="true"), 0)
        self.assertEqual(store.writes, [])

    def test_forged_markers_from_other_authors_are_ignored_and_duplicates_stop(self):
        store = FakeStore()
        self.run_release(store, [ROW])
        forged = {
            **copy.deepcopy(store.issues[0]),
            "number": 9,
            "user": {"login": "someone"},
        }
        store.issues.append(forged)
        self.run_release(store, [ROW])
        self.assertEqual(len(store.writes), 1)
        store.issues.append({**copy.deepcopy(store.issues[0]), "number": 10})
        self.assertEqual(self.run_release(store, [ROW]), 2)
        self.assertEqual(len(store.writes), 1)


class GitHubIssuesTests(unittest.TestCase):
    def test_http_error_is_reported_and_closed(self):
        error = urllib.error.HTTPError(
            "https://api.github.com/repos/saltydk/autoscan/issues",
            502,
            "Bad Gateway",
            Message(),
            io.BytesIO(b'{"message": "Bad Gateway"}'),
        )

        def opener(request, timeout):
            raise error

        store = rgi.GitHubIssues("test-token", "saltydk/autoscan", opener=opener)

        with self.assertRaisesRegex(RuntimeError, "HTTP 502") as raised:
            store.request("/repos/saltydk/autoscan/issues")

        self.assertIs(raised.exception.__cause__, error)
        self.assertTrue(error.fp.closed)


if __name__ == "__main__":
    unittest.main()
