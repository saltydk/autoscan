#!/usr/bin/env python3
"""Resolve, stage, and verify Autoscan container image inputs."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import stat

# The CLI launches approved Git/Docker/GitHub tools through _execute, without a shell.
import subprocess  # nosec B404
import sys
import tempfile
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path

BASE_REPOSITORY = "saltydk/alpine-s6overlay"
NONROOT_BASE = "gcr.io/distroless/static-debian13:nonroot"
IMAGE_REPOSITORY = "saltydk/autoscan"
RECIPES = ("master", "release")
VARIANTS = ("standard", "nonroot")
PLATFORMS = ("linux/amd64", "linux/arm64", "linux/arm/v7")
BASE_PATTERN = re.compile(
    rf"^{re.escape(BASE_REPOSITORY)}:sha-(?P<revision>[0-9a-f]{{40}})"
    r"@(?P<digest>sha256:[0-9a-f]{64})$"
)
NONROOT_PATTERN = re.compile(
    rf"^{re.escape(NONROOT_BASE)}@(?P<digest>sha256:[0-9a-f]{{64}})$"
)
DIGEST_PATTERN = re.compile(r"^sha256:[0-9a-f]{64}$")
REVISION_PATTERN = re.compile(r"^[0-9a-f]{40}$")
REFERENCE_PATTERN = re.compile(r"^.+@(?P<digest>sha256:[0-9a-f]{64})$")
BASE_ARG_PATTERN = re.compile(r'^ARG BASE_IMAGE="([^"]+)"[ \t]*$', re.MULTILINE)


class ImageError(RuntimeError):
    """Image metadata or local inputs violate the required contract."""


class ImageNotFoundError(ImageError):
    """The registry has no manifest for the requested image reference."""


@dataclass(frozen=True)
class BaseUpdate:
    before: Mapping[str, str]
    after: str
    changed: bool
    variant: str = "standard"


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


def base_pattern(variant: str) -> re.Pattern[str]:
    if variant == "standard":
        return BASE_PATTERN
    if variant == "nonroot":
        return NONROOT_PATTERN
    raise ImageError(f"unknown image variant: {variant}")


def parse_base_image(dockerfile: str, *, variant: str = "standard") -> str:
    matches = BASE_ARG_PATTERN.findall(dockerfile)
    if len(matches) != 1:
        raise ImageError(
            "Dockerfile must contain exactly one quoted BASE_IMAGE argument"
        )
    pin = matches[0]
    if base_pattern(variant).fullmatch(pin) is None:
        if variant == "nonroot":
            raise ImageError(f"BASE_IMAGE must pin {NONROOT_BASE} by manifest digest")
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
        raise ImageError("image platforms must be exactly " + ", ".join(PLATFORMS))
    return result


def _platform_labels(metadata: Mapping[str, object]) -> dict[str, Mapping[str, object]]:
    images = _mapping(metadata.get("image"), "platform image metadata")
    if set(images) != set(PLATFORMS):
        raise ImageError(
            "platform image metadata must cover exactly the required platforms"
        )
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
        _string(
            platform_labels, "org.opencontainers.image.revision", f"{platform} labels"
        )
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
            raise ImageError(
                "base image manifest digest does not match the pinned digest"
            )
        raise ImageError("base image OCI revision does not match the pinned SHA tag")
    if _manifest_digest(sha_tag_metadata) != match.group("digest"):
        raise ImageError("base image SHA tag has moved from the pinned manifest digest")


def validate_nonroot_base(pin: str, metadata: Mapping[str, object]) -> None:
    match = NONROOT_PATTERN.fullmatch(pin)
    if match is None or _manifest_digest(metadata) != match.group("digest"):
        raise ImageError("nonroot base manifest does not match the pinned digest")
    manifests = _manifest(metadata).get("manifests")
    if not isinstance(manifests, list):
        raise ImageError("nonroot base is missing platform descriptors")

    # Distroless also publishes other architectures and spells arm64 as arm64/v8.
    def normalize(platform):
        return "linux/arm64" if platform == "linux/arm64/v8" else platform

    platforms = set()
    for raw in manifests:
        descriptor = _mapping(raw, "nonroot base descriptor")
        platform = normalize(_descriptor_platform(descriptor))
        if platform is None:
            continue
        digest = _string(descriptor, "digest", "nonroot base descriptor")
        if DIGEST_PATTERN.fullmatch(digest) is None:
            raise ImageError(f"nonroot base contains an invalid digest for {platform}")
        if platform in platforms:
            raise ImageError(f"nonroot base contains duplicate platform {platform}")
        platforms.add(platform)
    if not set(PLATFORMS).issubset(platforms):
        raise ImageError("nonroot base does not cover the required platforms")
    images = _mapping(metadata.get("image"), "nonroot base image metadata")
    users = {}
    for platform, raw in images.items():
        platform = normalize(platform)
        if platform not in PLATFORMS:
            continue
        if platform in users:
            raise ImageError(f"nonroot base contains duplicate metadata for {platform}")
        config = _mapping(
            _mapping(raw, "nonroot platform image").get("config"),
            "nonroot image config",
        )
        users[platform] = config.get("User")
    if set(users) != set(PLATFORMS) or any(
        user not in ("65532", "65532:65532") for user in users.values()
    ):
        raise ImageError("nonroot base must use UID 65532 on every required platform")


def inspect_image(reference: str) -> Mapping[str, object]:
    return _inspect_format(reference, "{{json .}}", "image metadata")


def inspect_sbom(reference: str) -> Mapping[str, object]:
    return _inspect_format(reference, "{{json .SBOM}}", "SBOM metadata")


def _inspect_format(
    reference: str, template: str, description: str
) -> Mapping[str, object]:
    completed = _execute(
        ["docker", "buildx", "imagetools", "inspect", reference, "--format", template],
    )
    if completed.returncode:
        detail = completed.stderr.strip() or completed.stdout.strip() or "unknown error"
        missing = rf"(?:ERROR: )?(?:docker\.io/)?{re.escape(reference)}: (?:not found|manifest unknown(?:: manifest unknown)?)"
        if re.fullmatch(missing, detail):
            raise ImageNotFoundError(f"failed to inspect {reference}: {detail}")
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


def dockerfile_path(root: Path, recipe: str, variant: str = "standard") -> Path:
    base_pattern(variant)
    name = "Dockerfile.nonroot" if variant == "nonroot" else "Dockerfile"
    return recipe_directory(root, recipe) / name


def base_pin(root: Path, recipe: str, variant: str = "standard") -> str:
    return parse_base_image(
        dockerfile_path(root, recipe, variant).read_text(encoding="utf-8"),
        variant=variant,
    )


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
                result[str(relative)] = (
                    path.read_bytes(),
                    stat.S_IMODE(path.stat().st_mode),
                )
        if "Dockerfile" not in result or "run" not in result:
            raise ImageError(f"{recipe} recipe is missing Dockerfile or run")
        return result

    master, release = files("master"), files("release")
    changed = sorted(
        name
        for name in master.keys() | release.keys()
        if master.get(name) != release.get(name)
    )
    if changed:
        raise ImageError("release recipe differs from master: " + ", ".join(changed))


def verify_base(
    root: Path,
    inspector: Inspector = inspect_image,
    *,
    recipe: str = "master",
    variant: str = "standard",
) -> str:
    pin = base_pin(root, recipe, variant)
    if variant == "nonroot":
        validate_nonroot_base(pin, inspector(pin))
        return pin
    match = BASE_PATTERN.fullmatch(pin)
    if match is None:
        raise ImageError("standard base image pin is invalid")
    sha_tag = f"{BASE_REPOSITORY}:sha-{match.group('revision')}"
    validate_base_reference(pin, inspector(pin), inspector(sha_tag))
    return pin


def _replace_base_image(path: Path, before: str, after: str) -> None:
    original = path.read_text(encoding="utf-8")
    replacement = f'ARG BASE_IMAGE="{after}"'
    rendered, count = re.subn(
        rf'^ARG BASE_IMAGE="{re.escape(before)}"[ \t]*$',
        replacement,
        original,
        count=1,
        flags=re.MULTILINE,
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


def update_base(
    root: Path,
    inspector: Inspector = inspect_image,
    *,
    write: bool = False,
    variant: str = "standard",
) -> BaseUpdate:
    before = {recipe: base_pin(root, recipe, variant) for recipe in RECIPES}
    if variant == "nonroot":
        latest = inspector(NONROOT_BASE)
        after = f"{NONROOT_BASE}@{_manifest_digest(latest)}"
        validate_nonroot_base(after, latest)
    else:
        latest = inspector(f"{BASE_REPOSITORY}:latest")
        after = derive_base_image(latest)
        match = BASE_PATTERN.fullmatch(after)
        if match is None:
            raise ImageError("resolved standard base image pin is invalid")
        sha_tag = f"{BASE_REPOSITORY}:sha-{match.group('revision')}"
        validate_base_reference(after, latest, inspector(sha_tag))
    result = BaseUpdate(
        before=before,
        after=after,
        changed=any(pin != after for pin in before.values()),
        variant=variant,
    )
    if write and result.changed:
        for recipe, pin in before.items():
            if pin != after:
                _replace_base_image(dockerfile_path(root, recipe, variant), pin, after)
    return result


def _write_update_outputs(
    result: BaseUpdate, output: Path | None, summary: Path | None
) -> None:
    if output is not None:
        output.parent.mkdir(parents=True, exist_ok=True)
        with output.open("a", encoding="utf-8") as stream:
            stream.write(f"changed={'true' if result.changed else 'false'}\n")
            stream.write(f"base-image={result.after}\n")
    if summary is not None:
        summary.parent.mkdir(parents=True, exist_ok=True)
        status = "changed" if result.changed else "unchanged"
        with summary.open("a", encoding="utf-8") as stream:
            stream.write(f"## Base image update: {result.variant}\n\n")
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
        if (
            not isinstance(raw, dict)
            or raw.get("type") != "Binary"
            or raw.get("goos") != "linux"
        ):
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
            raise ImageError(
                f"artifact is unavailable: {path_value}: {error}"
            ) from error
        if candidate.is_symlink() or not stat.S_ISREG(source_stat.st_mode):
            raise ImageError(f"artifact is not a regular file: {path_value}")
        if key in selected and selected[key] != source:
            raise ImageError(
                f"duplicate artifact for linux/{key[0]}{('/' + key[1]) if key[1] else ''}"
            )
        selected[key] = source

    missing = [key for key in targets if key not in selected]
    if missing:
        rendered = ", ".join(
            f"linux/{arch}{('/' + arm) if arm else ''}" for arch, arm in missing
        )
        raise ImageError(f"missing required binary artifacts: {rendered}")

    for key, destination in targets.items():
        _copy_binary(selected[key], destination)


def _copy_binary(source: Path, destination: Path) -> None:
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(
        dir=destination.parent, prefix=".autoscan-", delete=False
    ) as temporary:
        temporary_path = Path(temporary.name)
    try:
        shutil.copyfile(source, temporary_path)
        # Public release binaries require read/execute access; only the owner can write.
        os.chmod(temporary_path, 0o755)  # nosec B103
        os.replace(temporary_path, destination)
    finally:
        temporary_path.unlink(missing_ok=True)


def _execute(command: Sequence[str]) -> subprocess.CompletedProcess[str]:
    if not command or command[0] not in {"docker", "git", "gh"}:
        raise ImageError("unsupported image tooling command")
    executable = shutil.which(command[0])
    if executable is None:
        raise ImageError(f"required image tool is unavailable: {command[0]}")
    # Executables are allowlisted above; callers pass validated values in argv, never shell code.
    return subprocess.run(  # nosec B603
        [executable, *command[1:]],
        capture_output=True,
        text=True,
        check=False,
        shell=False,
    )


def _run(command: Sequence[str]) -> str:
    result = _execute(command)
    if result.returncode:
        raise ImageError(result.stderr.strip() or f"{command[0]} failed")
    return result.stdout.strip()


def remote_revision(source_ref: str) -> str:
    output = _run(
        ["git", "ls-remote", "--exit-code", "origin", source_ref, source_ref + "^{}"]
    )
    refs = {ref: sha for sha, ref in (line.split() for line in output.splitlines())}
    revision = refs.get(source_ref + "^{}", refs.get(source_ref, ""))
    if REVISION_PATTERN.fullmatch(revision) is None:
        raise ImageError(f"invalid remote revision for {source_ref}")
    return revision


def resolve_release(tag: str = "") -> tuple[str, str]:
    command = ["gh", "release", "view"]
    if tag:
        command.append(tag)
    command.extend(
        [
            "--repo",
            os.environ.get("GITHUB_REPOSITORY", IMAGE_REPOSITORY),
            "--json",
            "tagName,isDraft,isPrerelease",
        ]
    )
    release = json.loads(_run(command))
    name = release.get("tagName", "")
    if (
        release.get("isDraft")
        or release.get("isPrerelease")
        or not re.fullmatch(r"v[0-9][0-9A-Za-z_.-]{0,126}", name)
        or (tag and name != tag)
    ):
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
    for arch, directory in (
        ("amd64", "linux_amd64"),
        ("arm64", "linux_arm64"),
        ("armv7", "linux_arm_7"),
    ):
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
        command = [
            "gh",
            "release",
            "download",
            tag,
            "--repo",
            os.environ.get("GITHUB_REPOSITORY", IMAGE_REPOSITORY),
            "--dir",
            directory,
            "--pattern",
            "checksums.txt",
        ]
        for arch in ("amd64", "arm64", "armv7"):
            command.extend(["--pattern", f"autoscan_{tag}_linux_{arch}"])
        _run(command)
        stage_release(root, tag, Path(directory))


def publication_tags(
    recipe: str, source_ref: str, release_tag: str = "", *, variant: str = "standard"
) -> list[str]:
    base_pattern(variant)
    tags = None
    if (
        recipe == "master"
        and not release_tag
        and source_ref.startswith(("refs/heads/", "refs/pull/"))
    ):
        tags = ["master"] if source_ref == "refs/heads/master" else []
    if recipe == "release" and re.fullmatch(
        r"v[0-9][0-9A-Za-z_.-]{0,126}", release_tag
    ):
        if source_ref == "refs/tags/" + release_tag:
            tags = [release_tag[1:], "latest"]
        if source_ref == "refs/heads/master":
            tags = ["latest"]
    if tags is None:
        raise ImageError(
            "image recipe, source ref, and release tag do not describe a supported publication"
        )
    return [tag + "-nonroot" for tag in tags] if variant == "nonroot" else tags


def prepare_image(
    root: Path,
    recipe: str,
    source_ref: str,
    source_sha: str,
    release_tag: str = "",
    *,
    variant: str = "standard",
) -> dict[str, str]:
    tags = publication_tags(recipe, source_ref, release_tag, variant=variant)
    if REVISION_PATTERN.fullmatch(source_sha) is None:
        raise ImageError("invalid image source revision")
    verify_base(root, recipe=recipe, variant=variant)
    binary_sha = source_sha
    if recipe == "release":
        release_tag, binary_sha = resolve_release(release_tag)
        if source_ref.startswith("refs/tags/") and source_sha != binary_sha:
            raise ImageError(
                "release binary source does not match the tagged image source"
            )
        download_release(root, release_tag)
    return {
        "binary-sha": binary_sha,
        "short-sha": binary_sha[:7],
        "version": release_tag or "development",
        "tags": json.dumps(tags),
    }


def publication_current(
    metadata: Mapping[str, object], expected_labels: Mapping[str, str]
) -> bool:
    try:
        _image_descriptors(metadata)
        labels = _platform_labels(metadata)
        return all(
            all(values.get(key) == value for key, value in expected_labels.items())
            for values in labels.values()
        )
    except ImageError:
        return False


def refresh_status(root: Path, inspector: Inspector = inspect_image) -> dict[str, str]:
    source_sha = _run(["git", "rev-parse", "HEAD"])
    tag, binary_sha = resolve_release()
    result = {"source-sha": source_sha, "release-tag": tag, "release-sha": binary_sha}
    for recipe, image_tag in (("master", "master"), ("release", "latest")):
        pending = False
        for variant in VARIANTS:
            labels = {
                "org.opencontainers.image.base.name": base_pin(root, recipe, variant),
                "io.autoscan.variant": variant,
            }
            if recipe == "master":
                labels["org.opencontainers.image.revision"] = source_sha
            else:
                labels["org.opencontainers.image.version"] = tag
                labels["io.autoscan.binary.revision"] = binary_sha
            published_tag = image_tag + ("-nonroot" if variant == "nonroot" else "")
            try:
                current = publication_current(
                    inspector(f"{IMAGE_REPOSITORY}:{published_tag}"), labels
                )
            except ImageError:
                current = False
            result[f"{recipe}-{variant}-rebuild"] = str(not current).lower()
            pending = pending or not current
        result[f"{recipe}-rebuild"] = str(pending).lower()
    return result


def published_promotion_allowed(
    reference: str, source_sha: str, binary_sha: str = ""
) -> bool:
    try:
        metadata = inspect_image(reference)
    except ImageNotFoundError:
        return True
    _image_descriptors(metadata)
    labels = _platform_labels(metadata)

    def revision(key: str) -> str:
        revisions = {
            _string(values, key, "published image") for values in labels.values()
        }
        if (
            len(revisions) != 1
            or REVISION_PATTERN.fullmatch(next(iter(revisions))) is None
        ):
            raise ImageError(
                f"published platforms must share a valid revision for {key}"
            )
        return revisions.pop()

    published = revision("org.opencontainers.image.revision")
    candidate = source_sha
    if binary_sha:
        published_binary = revision("io.autoscan.binary.revision")
        # Stable binaries and runtime recipes can come from different commits.
        # Compare releases by binary lineage, and refreshes by recipe lineage.
        if published_binary != binary_sha:
            published, candidate = published_binary, binary_sha
    if _run(["git", "rev-parse", "--is-shallow-repository"]) != "false":
        raise ImageError("publication ancestry requires complete Git history")
    ancestry = _execute(["git", "merge-base", "--is-ancestor", published, candidate])
    if ancestry.returncode not in (0, 1):
        raise ImageError(
            "failed to check publication ancestry: " + ancestry.stderr.strip()
        )
    if ancestry.returncode == 1:
        print(
            f"image: {reference} already contains a newer or divergent source; skipping promotion",
            file=sys.stderr,
        )
        return False
    return True


def check_promotion(
    recipe: str,
    source_ref: str,
    source_sha: str,
    release_tag: str,
    binary_sha: str,
    *,
    variant: str = "standard",
) -> bool:
    tags = publication_tags(recipe, source_ref, release_tag, variant=variant)
    if not tags:
        raise ImageError("this source is only eligible for image testing")
    if any(
        REVISION_PATTERN.fullmatch(revision) is None
        for revision in (source_sha, binary_sha)
    ):
        raise ImageError("invalid image source or binary revision")
    if recipe == "master":
        return published_promotion_allowed(f"{IMAGE_REPOSITORY}:{tags[0]}", source_sha)
    if source_ref == "refs/heads/master":
        if resolve_release() != (release_tag, binary_sha):
            print(
                "image: Latest release changed; skipping superseded runtime refresh",
                file=sys.stderr,
            )
            return False
        return published_promotion_allowed(
            f"{IMAGE_REPOSITORY}:{tags[0]}", source_sha, binary_sha
        )
    if remote_revision(source_ref) != source_sha:
        raise ImageError("Source ref moved; refusing publication")
    if recipe == "release" and resolve_release() != (release_tag, binary_sha):
        raise ImageError("Latest release changed; refusing publication")
    return True


def validate_publication(
    reference: str,
    source_sha: str,
    base_pin: str,
    metadata: Mapping[str, object],
    sbom_metadata: Mapping[str, object],
    *,
    binary_sha: str = "",
    version: str = "",
    variant: str = "standard",
) -> str:
    reference_match = REFERENCE_PATTERN.fullmatch(reference)
    if reference_match is None:
        raise ImageError("publication reference must include an exact sha256 digest")
    if REVISION_PATTERN.fullmatch(source_sha) is None:
        raise ImageError("source revision must be a lowercase 40-character Git SHA")
    if base_pattern(variant).fullmatch(base_pin) is None:
        raise ImageError("tracked base image pin is malformed")
    if _manifest_digest(metadata) != reference_match.group("digest"):
        raise ImageError(
            "published manifest digest does not match the requested reference"
        )

    descriptors = _image_descriptors(metadata)
    labels = _platform_labels(metadata)
    for platform, platform_labels in labels.items():
        if platform_labels.get("org.opencontainers.image.revision") != source_sha:
            raise ImageError(f"{platform} source revision label does not match")
        if platform_labels.get("org.opencontainers.image.base.name") != base_pin:
            raise ImageError(
                f"{platform} base image label does not match the tracked pin"
            )
        if platform_labels.get("io.autoscan.variant") != variant:
            raise ImageError(f"{platform} image variant label does not match")
        if (
            binary_sha
            and platform_labels.get("io.autoscan.binary.revision") != binary_sha
        ):
            raise ImageError(f"{platform} binary source revision label does not match")
        if (
            version
            and platform_labels.get("org.opencontainers.image.version") != version
        ):
            raise ImageError(f"{platform} application version label does not match")
    if variant == "nonroot":
        images = _mapping(metadata.get("image"), "nonroot publication image metadata")
        for platform in PLATFORMS:
            config = _mapping(
                _mapping(images[platform], "platform image").get("config"),
                "image config",
            )
            if config.get("User") != "65532:65532":
                raise ImageError(
                    f"{platform} nonroot image must use UID/GID 65532:65532"
                )

    sbom = _mapping(sbom_metadata, "publication SBOM metadata")
    if set(sbom) != set(PLATFORMS):
        raise ImageError(
            "publication SBOM metadata must cover exactly the required platforms"
        )
    for platform in PLATFORMS:
        platform_sbom = _mapping(sbom[platform], f"{platform} SBOM metadata")
        spdx = _mapping(platform_sbom.get("SPDX"), f"{platform} SPDX SBOM")
        version = _string(spdx, "spdxVersion", f"{platform} SPDX SBOM")
        if not version.startswith("SPDX-"):
            raise ImageError(f"{platform} SBOM is not SPDX")

    subjects: list[str] = []
    manifests = _manifest(metadata)["manifests"]
    if not isinstance(manifests, list):
        raise ImageError("publication manifest descriptors must be an array")
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
        raise ImageError(
            "SBOM subject digests do not match the platform image manifests"
        )
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
    variant: str = "standard",
) -> str:
    pin = base_pin(root, recipe, variant)
    return validate_publication(
        reference,
        source_sha,
        pin,
        inspector(reference),
        sbom_inspector(reference),
        binary_sha=binary_sha,
        version=version,
        variant=variant,
    )


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--recipe", choices=RECIPES, default="master")
    parser.add_argument("--variant", choices=VARIANTS, default="standard")
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
    promotion.add_argument("--github-output", type=Path)
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
            print(verify_base(root, recipe=args.recipe, variant=args.variant))
        elif args.command == "base":
            base_update = update_base(root, write=args.write, variant=args.variant)
            _write_update_outputs(base_update, args.github_output, args.summary)
            print(format_base_update(base_update))
        elif args.command == "stage":
            stage_artifacts(root)
        elif args.command == "check-release":
            check_release_recipes(root)
            print("Release and master runtime recipes match")
        elif args.command in ("prepare", "refresh-status"):
            if args.command == "prepare":
                result = prepare_image(
                    root,
                    args.recipe,
                    args.source_ref,
                    args.source_sha,
                    args.release_tag,
                    variant=args.variant,
                )
            else:
                result = refresh_status(root)
                if args.summary:
                    with args.summary.open("a", encoding="utf-8") as stream:
                        stream.write("\n## Image refresh status\n\n")
                        for recipe in RECIPES:
                            state = (
                                "pending rebuild"
                                if result[f"{recipe}-rebuild"] == "true"
                                else "current"
                            )
                            stream.write(f"- {recipe}: {state}\n")
                        stream.write(
                            f"- Application release: `{result['release-tag']}`\n"
                        )
            if args.github_output:
                with args.github_output.open("a", encoding="utf-8") as stream:
                    for key, value in result.items():
                        stream.write(f"{key}={value}\n")
            print(json.dumps(result, indent=2))
        elif args.command == "check-promotion":
            eligible = str(
                check_promotion(
                    args.recipe,
                    args.source_ref,
                    args.source_sha,
                    args.release_tag,
                    args.binary_sha,
                    variant=args.variant,
                )
            ).lower()
            if args.github_output:
                with args.github_output.open("a", encoding="utf-8") as stream:
                    stream.write(f"eligible={eligible}\n")
            print(eligible)
        else:
            print(
                verify_publication(
                    root,
                    args.image_reference,
                    args.source_sha,
                    recipe=args.recipe,
                    binary_sha=args.binary_sha,
                    version=args.version,
                    variant=args.variant,
                )
            )
    except (ImageError, OSError, ValueError) as error:
        print(f"image: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
