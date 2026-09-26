#!/usr/bin/env python3
"""Resolve, stage, and verify Autoscan container image inputs."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import hashlib
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
IMAGE_REPOSITORY = "saltydk/autoscan"
RECIPES = ("master", "release")
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
    before: Mapping[str, str]
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
        raise ImageError("Dockerfile must contain exactly one quoted BASE_IMAGE argument")
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


def recipe_directory(root: Path, recipe: str) -> Path:
    if recipe not in RECIPES:
        raise ImageError(f"unknown image recipe: {recipe}")
    return root / "docker" / recipe


def base_pin(root: Path, recipe: str) -> str:
    return parse_base_image((recipe_directory(root, recipe) / "Dockerfile").read_text(encoding="utf-8"))


def check_release_recipes(root: Path) -> None:
    def files(recipe: str) -> dict[str, tuple[bytes, int]]:
        directory = recipe_directory(root, recipe)
        result = {}
        for path in directory.rglob("*"):
            relative = path.relative_to(directory)
            if relative.parts[0] == "binaries":
                continue  # Binaries are staged by CI, outside the runtime recipe.
            if path.is_symlink():
                raise ImageError(f"recipe contains a symlink: {path}")
            if path.is_file():
                result[str(relative)] = (path.read_bytes(), stat.S_IMODE(path.stat().st_mode))
        if "Dockerfile" not in result or "run" not in result:
            raise ImageError(f"{recipe} recipe is missing Dockerfile or run")
        return result

    master, release = files("master"), files("release")
    changed = sorted(name for name in master.keys() | release.keys() if master.get(name) != release.get(name))
    if changed:
        raise ImageError("release recipe differs from master: " + ", ".join(changed))


def verify_base(root: Path, inspector: Inspector = inspect_image, *, recipe: str = "master") -> str:
    pin = base_pin(root, recipe)
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
    before = {recipe: base_pin(root, recipe) for recipe in RECIPES}
    latest = inspector(f"{BASE_REPOSITORY}:latest")
    after = derive_base_image(latest)
    match = BASE_PATTERN.fullmatch(after)
    assert match is not None
    sha_tag = f"{BASE_REPOSITORY}:sha-{match.group('revision')}"
    validate_base_reference(after, latest, inspector(sha_tag))
    result = BaseUpdate(before=before, after=after, changed=any(pin != after for pin in before.values()))
    if write and result.changed:
        for recipe, pin in before.items():
            if pin != after:
                _replace_base_image(recipe_directory(root, recipe) / "Dockerfile", pin, after)
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
            for recipe, pin in result.before.items():
                stream.write(f"- Before ({recipe}): `{pin}`\n")
            stream.write(f"- After: `{result.after}`\n")


def format_base_update(result: BaseUpdate) -> str:
    if result.changed:
        return f"base images updated to: {result.after}"
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
        _copy_binary(selected[key], destination)


def _copy_binary(source: Path, destination: Path) -> None:
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=destination.parent, prefix=".autoscan-", delete=False) as temporary:
        temporary_path = Path(temporary.name)
    try:
        shutil.copyfile(source, temporary_path)
        os.chmod(temporary_path, 0o755)
        os.replace(temporary_path, destination)
    finally:
        temporary_path.unlink(missing_ok=True)


def _run(command: Sequence[str]) -> str:
    result = subprocess.run(command, capture_output=True, text=True, check=False)
    if result.returncode:
        raise ImageError(result.stderr.strip() or f"{command[0]} failed")
    return result.stdout.strip()


def remote_revision(source_ref: str) -> str:
    output = _run(["git", "ls-remote", "--exit-code", "origin", source_ref, source_ref + "^{}"])
    refs = {ref: sha for sha, ref in (line.split() for line in output.splitlines())}
    revision = refs.get(source_ref + "^{}", refs.get(source_ref, ""))
    if REVISION_PATTERN.fullmatch(revision) is None:
        raise ImageError(f"invalid remote revision for {source_ref}")
    return revision


def resolve_release(tag: str = "") -> tuple[str, str]:
    command = ["gh", "release", "view"]
    if tag:
        command.append(tag)
    command.extend(["--repo", os.environ.get("GITHUB_REPOSITORY", IMAGE_REPOSITORY),
                    "--json", "tagName,isDraft,isPrerelease"])
    release = json.loads(_run(command))
    name = release.get("tagName", "")
    if (release.get("isDraft") or release.get("isPrerelease")
            or not re.fullmatch(r"v[0-9][0-9A-Za-z_.-]{0,126}", name)
            or (tag and name != tag)):
        raise ImageError("expected a published stable v-prefixed release")
    return name, remote_revision("refs/tags/" + name)


def stage_release(root: Path, tag: str, assets: Path) -> None:
    if not re.fullmatch(r"v[0-9][0-9A-Za-z_.-]{0,126}", tag):
        raise ImageError("invalid release tag")
    checksums: dict[str, str] = {}
    for line in (assets / "checksums.txt").read_text(encoding="utf-8").splitlines():
        checksum, name = line.split()
        if name in checksums or re.fullmatch(r"[0-9a-f]{128}", checksum) is None:
            raise ImageError("invalid release checksum manifest")
        checksums[name] = checksum
    binaries = []
    for arch, directory in (("amd64", "linux_amd64"), ("arm64", "linux_arm64"), ("armv7", "linux_arm_7")):
        name = f"autoscan_{tag}_linux_{arch}"
        source = assets / name
        if source.is_symlink() or not source.is_file():
            raise ImageError(f"release binary is missing or not a regular file: {name}")
        if hashlib.sha512(source.read_bytes()).hexdigest() != checksums.get(name):
            raise ImageError(f"release binary checksum mismatch: {name}")
        binaries.append((source, root / "dist" / "docker" / directory / "autoscan"))
    for source, destination in binaries:
        _copy_binary(source, destination)


def download_release(root: Path, tag: str) -> None:
    with tempfile.TemporaryDirectory(prefix="autoscan-release-") as directory:
        command = ["gh", "release", "download", tag, "--repo",
                   os.environ.get("GITHUB_REPOSITORY", IMAGE_REPOSITORY), "--dir", directory,
                   "--pattern", "checksums.txt"]
        for arch in ("amd64", "arm64", "armv7"):
            command.extend(["--pattern", f"autoscan_{tag}_linux_{arch}"])
        _run(command)
        stage_release(root, tag, Path(directory))


def publication_tags(recipe: str, source_ref: str, release_tag: str = "") -> list[str]:
    if recipe == "master" and not release_tag and source_ref.startswith(("refs/heads/", "refs/pull/")):
        return ["master"] if source_ref == "refs/heads/master" else []
    if recipe == "release" and re.fullmatch(r"v[0-9][0-9A-Za-z_.-]{0,126}", release_tag):
        if source_ref == "refs/tags/" + release_tag:
            return [release_tag[1:], "latest"]
        if source_ref == "refs/heads/master":
            return ["latest"]
    raise ImageError("image recipe, source ref, and release tag do not describe a supported publication")


def prepare_image(root: Path, recipe: str, source_ref: str, source_sha: str, release_tag: str = "") -> dict[str, str]:
    tags = publication_tags(recipe, source_ref, release_tag)
    if REVISION_PATTERN.fullmatch(source_sha) is None:
        raise ImageError("invalid image source revision")
    verify_base(root, recipe=recipe)
    binary_sha = source_sha
    if recipe == "release":
        release_tag, binary_sha = resolve_release(release_tag)
        if source_ref.startswith("refs/tags/") and source_sha != binary_sha:
            raise ImageError("release binary source does not match the tagged image source")
        download_release(root, release_tag)
    return {"binary-sha": binary_sha, "short-sha": binary_sha[:7],
            "version": release_tag or "development", "tags": json.dumps(tags)}


def publication_current(metadata: Mapping[str, object], expected_labels: Mapping[str, str]) -> bool:
    try:
        _image_descriptors(metadata)
        labels = _platform_labels(metadata)
        return all(all(values.get(key) == value for key, value in expected_labels.items())
                   for values in labels.values())
    except ImageError:
        return False


def refresh_status(root: Path, inspector: Inspector = inspect_image) -> dict[str, str]:
    source_sha = _run(["git", "rev-parse", "HEAD"])
    tag, binary_sha = resolve_release()
    result = {"source-sha": source_sha, "release-tag": tag, "release-sha": binary_sha}
    for recipe, image_tag in (("master", "master"), ("release", "latest")):
        labels = {"org.opencontainers.image.base.name": base_pin(root, recipe)}
        if recipe == "master":
            labels["org.opencontainers.image.revision"] = source_sha
        else:
            labels["org.opencontainers.image.version"] = tag
            labels["io.autoscan.binary.revision"] = binary_sha
        try:
            current = publication_current(inspector(f"{IMAGE_REPOSITORY}:{image_tag}"), labels)
        except ImageError:
            current = False
        result[f"{recipe}-rebuild"] = str(not current).lower()
    return result


def check_promotion(recipe: str, source_ref: str, source_sha: str, release_tag: str, binary_sha: str) -> None:
    if not publication_tags(recipe, source_ref, release_tag):
        raise ImageError("this source is only eligible for image testing")
    if remote_revision(source_ref) != source_sha:
        raise ImageError("Source ref moved; refusing publication")
    if recipe == "release" and resolve_release() != (release_tag, binary_sha):
        raise ImageError("Latest release changed; refusing publication")


def validate_publication(
    reference: str,
    source_sha: str,
    base_pin: str,
    metadata: Mapping[str, object],
    sbom_metadata: Mapping[str, object],
    *,
    binary_sha: str = "",
    version: str = "",
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
        if binary_sha and platform_labels.get("io.autoscan.binary.revision") != binary_sha:
            raise ImageError(f"{platform} binary source revision label does not match")
        if version and platform_labels.get("org.opencontainers.image.version") != version:
            raise ImageError(f"{platform} application version label does not match")

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
    *,
    recipe: str = "master",
    binary_sha: str = "",
    version: str = "",
) -> str:
    pin = base_pin(root, recipe)
    return validate_publication(
        reference, source_sha, pin, inspector(reference), sbom_inspector(reference),
        binary_sha=binary_sha, version=version,
    )


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--recipe", choices=RECIPES, default="master")
    commands = parser.add_subparsers(dest="command", required=True)
    base = commands.add_parser("base")
    base_commands = base.add_subparsers(dest="base_command", required=True)
    base_commands.add_parser("verify")
    update = base_commands.add_parser("update")
    update.add_argument("--write", action="store_true")
    update.add_argument("--github-output", type=Path)
    update.add_argument("--summary", type=Path)
    commands.add_parser("stage")
    commands.add_parser("check-release")
    prepare = commands.add_parser("prepare")
    promotion = commands.add_parser("check-promotion")
    for command in (prepare, promotion):
        command.add_argument("--source-sha", required=True)
        command.add_argument("--source-ref", required=True)
        command.add_argument("--release-tag", default="")
    promotion.add_argument("--binary-sha", required=True)
    prepare.add_argument("--github-output", type=Path)
    status = commands.add_parser("refresh-status")
    status.add_argument("--github-output", type=Path)
    status.add_argument("--summary", type=Path)
    publication = commands.add_parser("verify-publication")
    publication.add_argument("image_reference")
    publication.add_argument("source_sha")
    publication.add_argument("--binary-sha", default="")
    publication.add_argument("--version", default="")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    root = Path.cwd()
    try:
        if args.command == "base" and args.base_command == "verify":
            print(verify_base(root, recipe=args.recipe))
        elif args.command == "base":
            result = update_base(root, write=args.write)
            _write_update_outputs(result, args.github_output, args.summary)
            print(format_base_update(result))
        elif args.command == "stage":
            stage_artifacts(root)
        elif args.command == "check-release":
            check_release_recipes(root)
            print("Release and master runtime recipes match")
        elif args.command in ("prepare", "refresh-status"):
            if args.command == "prepare":
                result = prepare_image(root, args.recipe, args.source_ref, args.source_sha, args.release_tag)
            else:
                result = refresh_status(root)
                if args.summary:
                    with args.summary.open("a", encoding="utf-8") as stream:
                        stream.write("\n## Image refresh status\n\n")
                        for recipe in RECIPES:
                            state = "pending rebuild" if result[f"{recipe}-rebuild"] == "true" else "current"
                            stream.write(f"- {recipe}: {state}\n")
                        stream.write(f"- Application release: `{result['release-tag']}`\n")
            if args.github_output:
                with args.github_output.open("a", encoding="utf-8") as stream:
                    for key, value in result.items():
                        stream.write(f"{key}={value}\n")
            print(json.dumps(result, indent=2))
        elif args.command == "check-promotion":
            check_promotion(args.recipe, args.source_ref, args.source_sha, args.release_tag, args.binary_sha)
        else:
            print(verify_publication(root, args.image_reference, args.source_sha, recipe=args.recipe,
                                     binary_sha=args.binary_sha, version=args.version))
    except (ImageError, OSError, ValueError) as error:
        print(f"image: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
