"""Keep one issue per govulncheck advisory reachable in the published release."""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.request
from collections.abc import Callable
from pathlib import Path
from typing import Any, Protocol

BOT = "github-actions[bot]"
START = "<!-- autoscan-release-govulncheck:start -->"
END = "<!-- autoscan-release-govulncheck:end -->"
MARKER = re.compile(r"<!-- autoscan-release-govulncheck:(\{[^\n]*\}) -->")
ADVISORY = re.compile(r"GO-\d{4}-\d+")
RELEASE = re.compile(r"v\d+\.\d+\.\d+")


class IssueStore(Protocol):
    def list_issues(self) -> list[dict]: ...
    def closed_by_bot(self, number: int) -> bool: ...
    def write(self, action: dict) -> dict: ...


def load_scan(directory: Path) -> tuple[dict[str, list[dict]], dict[str, dict], dict]:
    """Return reachable rows grouped by advisory, OSV metadata and scan configuration."""
    report = json.loads((directory / "report.json").read_text())
    if report.get("scan_complete") is not True:
        raise ValueError(
            "the release govulncheck scan did not complete: "
            + str(report.get("headline", ""))
        )
    grouped: dict[str, list[dict]] = {}
    for row in report.get("reachable", []):
        if not isinstance(row, dict) or not ADVISORY.fullmatch(str(row.get("id", ""))):
            raise ValueError("invalid reachable govulncheck row")
        grouped.setdefault(row["id"], []).append(row)
    metadata: dict[str, dict] = {}
    config: dict = {}
    raw = (directory / "govulncheck.json").read_text()
    decoder = json.JSONDecoder()
    position = 0
    while position < len(raw):
        if raw[position].isspace():
            position += 1
            continue
        message, position = decoder.raw_decode(raw, position)
        if isinstance(message, dict) and isinstance(message.get("config"), dict):
            config = message["config"]
        osv = message.get("osv") if isinstance(message, dict) else None
        if isinstance(osv, dict) and ADVISORY.fullmatch(str(osv.get("id", ""))):
            aliases = [
                alias for alias in osv.get("aliases", []) if isinstance(alias, str)
            ]
            metadata[osv["id"]] = {
                "aliases": sorted(aliases),
                "summary": str(osv.get("summary", "")),
            }
    return grouped, metadata, config


def marker(issue: dict) -> dict | None:
    """Only automation-authored issues with exactly one managed block are owned."""
    user = issue.get("user")
    body = issue.get("body") or ""
    if (
        "pull_request" in issue
        or not isinstance(user, dict)
        or user.get("login") != BOT
    ):
        return None
    found = MARKER.findall(body)
    if len(found) != 1 or body.count(START) != 1 or body.count(END) != 1:
        return None
    try:
        value = json.loads(found[0])
    except ValueError:
        return None
    if (
        not isinstance(value, dict)
        or value.get("schema") != 1
        or not ADVISORY.fullmatch(str(value.get("id", "")))
        or type(value.get("resolved")) is not bool
    ):
        return None
    return value


def cell(value: str) -> str:
    return value.replace("|", "\\|").replace("\n", " ").replace("\r", " ")


def found(row: dict) -> str:
    version = (
        row["version"].removeprefix("v")
        if row["module"] == "stdlib"
        else row["version"]
    )
    return ("Go " if row["module"] == "stdlib" else row["module"] + " ") + version


def fixed(row: dict) -> str:
    value = row.get("fixed") or ""
    return (
        value.removeprefix("v") if row["module"] == "stdlib" else value
    ) or "No fix advertised"


def render(
    advisory: str,
    rows: list[dict],
    meta: dict,
    release: str,
    config: dict,
    *,
    resolved: bool,
    previous_rows: list[dict] | None = None,
) -> tuple[str, str]:
    summary = meta.get("summary") or "Go vulnerability"
    aliases = meta.get("aliases") or []
    title = f"{advisory}: {summary}"[:256]
    state = {"schema": 1, "id": advisory, "resolved": resolved}
    lines = [
        START,
        "<!-- autoscan-release-govulncheck:"
        + json.dumps(state, separators=(",", ":"))
        + " -->",
        f"## {advisory}: {cell(summary)}",
        "",
        (
            f"Status: No longer reachable in the published release `{release}`; the fixed build is live."
            if resolved
            else f"Status: Reachable in the published release `{release}`."
        ),
        "",
    ]
    if aliases:
        lines.extend(["Aliases: " + ", ".join(aliases), ""])
    shown = previous_rows if resolved else rows
    if shown:
        lines.extend(
            [
                "Last reachable in the release:"
                if resolved
                else "Reachable code in the release:",
                "",
                "| Found in | Fixed in | Packages |",
                "| --- | --- | --- |",
            ]
        )
        lines.extend(
            f"| {cell(found(row))} | {cell(fixed(row))} | {cell(', '.join(row['packages']))} |"
            for row in shown
        )
        lines.append("")
    scanner = config.get("scanner_version", "unknown")
    go_version = config.get("go_version", "unknown")
    lines.extend(
        [
            f"Advisory: https://pkg.go.dev/vuln/{advisory}",
            "",
            (
                f"Checked with govulncheck {cell(str(scanner))} against the source of release `{release}` "
                f"built with {cell(str(go_version))}. Image scanners may track the same advisory under its aliases."
            ),
            "",
            "<!-- autoscan-release-govulncheck-rows:"
            + json.dumps(
                rows if not resolved else shown or [], separators=(",", ":")
            ).replace("--", "\\u002d\\u002d")
            + " -->",
            "This issue closes only after a newer published release no longer reaches this advisory.",
            END,
        ]
    )
    return title, "\n".join(lines)


def stored_rows(body: str) -> list[dict]:
    match = re.search(r"<!-- autoscan-release-govulncheck-rows:([^\n]*) -->", body)
    if not match:
        return []
    try:
        value = json.loads(match.group(1))
    except ValueError:
        return []
    return value if isinstance(value, list) else []


def replace_block(old: str, managed: str) -> str:
    first = old.index(START)
    last = old.index(END, first) + len(END)
    return old[:first] + managed + old[last:]


def plan(
    grouped: dict[str, list[dict]],
    metadata: dict[str, dict],
    config: dict,
    release: str,
    issues: list[dict],
    closed_by_bot: Callable[[int], bool],
) -> list[dict]:
    if not RELEASE.fullmatch(release):
        raise ValueError("release tag must be a published semantic version")
    owned: dict[str, dict] = {}
    for issue in issues:
        value = marker(issue)
        if value is None:
            continue
        if value["id"] in owned:
            raise ValueError(
                f"duplicate release govulncheck issues for {value['id']} need manual reconciliation"
            )
        owned[value["id"]] = issue
    actions = []
    for advisory in sorted(set(grouped) | set(owned)):
        existing = owned.get(advisory)
        rows = grouped.get(advisory, [])
        if rows:
            title, body = render(
                advisory,
                rows,
                metadata.get(advisory, {}),
                release,
                config,
                resolved=False,
            )
            if existing is None:
                actions.append(
                    {
                        "operation": "create",
                        "number": None,
                        "advisory": advisory,
                        "payload": {"title": title, "body": body},
                    }
                )
                continue
            if existing["state"] == "closed":
                if not closed_by_bot(existing["number"]):
                    continue  # A human closure is an explicit opt-out.
                payload = {
                    "title": title,
                    "body": replace_block(existing["body"], body),
                    "state": "open",
                }
                actions.append(
                    {
                        "operation": "reopen",
                        "number": existing["number"],
                        "advisory": advisory,
                        "payload": payload,
                    }
                )
                continue
            payload = {"title": title, "body": replace_block(existing["body"], body)}
        else:
            if existing is None or existing["state"] != "open":
                continue
            previous = stored_rows(existing["body"])
            title, body = render(
                advisory,
                [],
                metadata.get(advisory, {}),
                release,
                config,
                resolved=True,
                previous_rows=previous,
            )
            title = existing["title"]
            payload = {
                "title": title,
                "body": replace_block(existing["body"], body),
                "state": "closed",
                "state_reason": "completed",
            }
            if existing["body"] != payload["body"] or existing["state"] != "closed":
                actions.append(
                    {
                        "operation": "close",
                        "number": existing["number"],
                        "advisory": advisory,
                        "payload": payload,
                    }
                )
            continue
        if any(existing.get(key) != value for key, value in payload.items()):
            actions.append(
                {
                    "operation": "update",
                    "number": existing["number"],
                    "advisory": advisory,
                    "payload": payload,
                }
            )
    return actions


class GitHubIssues:
    def __init__(
        self,
        token: str,
        repository: str,
        *,
        opener: Callable[..., Any] = urllib.request.urlopen,
    ):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise ValueError("invalid repository")
        if not token:
            raise ValueError("a GitHub token is required")
        self.token, self.repository, self.opener = token, repository, opener

    def request(
        self, path: str, method: str = "GET", payload: dict | None = None
    ) -> Any:
        data = json.dumps(payload).encode() if payload is not None else None
        # The origin is fixed to the GitHub API over HTTPS; only the path varies.
        request = urllib.request.Request(
            "https://api.github.com" + path,
            data=data,
            method=method,
            headers={
                "Authorization": f"Bearer {self.token}",
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
                "Content-Type": "application/json",
            },
        )
        try:
            with self.opener(request, timeout=30) as response:  # nosec B310
                return json.loads(response.read() or b"null")
        except urllib.error.HTTPError as error:
            # The error owns the response body; close it once inspected.
            with error:
                raise RuntimeError(
                    f"GitHub API {method} {path} failed with HTTP {error.code}"
                ) from error

    def list_issues(self) -> list[dict]:
        issues: list[dict] = []
        page = 1
        while True:
            rows = self.request(
                f"/repos/{self.repository}/issues?state=all&per_page=100&page={page}"
            )
            if not isinstance(rows, list):
                raise TypeError("GitHub issues response must be a list")
            issues.extend(
                row
                for row in rows
                if isinstance(row, dict) and "pull_request" not in row
            )
            if len(rows) < 100:
                return issues
            page += 1

    def closed_by_bot(self, number: int) -> bool:
        last, page = None, 1
        while True:
            events = self.request(
                f"/repos/{self.repository}/issues/{number}/events?per_page=100&page={page}"
            )
            if not isinstance(events, list):
                raise TypeError("GitHub issue events response must be a list")
            for event in events:
                if isinstance(event, dict) and event.get("event") == "closed":
                    actor = event.get("actor")
                    last = actor.get("login") if isinstance(actor, dict) else None
            if len(events) < 100:
                return last == BOT
            page += 1

    def write(self, action: dict) -> dict:
        if action["number"]:
            return self.request(
                f"/repos/{self.repository}/issues/{action['number']}",
                "PATCH",
                action["payload"],
            )
        return self.request(
            f"/repos/{self.repository}/issues", "POST", action["payload"]
        )


def main(argv: list[str] | None = None, *, store: IssueStore | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report-directory", required=True, type=Path)
    parser.add_argument("--release-tag", required=True)
    parser.add_argument("--dry-run", choices=("true", "false"), default="true")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args(argv)
    result: dict = {
        "release": args.release_tag,
        "dry_run": args.dry_run == "true",
        "actions": [],
        "applied": [],
    }
    status = 0
    try:
        grouped, metadata, config = load_scan(args.report_directory)
        store = store or GitHubIssues(
            os.environ.get("GITHUB_TOKEN", ""), os.environ.get("GITHUB_REPOSITORY", "")
        )
        actions = plan(
            grouped,
            metadata,
            config,
            args.release_tag,
            store.list_issues(),
            store.closed_by_bot,
        )
        result["actions"] = [
            {key: action[key] for key in ("operation", "number", "advisory")}
            for action in actions
        ]
        if not result["dry_run"]:
            for action in actions:
                issue = store.write(action)
                result["applied"].append(
                    {
                        "operation": action["operation"],
                        "number": issue.get("number"),
                        "url": issue.get("html_url", ""),
                    }
                )
    except (OSError, ValueError, KeyError, TypeError, RuntimeError) as error:
        status = 2
        result["error"] = str(error)
        print(
            f"::error title=Release Go vulnerability issues::{error}", file=sys.stderr
        )
    if args.output:
        args.output.write_text(json.dumps(result, indent=2) + "\n")
    lines = [f"## Release Go vulnerability issues ({args.release_tag})", ""]
    if "error" in result:
        lines.append(f"Reconciliation failed: {result['error']}")
    elif not result["actions"]:
        lines.append("Issues already match the release govulncheck findings.")
    lines.extend(
        f"- {row['operation']} {row['advisory']}"
        + (f" (#{row['number']})" if row["number"] else "")
        + (" (dry run)" if result["dry_run"] else "")
        for row in result["actions"]
    )
    if path := os.environ.get("GITHUB_STEP_SUMMARY"):
        with Path(path).open("a") as stream:
            stream.write("\n".join(lines) + "\n")
    print("\n".join(lines))
    return status


if __name__ == "__main__":
    raise SystemExit(main())
