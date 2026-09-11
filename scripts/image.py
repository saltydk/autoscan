#!/usr/bin/env python3
"""Resolve, stage, and verify Autoscan container image inputs."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import tempfile
from typing import Callable, Mapping, Sequence


BASE_REPOSITORY = "saltydk/alpine-s6overlay"
PLATFORMS = ("linux/amd64", "linux/arm64", "linux/arm/v7")
BASE_PATTERN = re.compile(
    rf"^{re.escape(BASE_REPOSITORY)}:sha-(?P<revision>[0-9a-f]{{40}})"
    r"@(?P<digest>sha256:[0-9a-f]{64})$"
)
DIGEST_PATTERN = re.compile(r"^sha256:[0-9a-f]{64}$")
REVISION_PATTERN = re.compile(r"^[0-9a-f]{40}$")
REFERENCE_PATTERN = re.compile(r"^.+@(?P<digest>sha256:[0-9a-f]{64})$")
BASE_ARG_PATTERN = re.compile(r'^ARG BASE_IMAGE="([^"]+)"[ \t]*$', re.MULTILINE)


class ImageError(RuntimeError):
    """Image metadata or local inputs violate the required contract."""


@dataclass(frozen=True)
class BaseUpdate:
    before: str
    after: str
    changed: bool


Inspector = Callable[[str], Mapping[str, object]]


def _mapping(value: object, description: str) -> Mapping[str, object]:
    if not isinstance(value, dict):
        raise ImageError(f"{description} must be a JSON object")
    return value


def _string(data: Mapping[str, object], key: str, description: str) -> str:
    value = data.get(key)
    if not isinstance(value, str) or not value:
        raise ImageError(f"{description} is missing {key}")
    return value


def parse_base_image(dockerfile: str) -> str:
    matches = BASE_ARG_PATTERN.findall(dockerfile)
    if len(matches) != 1:
        raise ImageError("docker/Dockerfile must contain exactly one quoted BASE_IMAGE argument")
    pin = matches[0]
    if BASE_PATTERN.fullmatch(pin) is None:
        raise ImageError(
            "BASE_IMAGE must contain the alpine-s6overlay source SHA tag and manifest digest"
        )
    return pin


def _manifest(metadata: Mapping[str, object]) -> Mapping[str, object]:
    return _mapping(metadata.get("manifest"), "image manifest")


def _manifest_digest(metadata: Mapping[str, object]) -> str:
    digest = _string(_manifest(metadata), "digest", "image manifest")
    if DIGEST_PATTERN.fullmatch(digest) is None:
        raise ImageError("image manifest has an invalid digest")
    return digest


def _descriptor_platform(descriptor: Mapping[str, object]) -> str | None:
    platform = _mapping(descriptor.get("platform"), "manifest platform")
    os_name = _string(platform, "os", "manifest platform")
    architecture = _string(platform, "architecture", "manifest platform")
    if architecture == "unknown":
        return None
    variant = platform.get("variant")
    if variant is not None and not isinstance(variant, str):
        raise ImageError("manifest platform variant must be a string")
    return f"{os_name}/{architecture}{('/' + variant) if variant else ''}"


def _image_descriptors(metadata: Mapping[str, object]) -> dict[str, str]:
    manifests = _manifest(metadata).get("manifests")
    if not isinstance(manifests, list):
        raise ImageError("image manifest is missing platform descriptors")
    result: dict[str, str] = {}
    for raw in manifests:
        descriptor = _mapping(raw, "manifest descriptor")
        platform = _descriptor_platform(descriptor)
        if platform is None:
            continue
        digest = _string(descriptor, "digest", f"{platform} descriptor")
        if DIGEST_PATTERN.fullmatch(digest) is None:
            raise ImageError(f"{platform} descriptor has an invalid digest")
        if platform in result:
            raise ImageError(f"image manifest contains duplicate platform {platform}")
        result[platform] = digest
    if set(result) != set(PLATFORMS):
        raise ImageError(
            "image platforms must be exactly " + ", ".join(PLATFORMS)
        )
    return result


def _platform_labels(metadata: Mapping[str, object]) -> dict[str, Mapping[str, object]]:
    images = _mapping(metadata.get("image"), "platform image metadata")
    if set(images) != set(PLATFORMS):
        raise ImageError("platform image metadata must cover exactly the required platforms")
    labels: dict[str, Mapping[str, object]] = {}
    for platform in PLATFORMS:
        image = _mapping(images[platform], f"{platform} image metadata")
        config = _mapping(image.get("config"), f"{platform} image config")
        labels[platform] = _mapping(config.get("Labels"), f"{platform} image labels")
    return labels


def derive_base_image(metadata: Mapping[str, object]) -> str:
    digest = _manifest_digest(metadata)
    _image_descriptors(metadata)
    labels = _platform_labels(metadata)
    revisions = {
        _string(platform_labels, "org.opencontainers.image.revision", f"{platform} labels")
        for platform, platform_labels in labels.items()
    }
    if len(revisions) != 1:
        raise ImageError("base image platforms have inconsistent OCI revisions")
    revision = revisions.pop()
    if REVISION_PATTERN.fullmatch(revision) is None:
        raise ImageError("base image has an invalid OCI revision")
    return f"{BASE_REPOSITORY}:sha-{revision}@{digest}"


def validate_base_reference(
    pin: str,
    metadata: Mapping[str, object],
    sha_tag_metadata: Mapping[str, object],
) -> None:
    match = BASE_PATTERN.fullmatch(pin)
    if match is None:
        raise ImageError("base image pin is malformed")
    derived = derive_base_image(metadata)
    if derived != pin:
        if _manifest_digest(metadata) != match.group("digest"):
            raise ImageError("base image manifest digest does not match the pinned digest")
        raise ImageError("base image OCI revision does not match the pinned SHA tag")
    if _manifest_digest(sha_tag_metadata) != match.group("digest"):
        raise ImageError("base image SHA tag has moved from the pinned manifest digest")


def inspect_image(reference: str) -> Mapping[str, object]:
    return _inspect_format(reference, "{{json .}}", "image metadata")


def inspect_sbom(reference: str) -> Mapping[str, object]:
    return _inspect_format(reference, "{{json .SBOM}}", "SBOM metadata")


def _inspect_format(reference: str, template: str, description: str) -> Mapping[str, object]:
    completed = subprocess.run(
        ["docker", "buildx", "imagetools", "inspect", reference, "--format", template],
        check=False,
        capture_output=True,
        text=True,
    )
    if completed.returncode:
        detail = completed.stderr.strip() or completed.stdout.strip() or "unknown error"
        raise ImageError(f"failed to inspect {reference}: {detail}")
    try:
        value = json.loads(completed.stdout)
    except json.JSONDecodeError as error:
        raise ImageError(f"inspection for {reference} returned invalid JSON") from error
    return _mapping(value, f"{description} inspection for {reference}")


def verify_base(root: Path, inspector: Inspector = inspect_image) -> str:
    pin = parse_base_image((root / "docker" / "Dockerfile").read_text(encoding="utf-8"))
    match = BASE_PATTERN.fullmatch(pin)
    assert match is not None
    sha_tag = f"{BASE_REPOSITORY}:sha-{match.group('revision')}"
    validate_base_reference(pin, inspector(pin), inspector(sha_tag))
    return pin


def _replace_base_image(path: Path, before: str, after: str) -> None:
    original = path.read_text(encoding="utf-8")
    replacement = f'ARG BASE_IMAGE="{after}"'
    rendered, count = re.subn(
        rf'^ARG BASE_IMAGE="{re.escape(before)}"[ \t]*$', replacement, original,
        count=1, flags=re.MULTILINE,
    )
    if count != 1:
        raise ImageError("BASE_IMAGE changed while preparing the update")
    mode = stat.S_IMODE(path.stat().st_mode)
    with tempfile.NamedTemporaryFile(
        mode="w", encoding="utf-8", dir=path.parent, prefix=".Dockerfile-", delete=False
    ) as temporary:
        temporary.write(rendered)
        temporary.flush()
        os.fsync(temporary.fileno())
        temporary_path = Path(temporary.name)
    try:
        os.chmod(temporary_path, mode)
        os.replace(temporary_path, path)
    finally:
        temporary_path.unlink(missing_ok=True)


def update_base(root: Path, inspector: Inspector = inspect_image, *, write: bool = False) -> BaseUpdate:
    path = root / "docker" / "Dockerfile"
    before = parse_base_image(path.read_text(encoding="utf-8"))
    latest = inspector(f"{BASE_REPOSITORY}:latest")
    after = derive_base_image(latest)
    match = BASE_PATTERN.fullmatch(after)
    assert match is not None
    sha_tag = f"{BASE_REPOSITORY}:sha-{match.group('revision')}"
    validate_base_reference(after, latest, inspector(sha_tag))
    result = BaseUpdate(before=before, after=after, changed=before != after)
    if write and result.changed:
        _replace_base_image(path, before, after)
    return result


def _write_update_outputs(result: BaseUpdate, output: Path | None, summary: Path | None) -> None:
    if output is not None:
        output.parent.mkdir(parents=True, exist_ok=True)
        with output.open("a", encoding="utf-8") as stream:
            stream.write(f"changed={'true' if result.changed else 'false'}\n")
            stream.write(f"base-image={result.after}\n")
    if summary is not None:
        summary.parent.mkdir(parents=True, exist_ok=True)
        status = "changed" if result.changed else "unchanged"
        with summary.open("a", encoding="utf-8") as stream:
            stream.write("## Base image update\n\n")
            stream.write(f"- Status: {status}\n")
            stream.write(f"- Before: `{result.before}`\n")
            stream.write(f"- After: `{result.after}`\n")


def format_base_update(result: BaseUpdate) -> str:
    if result.changed:
        return f"base image changed: {result.before} -> {result.after}"
    return f"base image unchanged: {result.after}"


def stage_artifacts(root: Path) -> None:
    dist = (root / "dist").resolve()
    try:
        artifacts = json.loads((dist / "artifacts.json").read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ImageError(f"failed to load dist/artifacts.json: {error}") from error
    if not isinstance(artifacts, list):
        raise ImageError("dist/artifacts.json must contain a JSON array")

    targets = {
        ("amd64", ""): dist / "docker" / "linux_amd64" / "autoscan",
        ("arm64", ""): dist / "docker" / "linux_arm64" / "autoscan",
        ("arm", "7"): dist / "docker" / "linux_arm_7" / "autoscan",
    }
    selected: dict[tuple[str, str], Path] = {}
    for raw in artifacts:
        if not isinstance(raw, dict) or raw.get("type") != "Binary" or raw.get("goos") != "linux":
            continue
        key = (raw.get("goarch"), raw.get("goarm") or "")
        if key not in targets:
            continue
        path_value = raw.get("path")
        if not isinstance(path_value, str) or not path_value:
            raise ImageError(f"artifact {key[0]}/{key[1] or 'default'} has no path")
        candidate = root / path_value
        source = candidate.resolve()
        try:
            source.relative_to(dist)
        except ValueError as error:
            raise ImageError(f"artifact path escapes dist: {path_value}") from error
        try:
            source_stat = candidate.lstat()
        except OSError as error:
            raise ImageError(f"artifact is unavailable: {path_value}: {error}") from error
        if candidate.is_symlink() or not stat.S_ISREG(source_stat.st_mode):
            raise ImageError(f"artifact is not a regular file: {path_value}")
        if key in selected and selected[key] != source:
            raise ImageError(f"duplicate artifact for linux/{key[0]}{('/' + key[1]) if key[1] else ''}")
        selected[key] = source

    missing = [key for key in targets if key not in selected]
    if missing:
        rendered = ", ".join(f"linux/{arch}{('/' + arm) if arm else ''}" for arch, arm in missing)
        raise ImageError(f"missing required binary artifacts: {rendered}")

    for key, destination in targets.items():
        destination.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(dir=destination.parent, prefix=".autoscan-", delete=False) as temporary:
            temporary_path = Path(temporary.name)
        try:
            shutil.copyfile(selected[key], temporary_path)
            os.chmod(temporary_path, 0o755)
            os.replace(temporary_path, destination)
        finally:
            temporary_path.unlink(missing_ok=True)


def validate_publication(
    reference: str,
    source_sha: str,
    base_pin: str,
    metadata: Mapping[str, object],
    sbom_metadata: Mapping[str, object],
) -> str:
    reference_match = REFERENCE_PATTERN.fullmatch(reference)
    if reference_match is None:
        raise ImageError("publication reference must include an exact sha256 digest")
    if REVISION_PATTERN.fullmatch(source_sha) is None:
        raise ImageError("source revision must be a lowercase 40-character Git SHA")
    if BASE_PATTERN.fullmatch(base_pin) is None:
        raise ImageError("tracked base image pin is malformed")
    if _manifest_digest(metadata) != reference_match.group("digest"):
        raise ImageError("published manifest digest does not match the requested reference")

    descriptors = _image_descriptors(metadata)
    labels = _platform_labels(metadata)
    for platform, platform_labels in labels.items():
        if platform_labels.get("org.opencontainers.image.revision") != source_sha:
            raise ImageError(f"{platform} source revision label does not match")
        if platform_labels.get("org.opencontainers.image.base.name") != base_pin:
            raise ImageError(f"{platform} base image label does not match the tracked pin")

    sbom = _mapping(sbom_metadata, "publication SBOM metadata")
    if set(sbom) != set(PLATFORMS):
        raise ImageError("publication SBOM metadata must cover exactly the required platforms")
    for platform in PLATFORMS:
        platform_sbom = _mapping(sbom[platform], f"{platform} SBOM metadata")
        spdx = _mapping(platform_sbom.get("SPDX"), f"{platform} SPDX SBOM")
        version = _string(spdx, "spdxVersion", f"{platform} SPDX SBOM")
        if not version.startswith("SPDX-"):
            raise ImageError(f"{platform} SBOM is not SPDX")

    subjects: list[str] = []
    manifests = _manifest(metadata)["manifests"]
    assert isinstance(manifests, list)
    for raw in manifests:
        descriptor = _mapping(raw, "manifest descriptor")
        annotations = descriptor.get("annotations")
        if not isinstance(annotations, dict):
            continue
        if annotations.get("vnd.docker.reference.type") != "attestation-manifest":
            continue
        subject = annotations.get("vnd.docker.reference.digest")
        if not isinstance(subject, str) or DIGEST_PATTERN.fullmatch(subject) is None:
            raise ImageError("attestation has an invalid SBOM subject digest")
        subjects.append(subject)
    expected_subjects = set(descriptors.values())
    if set(subjects) != expected_subjects:
        raise ImageError("SBOM subject digests do not match the platform image manifests")
    return f"{reference} ({source_sha})"


def verify_publication(
    root: Path,
    reference: str,
    source_sha: str,
    inspector: Inspector = inspect_image,
    sbom_inspector: Inspector = inspect_sbom,
) -> str:
    base_pin = parse_base_image((root / "docker" / "Dockerfile").read_text(encoding="utf-8"))
    return validate_publication(
        reference, source_sha, base_pin, inspector(reference), sbom_inspector(reference)
    )


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    base = commands.add_parser("base")
    base_commands = base.add_subparsers(dest="base_command", required=True)
    base_commands.add_parser("verify")
    update = base_commands.add_parser("update")
    update.add_argument("--write", action="store_true")
    update.add_argument("--github-output", type=Path)
    update.add_argument("--summary", type=Path)
    commands.add_parser("stage")
    publication = commands.add_parser("verify-publication")
    publication.add_argument("image_reference")
    publication.add_argument("source_sha")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    root = Path.cwd()
    try:
        if args.command == "base" and args.base_command == "verify":
            print(verify_base(root))
        elif args.command == "base":
            result = update_base(root, write=args.write)
            _write_update_outputs(result, args.github_output, args.summary)
            print(format_base_update(result))
        elif args.command == "stage":
            stage_artifacts(root)
        else:
            print(verify_publication(root, args.image_reference, args.source_sha))
    except (ImageError, OSError) as error:
        print(f"image: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
