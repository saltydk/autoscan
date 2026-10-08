"""Report govulncheck failures without an annotation for every call trace."""
from __future__ import annotations

import argparse
import html
import json
import os
from pathlib import Path
import re
import subprocess


def clean(value: str) -> str:
    for key in ("GITHUB_TOKEN", "GH_TOKEN"):
        if token := os.environ.get(key):
            value = value.replace(token, "***")
    return value


def findings(raw: str) -> tuple[list[dict], int]:
    decoder = json.JSONDecoder()
    position = 0
    messages = []
    while position < len(raw):
        if raw[position].isspace():
            position += 1
            continue
        message, position = decoder.raw_decode(raw, position)
        if not isinstance(message, dict):
            raise ValueError("invalid govulncheck message")
        messages.append(message)
    config = messages[0].get("config", {}) if messages else {}
    if (config.get("protocol_version") != "v1.0.0" or
            config.get("scan_level") != "symbol" or config.get("scan_mode") != "source"):
        raise ValueError("govulncheck did not return a complete source/symbol scan configuration")

    rows = {}
    advisory_ids = set()
    for message in messages:
        if "finding" not in message:
            continue
        finding = message["finding"]
        identifier = finding["osv"]
        trace = finding["trace"]
        if not re.fullmatch(r"GO-\d{4}-\d+", identifier) or not isinstance(trace, list) or not trace:
            raise ValueError("invalid govulncheck finding")
        frame = trace[0]
        module = frame["module"]
        version = frame.get("version", "unknown")
        fixed = finding.get("fixed_version", "")
        if not all(isinstance(value, str) for value in (module, version, fixed)) or not module:
            raise ValueError("invalid govulncheck module identity")
        if not frame.get("function"):
            advisory_ids.add(identifier)
            continue
        key = (identifier, module, version, fixed)
        row = rows.setdefault(key, {"id": identifier, "module": module,
                                   "version": version, "fixed": fixed, "packages": set()})
        row["packages"].add(frame["package"])
    reachable_ids = {row["id"] for row in rows.values()}
    return [{**rows[key], "packages": sorted(rows[key]["packages"])} for key in sorted(rows)], len(
        advisory_ids - reachable_ids)


def version(row: dict, key: str) -> str:
    value = row[key]
    return value.removeprefix("v") if row["module"] == "stdlib" else value


def summary(rows: list[dict], advisories: int) -> tuple[str, str]:
    count = len({row["id"] for row in rows})
    noun = "vulnerability" if count == 1 else "vulnerabilities"
    headline = f"Go vulnerability check failed: {count} reachable {noun}." if rows else (
        "Go vulnerability check passed: No reachable vulnerabilities.")
    upgrades = []
    for module in sorted({row["module"] for row in rows}):
        affected = [row for row in rows if row["module"] == module]
        versions = {version(row, "version") for row in affected}
        fixes = {version(row, "fixed") for row in affected}
        if len(versions) == len(fixes) == 1 and "" not in fixes:
            name = "Go" if module == "stdlib" else module
            upgrades.append(f"Upgrade {name} from {next(iter(versions))} to {next(iter(fixes))}.")
    headline = " ".join([headline, *upgrades])
    markdown = f"## Go vulnerability check\n\n{html.escape(headline)}\n\n"
    if rows:
        markdown += "| Vulnerability | Packages | Found in | Fixed in |\n| --- | --- | --- | --- |\n"
        for row in rows:
            found = ("Go " if row["module"] == "stdlib" else row["module"] + " ") + version(row, "version")
            cells = [", ".join(row["packages"]), found, version(row, "fixed") or "No fix advertised"]
            cells = [html.escape(cell).replace("|", "&#124;").replace("\n", " ") for cell in cells]
            identifier = row["id"]
            markdown += f"| [{identifier}](https://pkg.go.dev/vuln/{identifier}) | " + " | ".join(cells) + " |\n"
    if advisories:
        markdown += f"\nAdditional advisories without a reachable call: {advisories}. These do not fail this check.\n"
    markdown += "\nFull findings and call traces are retained in the Go vulnerability report artifact.\n"
    return headline, markdown


def main(argv: list[str] | None = None, *, runner=subprocess.run) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tool", required=True)
    parser.add_argument("--output-directory", required=True, type=Path)
    args = parser.parse_args(argv)
    args.output_directory.mkdir(parents=True, exist_ok=True)
    status = 2
    rows, advisories = [], 0
    try:
        result = runner(["go", "run", args.tool, "-json", "-scan=symbol", "./..."],
                        capture_output=True, text=True, check=False)
        (args.output_directory / "govulncheck.json").write_text(clean(result.stdout))
        (args.output_directory / "stderr.log").write_text(clean(result.stderr))
        if result.returncode:
            detail = " ".join(clean(result.stderr).split())[:1000] or f"exit status {result.returncode}"
            raise ValueError(detail)
        rows, advisories = findings(result.stdout)
        headline, markdown = summary(rows, advisories)
        status = 1 if rows else 0
    except (OSError, ValueError, KeyError, TypeError, AttributeError) as error:
        headline = "Go vulnerability scan failed: " + " ".join(clean(str(error)).split())[:1000]
        markdown = f"## Go vulnerability scan failed\n\n{html.escape(headline)}\n"
        markdown += "\nThe scan could not complete. This is a scanner failure, not a clean assessment.\n"
    (args.output_directory / "report.json").write_text(json.dumps({
        "status": "failed" if status else "passed", "scan_complete": status != 2,
        "headline": headline, "reachable": rows, "advisories": advisories,
    }, indent=2) + "\n")
    if path := os.environ.get("GITHUB_STEP_SUMMARY"):
        with Path(path).open("a") as stream:
            stream.write(markdown)
    if status:
        escaped = headline.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
        print(f"::error title=Go vulnerability check::{escaped}")
    else:
        print(headline)
    return status


if __name__ == "__main__":
    raise SystemExit(main())
