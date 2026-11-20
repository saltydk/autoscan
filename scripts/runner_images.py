"""List the GitHub-hosted Ubuntu runner labels from the runner-images README.

The "Available Images" table in actions/runner-images is GitHub's published
list of hosted runner images and their YAML labels. One label is chosen per
Ubuntu image, preferring the versioned label over a moving ``-latest`` alias,
so each image is tested exactly once.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from collections.abc import Sequence
from pathlib import Path

TABLE_HEADING = "## Available Images"
LABEL_PATTERN = re.compile(r"`([^`]+)`")
VALID_LABEL = re.compile(r"ubuntu-[a-z0-9][a-z0-9.-]*")
STATUS_PATTERN = re.compile(r"img\.shields\.io/badge/([a-z]+)-", re.IGNORECASE)


class RunnerImagesError(ValueError):
    """Raised when the runner-images README cannot be turned into a matrix."""


def _cells(line: str) -> list[str]:
    return [cell.strip() for cell in line.strip().strip("|").split("|")]


def _table(readme: str) -> tuple[list[str], list[list[str]]]:
    lines = readme.splitlines()
    try:
        start = next(
            index
            for index, line in enumerate(lines)
            if line.strip().casefold() == TABLE_HEADING.casefold()
        )
    except StopIteration:
        raise RunnerImagesError(f"README has no {TABLE_HEADING!r} section") from None
    rows: list[str] = []
    for line in lines[start + 1 :]:
        if line.startswith("|"):
            rows.append(line)
        elif rows or line.startswith("#"):
            break
    if len(rows) < 3:
        raise RunnerImagesError("Available Images table has no image rows")
    header = [cell.casefold() for cell in _cells(rows[0])]
    if not all(re.fullmatch(r":?-+:?", cell) for cell in _cells(rows[1])):
        raise RunnerImagesError("Available Images table has no separator row")
    return header, [_cells(row) for row in rows[2:]]


def _column(header: list[str], name: str) -> int:
    try:
        return header.index(name)
    except ValueError:
        raise RunnerImagesError(
            f"Available Images table has no {name!r} column"
        ) from None


def ubuntu_runners(readme: str) -> list[dict[str, str]]:
    """Return one matrix entry per Ubuntu image in the Available Images table."""
    header, rows = _table(readme)
    image_column = _column(header, "image")
    arch_column = _column(header, "architecture")
    label_column = _column(header, "yaml label")
    runners: list[dict[str, str]] = []
    seen: set[str] = set()
    for row in rows:
        if len(row) != len(header):
            raise RunnerImagesError(f"malformed Available Images row: {row!r}")
        image_cell = row[image_column]
        name = image_cell.split("<br>", 1)[0]
        name = re.sub(r"\[!\[[^\]]*\]\([^)]*\)\]\([^)]*\)", "", name).strip()
        if not name.casefold().startswith("ubuntu"):
            continue
        labels = LABEL_PATTERN.findall(row[label_column])
        if not labels:
            raise RunnerImagesError(f"{name} has no YAML label")
        for label in labels:
            if not VALID_LABEL.fullmatch(label):
                raise RunnerImagesError(f"{name} has an unexpected label {label!r}")
        versioned = [label for label in labels if not label.endswith("-latest")]
        label = (versioned or labels)[0]
        if label in seen:
            raise RunnerImagesError(f"label {label!r} is listed twice")
        seen.add(label)
        status = STATUS_PATTERN.search(image_cell)
        runners.append(
            {
                "runner": label,
                "image": name,
                "architecture": row[arch_column],
                "status": status.group(1).casefold() if status else "available",
            }
        )
    if not runners:
        raise RunnerImagesError("Available Images table lists no Ubuntu images")
    return runners


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("readme", type=Path, help="runner-images README.md")
    parser.add_argument("--github-output", type=Path)
    parser.add_argument("--summary", type=Path)
    args = parser.parse_args(argv)
    try:
        runners = ubuntu_runners(args.readme.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, RunnerImagesError) as error:
        print(f"::error::Runner discovery failed: {error}", file=sys.stderr)
        return 2
    matrix = json.dumps({"include": runners}, separators=(",", ":"))
    if args.github_output:
        with args.github_output.open("a", encoding="utf-8") as stream:
            stream.write(f"matrix={matrix}\n")
    if args.summary:
        with args.summary.open("a", encoding="utf-8") as stream:
            stream.write("## GitHub-hosted Ubuntu runners\n\n")
            stream.write("| Label | Image | Architecture | Status |\n")
            stream.write("| --- | --- | --- | --- |\n")
            for runner in runners:
                stream.write(
                    f"| `{runner['runner']}` | {runner['image']} "
                    f"| {runner['architecture']} | {runner['status']} |\n"
                )
    for runner in runners:
        print(runner["runner"])
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
