#!/usr/bin/env python3
"""Authenticate and verify the binary used by the pinned Docker Scout action."""

from __future__ import annotations

import hashlib
import os
from pathlib import Path
import re
import subprocess
import sys
from tempfile import TemporaryDirectory
import time
from typing import Callable


REPOSITORY = "docker/scout-action"
RETRY_DELAYS = (5, 15)
TRANSIENT_DOWNLOAD = re.compile(
    r"HTTP (?:408|429|500|502|503|504)\b|timed? out|timeout|connection reset|"
    r"temporary failure|unexpected EOF|TLS handshake timeout",
    re.IGNORECASE,
)
Runner = Callable[[list[str]], subprocess.CompletedProcess[str]]


class PreparationError(RuntimeError):
    """The pinned Scout binary could not be prepared safely."""


def scout_tag(metadata: Path) -> str:
    refs = re.findall(r"(?m)^\s*(?:-\s+)?uses:\s*docker/scout-action@(\S+)", metadata.read_text())
    if len(refs) != 1 or not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", refs[0]):
        raise PreparationError("Scout must have exactly one full release tag in the wrapper's uses reference")
    return refs[0]


def binary_name(runner_os: str, runner_arch: str) -> str:
    if runner_os != "Linux" or runner_arch not in {"X64", "ARM64"}:
        raise PreparationError(f"unsupported Scout runner: {runner_os}/{runner_arch}")
    architecture = {"X64": "amd64", "ARM64": "arm64"}[runner_arch]
    return f"docker-scout-action_linux_{architecture}"


def verified(binary: Path, checksums: Path, tag: str) -> bool:
    if not binary.is_file() or not checksums.is_file():
        return False
    entries = [line.split() for line in checksums.read_text().splitlines()]
    # Scout's published checksums can retain the build filename, while the
    # uploaded asset omits the repeated version and platform suffix.
    platform = binary.name.removeprefix("docker-scout-action_")
    names = {binary.name, f"{binary.name}_{tag[1:]}_{platform}"}
    digests = [parts[0] for parts in entries if len(parts) == 2 and parts[1] in names]
    if len(digests) != 1 or not re.fullmatch(r"[0-9a-f]{64}", digests[0]):
        return False
    with binary.open("rb") as stream:
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    return digest == digests[0]


def run(command: list[str]) -> subprocess.CompletedProcess[str]:
    return subprocess.run(command, capture_output=True, text=True, check=False, timeout=300)


def prepare(
    metadata: Path, runner_workspace: Path, runner_os: str, runner_arch: str,
    *, runner: Runner = run, sleep: Callable[[float], None] = time.sleep,
) -> Path:
    tag = scout_tag(metadata)
    name = binary_name(runner_os, runner_arch)
    # The runner caches repository actions under <work>/_actions/<owner>/<repo>/<ref>.
    action = runner_workspace.parent / "_actions" / REPOSITORY / tag
    if not (action / "index.js").is_file() or not (action / "action.yaml").is_file():
        raise PreparationError(f"the runner has not prepared {REPOSITORY}@{tag} at {action}")
    dist = action / "dist"
    dist.mkdir(exist_ok=True)
    binary = dist / name
    checksum_name = f"docker-scout_{tag[1:]}_checksums.txt"
    checksums = dist / checksum_name
    if verified(binary, checksums, tag):
        binary.chmod(0o755)
        return binary

    with TemporaryDirectory(prefix=".scout-download-", dir=dist) as temporary:
        download = Path(temporary)
        command = ["gh", "release", "download", tag, "--repo", REPOSITORY,
                   "--pattern", name, "--pattern", checksum_name, "--dir", str(download), "--clobber"]
        for attempt in range(len(RETRY_DELAYS) + 1):
            try:
                result = runner(command)
                detail = result.stderr.strip() or result.stdout.strip() or f"gh exited with status {result.returncode}"
                if result.returncode == 0:
                    break
            except subprocess.TimeoutExpired:
                detail = "download timed out"
            if attempt == len(RETRY_DELAYS) or not TRANSIENT_DOWNLOAD.search(detail):
                raise PreparationError(f"authenticated Scout download failed: {detail}")
            print(f"Scout download failed temporarily; retrying in {RETRY_DELAYS[attempt]} seconds.")
            sleep(RETRY_DELAYS[attempt])

        downloaded_binary = download / name
        downloaded_checksums = download / checksum_name
        if not verified(downloaded_binary, downloaded_checksums, tag):
            raise PreparationError(f"Scout {tag} download is missing or does not match its published checksum")
        downloaded_binary.chmod(0o755)
        os.replace(downloaded_checksums, checksums)
        os.replace(downloaded_binary, binary)
    return binary


def main() -> int:
    try:
        if not os.environ.get("GH_TOKEN"):
            raise PreparationError("a GitHub token is required; refusing an anonymous Scout download")
        required = ("RUNNER_WORKSPACE", "RUNNER_OS", "RUNNER_ARCH")
        if any(not os.environ.get(name) for name in required):
            raise PreparationError("RUNNER_WORKSPACE, RUNNER_OS, and RUNNER_ARCH are required")
        binary = prepare(Path(__file__).with_name("action.yml"), Path(os.environ["RUNNER_WORKSPACE"]),
                         os.environ["RUNNER_OS"], os.environ["RUNNER_ARCH"])
        print(f"Verified Scout binary: {binary}")
    except (PreparationError, OSError, UnicodeError) as error:
        print(f"Scout preparation failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
