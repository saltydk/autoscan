import hashlib
import json

# Fixture execution uses argument lists in temporary Git repositories, without a shell.
import subprocess  # nosec B404
import tempfile
import unittest
from pathlib import Path
from typing import Any
from unittest.mock import patch

from scripts import image

PLATFORMS = ("linux/amd64", "linux/arm64", "linux/arm/v7")
REVISION = "a" * 40
BASE_DIGEST = "sha256:" + "b" * 64
BASE_PIN = f"saltydk/alpine-s6overlay:sha-{REVISION}@{BASE_DIGEST}"
NONROOT_PIN = f"{image.NONROOT_BASE}@{BASE_DIGEST}"


def image_metadata(
    revision=REVISION, digest=BASE_DIGEST, platforms=PLATFORMS, variant="standard"
):
    images = {}
    manifests = []
    for index, platform in enumerate(platforms, start=1):
        os_name, architecture, *platform_variant = platform.split("/")
        descriptor: dict[str, Any] = {
            "digest": "sha256:" + str(index) * 64,
            "platform": {"os": os_name, "architecture": architecture},
        }
        if platform_variant:
            descriptor["platform"]["variant"] = platform_variant[0]
        manifests.append(descriptor)
        images[platform] = {
            "config": {
                "User": "65532:65532" if variant == "nonroot" else "",
                "Labels": {
                    "org.opencontainers.image.revision": revision,
                    "org.opencontainers.image.base.name": NONROOT_PIN
                    if variant == "nonroot"
                    else BASE_PIN,
                    "io.autoscan.variant": variant,
                },
            }
        }
    return {"manifest": {"digest": digest, "manifests": manifests}, "image": images}


def publication_metadata(source=REVISION, base=BASE_PIN, variant="standard"):
    metadata = image_metadata(revision=source, variant=variant)
    metadata["manifest"]["digest"] = "sha256:" + "f" * 64
    attestations = []
    for platform, descriptor in zip(PLATFORMS, metadata["manifest"]["manifests"]):
        metadata["image"][platform]["config"]["Labels"][
            "org.opencontainers.image.base.name"
        ] = base
        attestations.append(
            {
                "digest": "sha256:" + "e" * 64,
                "platform": {"os": "unknown", "architecture": "unknown"},
                "annotations": {
                    "vnd.docker.reference.type": "attestation-manifest",
                    "vnd.docker.reference.digest": descriptor["digest"],
                },
            }
        )
    metadata["manifest"]["manifests"].extend(attestations)
    return metadata


def publication_sbom():
    return {platform: {"SPDX": {"spdxVersion": "SPDX-2.3"}} for platform in PLATFORMS}


def write_recipes(root):
    for recipe in image.RECIPES:
        directory = root / "docker" / recipe
        directory.mkdir(parents=True)
        (directory / "Dockerfile").write_text(
            f'ARG BASE_IMAGE="{BASE_PIN}"\nFROM ${{BASE_IMAGE}}\n'
        )
        (directory / "Dockerfile.nonroot").write_text(
            f'ARG BASE_IMAGE="{NONROOT_PIN}"\nFROM ${{BASE_IMAGE}}\n'
        )
        (directory / "run").write_text("exec autoscan\n")
        (directory / "run").chmod(0o755)
    return root / "docker" / "release"


class BaseImageTests(unittest.TestCase):
    def test_parse_base_image_requires_one_complete_pin(self):
        dockerfile = f'ARG BASE_IMAGE="{BASE_PIN}"\nFROM ${{BASE_IMAGE}}\n'
        self.assertEqual(image.parse_base_image(dockerfile), BASE_PIN)

        for bad in (
            "saltydk/alpine-s6overlay:latest",
            f"saltydk/alpine-s6overlay:sha-{'a' * 39}@{BASE_DIGEST}",
            f"saltydk/alpine-s6overlay:sha-{REVISION}@sha256:{'b' * 63}",
        ):
            with self.subTest(pin=bad), self.assertRaises(image.ImageError):
                image.parse_base_image(f'ARG BASE_IMAGE="{bad}"\n')

        with self.assertRaises(image.ImageError):
            image.parse_base_image(
                f'ARG BASE_IMAGE="{BASE_PIN}"\nARG BASE_IMAGE="{BASE_PIN}"\n'
            )

    def test_validate_base_rejects_missing_platform_and_inconsistent_revision(self):
        sha_metadata = {"manifest": {"digest": BASE_DIGEST}}
        image.validate_base_reference(BASE_PIN, image_metadata(), sha_metadata)

        with self.assertRaisesRegex(image.ImageError, "platforms"):
            image.validate_base_reference(
                BASE_PIN, image_metadata(platforms=PLATFORMS[:-1]), sha_metadata
            )

        missing_descriptor = image_metadata()
        missing_descriptor["manifest"]["manifests"].pop()
        with self.assertRaisesRegex(image.ImageError, "platforms"):
            image.validate_base_reference(BASE_PIN, missing_descriptor, sha_metadata)

        inconsistent = image_metadata()
        inconsistent["image"]["linux/arm64"]["config"]["Labels"][
            "org.opencontainers.image.revision"
        ] = "c" * 40
        with self.assertRaisesRegex(image.ImageError, "revision"):
            image.validate_base_reference(BASE_PIN, inconsistent, sha_metadata)

    def test_validate_base_rejects_moved_sha_tag(self):
        moved = {"manifest": {"digest": "sha256:" + "c" * 64}}
        with self.assertRaisesRegex(image.ImageError, "SHA tag"):
            image.validate_base_reference(BASE_PIN, image_metadata(), moved)

    def test_update_does_not_write_until_latest_and_sha_tag_are_verified(self):
        new_revision = "c" * 40
        new_digest = "sha256:" + "d" * 64
        new_pin = f"saltydk/alpine-s6overlay:sha-{new_revision}@{new_digest}"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            original = f'ARG BASE_IMAGE="{BASE_PIN}"\n\nFROM ${{BASE_IMAGE}}\n'
            for recipe in image.RECIPES:
                recipe_directory = root / "docker" / recipe
                recipe_directory.mkdir(parents=True)
                (recipe_directory / "Dockerfile").write_text(original)
            dockerfile = root / "docker" / "master" / "Dockerfile"
            release_file = root / "docker" / "release" / "Dockerfile"
            release_file.write_text(original + "# stable recipe\n")

            def inspect(reference):
                if reference.endswith(":latest"):
                    return image_metadata(revision=new_revision, digest=new_digest)
                return {"manifest": {"digest": "sha256:" + "e" * 64}}

            with self.assertRaisesRegex(image.ImageError, "SHA tag"):
                image.update_base(root, inspect, write=True)
            self.assertEqual(dockerfile.read_text(), original)
            self.assertEqual(release_file.read_text(), original + "# stable recipe\n")

            def valid_inspect(reference):
                if reference.endswith(":latest"):
                    return image_metadata(revision=new_revision, digest=new_digest)
                return {"manifest": {"digest": new_digest}}

            result = image.update_base(root, valid_inspect, write=False)
            self.assertEqual(result.after, new_pin)
            self.assertTrue(result.changed)
            self.assertEqual(dockerfile.read_text(), original)

            image.update_base(root, valid_inspect, write=True)
            self.assertEqual(
                dockerfile.read_text(),
                f'ARG BASE_IMAGE="{new_pin}"\n\nFROM ${{BASE_IMAGE}}\n',
            )
            self.assertEqual(
                release_file.read_text(), dockerfile.read_text() + "# stable recipe\n"
            )

    def test_update_outputs_report_before_after_and_change_state(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "github-output"
            summary = Path(directory) / "summary.md"
            result = image.BaseUpdate(
                before={"master": BASE_PIN, "release": BASE_PIN},
                after=BASE_PIN,
                changed=False,
            )

            image._write_update_outputs(result, output, summary)

            self.assertEqual(
                output.read_text(), f"changed=false\nbase-image={BASE_PIN}\n"
            )
            self.assertEqual(
                summary.read_text(),
                "## Base image update: standard\n\n"
                f"- Status: unchanged\n- Before (master): `{BASE_PIN}`\n"
                f"- Before (release): `{BASE_PIN}`\n- After: `{BASE_PIN}`\n",
            )
            self.assertEqual(
                image.format_base_update(result), f"base image unchanged: {BASE_PIN}"
            )

            changed = image.BaseUpdate(
                before=result.before,
                after=BASE_PIN.replace("b" * 64, "c" * 64),
                changed=True,
            )
            self.assertEqual(
                image.format_base_update(changed),
                f"base images updated to: {changed.after}",
            )


class StageTests(unittest.TestCase):
    def test_stage_copies_each_binary_with_executable_mode(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            dist = root / "dist"
            dist.mkdir()
            artifacts = []
            targets = (
                ("amd64", "", b"amd64"),
                ("arm64", "", b"arm64"),
                ("arm", "7", b"armv7"),
            )
            for architecture, goarm, contents in targets:
                source = dist / f"autoscan-{architecture}-{goarm or 'default'}"
                source.write_bytes(contents)
                artifacts.append(
                    {
                        "type": "Binary",
                        "goos": "linux",
                        "goarch": architecture,
                        "goarm": goarm or None,
                        "path": str(source.relative_to(root)),
                        "internal_type": 4,
                    }
                )
                archive_entry = dict(artifacts[-1])
                archive_entry["internal_type"] = 2
                artifacts.append(archive_entry)
            (dist / "artifacts.json").write_text(json.dumps(artifacts))

            unrelated = dist / "docker" / "keep.txt"
            unrelated.parent.mkdir()
            unrelated.write_text("keep")

            image.stage_artifacts(root)

            self.assertEqual(unrelated.read_text(), "keep")

            expected = {
                "linux_amd64": b"amd64",
                "linux_arm64": b"arm64",
                "linux_arm_7": b"armv7",
            }
            for directory_name, contents in expected.items():
                staged = dist / "docker" / directory_name / "autoscan"
                self.assertEqual(staged.read_bytes(), contents)
                self.assertEqual(staged.stat().st_mode & 0o777, 0o755)

    def test_stage_rejects_missing_duplicate_and_traversal(self):
        cases = ("missing", "duplicate", "traversal", "symlink")
        for case in cases:
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                dist = root / "dist"
                dist.mkdir()
                sources = []
                for architecture, goarm in (("amd64", ""), ("arm64", ""), ("arm", "7")):
                    source = dist / f"{architecture}-{goarm}"
                    source.write_text(architecture)
                    sources.append(
                        {
                            "type": "Binary",
                            "goos": "linux",
                            "goarch": architecture,
                            "goarm": goarm or None,
                            "path": str(source.relative_to(root)),
                        }
                    )
                if case == "missing":
                    sources.pop()
                elif case == "duplicate":
                    duplicate = dist / "other-amd64"
                    duplicate.write_text("other")
                    duplicate_entry = dict(sources[0])
                    duplicate_entry["path"] = str(duplicate.relative_to(root))
                    sources.append(duplicate_entry)
                else:
                    if case == "traversal":
                        outside = root / "outside"
                        outside.write_text("outside")
                        sources[0]["path"] = "dist/../outside"
                    else:
                        link = dist / "linked-amd64"
                        link.symlink_to(dist / "amd64-")
                        sources[0]["path"] = str(link.relative_to(root))
                (dist / "artifacts.json").write_text(json.dumps(sources))

                with self.assertRaises(image.ImageError):
                    image.stage_artifacts(root)
                self.assertFalse((dist / "docker").exists())


class PublicationTests(unittest.TestCase):
    def test_nonroot_publication_requires_its_base_variant_and_unprivileged_user(self):
        metadata = publication_metadata(base=NONROOT_PIN, variant="nonroot")
        reference = "saltydk/autoscan@" + metadata["manifest"]["digest"]
        image.validate_publication(
            reference,
            REVISION,
            NONROOT_PIN,
            metadata,
            publication_sbom(),
            variant="nonroot",
        )
        with self.assertRaisesRegex(image.ImageError, "base image pin"):
            image.validate_publication(
                reference,
                REVISION,
                BASE_PIN,
                metadata,
                publication_sbom(),
                variant="nonroot",
            )
        config = metadata["image"]["linux/arm64"]["config"]
        config["User"] = "0"
        with self.assertRaisesRegex(image.ImageError, "UID/GID"):
            image.validate_publication(
                reference,
                REVISION,
                NONROOT_PIN,
                metadata,
                publication_sbom(),
                variant="nonroot",
            )
        config["User"] = "65532:65532"
        config["Labels"]["io.autoscan.variant"] = "standard"
        with self.assertRaisesRegex(image.ImageError, "variant"):
            image.validate_publication(
                reference,
                REVISION,
                NONROOT_PIN,
                metadata,
                publication_sbom(),
                variant="nonroot",
            )

    def test_validate_publication_checks_binary_identity_on_every_platform(self):
        metadata = publication_metadata()
        binary_sha = "c" * 40
        for platform in PLATFORMS:
            labels = metadata["image"][platform]["config"]["Labels"]
            labels["io.autoscan.binary.revision"] = binary_sha
            labels["org.opencontainers.image.version"] = "v1.4.4"
        reference = "saltydk/autoscan@" + metadata["manifest"]["digest"]
        image.validate_publication(
            reference,
            REVISION,
            BASE_PIN,
            metadata,
            publication_sbom(),
            binary_sha=binary_sha,
            version="v1.4.4",
        )
        labels = metadata["image"]["linux/arm64"]["config"]["Labels"]
        labels["io.autoscan.binary.revision"] = "d" * 40
        with self.assertRaisesRegex(image.ImageError, "binary source"):
            image.validate_publication(
                reference,
                REVISION,
                BASE_PIN,
                metadata,
                publication_sbom(),
                binary_sha=binary_sha,
                version="v1.4.4",
            )
        labels["io.autoscan.binary.revision"] = binary_sha
        labels["org.opencontainers.image.version"] = "development"
        with self.assertRaisesRegex(image.ImageError, "application version"):
            image.validate_publication(
                reference,
                REVISION,
                BASE_PIN,
                metadata,
                publication_sbom(),
                binary_sha=binary_sha,
                version="v1.4.4",
            )

    def test_validate_publication_accepts_exact_three_platform_spdx_subjects(self):
        digest = "sha256:" + "f" * 64
        reference = f"saltydk/autoscan@{digest}"
        identity = image.validate_publication(
            reference, REVISION, BASE_PIN, publication_metadata(), publication_sbom()
        )
        self.assertEqual(identity, f"{reference} ({REVISION})")

    def test_validate_publication_rejects_platform_source_and_base_mismatches(self):
        digest = "sha256:" + "f" * 64
        reference = f"saltydk/autoscan@{digest}"

        missing = publication_metadata()
        missing["image"].pop("linux/arm/v7")
        with self.assertRaisesRegex(image.ImageError, "platforms"):
            image.validate_publication(
                reference, REVISION, BASE_PIN, missing, publication_sbom()
            )

        wrong_source = publication_metadata()
        wrong_source["image"]["linux/arm64"]["config"]["Labels"][
            "org.opencontainers.image.revision"
        ] = "d" * 40
        with self.assertRaisesRegex(image.ImageError, "source revision"):
            image.validate_publication(
                reference, REVISION, BASE_PIN, wrong_source, publication_sbom()
            )

        wrong_base = publication_metadata()
        wrong_base["image"]["linux/amd64"]["config"]["Labels"][
            "org.opencontainers.image.base.name"
        ] = "wrong"
        with self.assertRaisesRegex(image.ImageError, "base image"):
            image.validate_publication(
                reference, REVISION, BASE_PIN, wrong_base, publication_sbom()
            )

    def test_validate_publication_rejects_missing_or_wrong_sbom_subject(self):
        digest = "sha256:" + "f" * 64
        reference = f"saltydk/autoscan@{digest}"

        missing = publication_sbom()
        missing.pop("linux/arm64")
        with self.assertRaisesRegex(image.ImageError, "SBOM"):
            image.validate_publication(
                reference, REVISION, BASE_PIN, publication_metadata(), missing
            )

        wrong_subject = publication_metadata()
        wrong_subject["manifest"]["manifests"][-1]["annotations"][
            "vnd.docker.reference.digest"
        ] = "sha256:" + "0" * 64
        with self.assertRaisesRegex(image.ImageError, "SBOM subject"):
            image.validate_publication(
                reference, REVISION, BASE_PIN, wrong_subject, publication_sbom()
            )


class ReleaseRecipeTests(unittest.TestCase):
    def test_gate_accepts_equal_recipes_with_different_staged_binaries(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            write_recipes(root)
            for recipe in image.RECIPES:
                binaries = root / "docker" / recipe / "binaries"
                binaries.mkdir()
                (binaries / "autoscan").write_text(recipe)
            image.check_release_recipes(root)

    def test_gate_rejects_recipe_runtime_base_file_set_and_mode_drift(self):
        for change in (
            "Dockerfile",
            "Dockerfile.nonroot",
            "run",
            "base",
            "extra",
            "missing",
            "mode",
            "symlink",
        ):
            with (
                self.subTest(change=change),
                tempfile.TemporaryDirectory() as directory,
            ):
                root = Path(directory)
                release = write_recipes(root)
                if change in ("Dockerfile", "Dockerfile.nonroot", "run"):
                    path = release / change
                    path.write_text(path.read_text() + "# changed\n")
                elif change == "base":
                    path = release / "Dockerfile"
                    path.write_text(path.read_text().replace("b" * 64, "c" * 64))
                elif change == "extra":
                    (release / "config").write_text("new setting\n")
                elif change == "missing":
                    (release / "run").unlink()
                elif change == "mode":
                    (release / "run").chmod(0o644)
                else:
                    (release / "run").unlink()
                    (release / "run").symlink_to(root / "docker" / "master" / "run")
                with self.assertRaises(image.ImageError):
                    image.check_release_recipes(root)

    def test_base_update_validates_both_recipes_before_writing_either(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            release = write_recipes(root)
            (release / "Dockerfile").write_text("FROM alpine:latest\n")
            master = root / "docker" / "master" / "Dockerfile"
            original = master.read_bytes()
            with self.assertRaises(image.ImageError):
                image.update_base(root, lambda ref: image_metadata(), write=True)
            self.assertEqual(master.read_bytes(), original)


class ReleasedBinaryTests(unittest.TestCase):
    def test_image_preparation_keeps_binary_source_separate_from_recipe_source(self):
        binary_sha = "c" * 40
        with (
            patch.object(image, "verify_base"),
            patch.object(
                image, "resolve_release", return_value=("v1.4.4", binary_sha)
            ) as resolve,
            patch.object(image, "download_release") as download,
        ):
            development = image.prepare_image(
                Path("."), "master", "refs/heads/master", REVISION
            )
            self.assertEqual(development["binary-sha"], REVISION)
            resolve.assert_not_called()
            download.assert_not_called()
            stable = image.prepare_image(
                Path("."), "release", "refs/heads/master", REVISION, "v1.4.4"
            )
            self.assertEqual(stable["binary-sha"], binary_sha)
            self.assertEqual(stable["version"], "v1.4.4")
            self.assertEqual(json.loads(stable["tags"]), ["latest"])
            download.assert_called_once_with(Path("."), "v1.4.4")
            with self.assertRaisesRegex(image.ImageError, "tagged image source"):
                image.prepare_image(
                    Path("."), "release", "refs/tags/v1.4.4", REVISION, "v1.4.4"
                )

    def test_stage_keeps_release_bytes_and_verifies_every_checksum_before_copying(self):
        for failure in (
            None,
            "corrupt",
            "missing-checksum",
            "missing-binary",
            "duplicate-checksum",
        ):
            with (
                self.subTest(failure=failure),
                tempfile.TemporaryDirectory() as directory,
            ):
                root = Path(directory)
                assets = root / "assets"
                assets.mkdir()
                checksums = []
                for arch in ("amd64", "arm64", "armv7"):
                    name = f"autoscan_v1.4.4_linux_{arch}"
                    contents = f"released {arch} binary".encode()
                    (assets / name).write_bytes(contents)
                    checksums.append(
                        f"{hashlib.sha512(contents).hexdigest()}  {name}\n"
                    )
                if failure == "corrupt":
                    (assets / "autoscan_v1.4.4_linux_armv7").write_bytes(b"corrupt")
                elif failure == "missing-checksum":
                    checksums.pop()
                elif failure == "missing-binary":
                    (assets / "autoscan_v1.4.4_linux_armv7").unlink()
                elif failure == "duplicate-checksum":
                    checksums.append(checksums[-1])
                (assets / "checksums.txt").write_text("".join(checksums))
                if failure:
                    with self.assertRaises(image.ImageError):
                        image.stage_release(root, "v1.4.4", assets)
                    self.assertFalse((root / "dist" / "docker").exists())
                else:
                    image.stage_release(root, "v1.4.4", assets)
                    for arch, target in (
                        ("amd64", "linux_amd64"),
                        ("arm64", "linux_arm64"),
                        ("armv7", "linux_arm_7"),
                    ):
                        binary = root / "dist" / "docker" / target / "autoscan"
                        self.assertEqual(
                            binary.read_bytes(),
                            (assets / f"autoscan_v1.4.4_linux_{arch}").read_bytes(),
                        )
                        self.assertEqual(binary.stat().st_mode & 0o777, 0o755)

    def test_resolve_release_uses_a_published_tag_and_peeled_commit(self):
        release = {"tagName": "v1.4.4", "isDraft": False, "isPrerelease": False}
        refs = f"{'c' * 40}\trefs/tags/v1.4.4\n{REVISION}\trefs/tags/v1.4.4^{{}}\n"
        with patch.object(image, "_run", side_effect=[json.dumps(release), refs]):
            self.assertEqual(image.resolve_release(), ("v1.4.4", REVISION))
        for field in ("isDraft", "isPrerelease"):
            with (
                self.subTest(field=field),
                patch.object(
                    image, "_run", return_value=json.dumps({**release, field: True})
                ),
                self.assertRaisesRegex(image.ImageError, "published stable"),
            ):
                image.resolve_release()


class RefreshTests(unittest.TestCase):
    def test_only_releases_and_base_refreshes_publish_latest(self):
        cases: tuple[tuple[str, str, str, list[str]], ...] = (
            ("master", "refs/heads/master", "", ["master"]),
            ("master", "refs/heads/latest", "", []),
            ("master", "refs/pull/12/merge", "", []),
            ("release", "refs/tags/v1.4.4", "v1.4.4", ["1.4.4", "latest"]),
            ("release", "refs/heads/master", "v1.4.4", ["latest"]),
        )
        for recipe, source_ref, tag, expected in cases:
            with self.subTest(recipe=recipe, ref=source_ref):
                self.assertEqual(
                    image.publication_tags(recipe, source_ref, tag), expected
                )
                self.assertEqual(
                    image.publication_tags(recipe, source_ref, tag, variant="nonroot"),
                    [value + "-nonroot" for value in expected],
                )
        for recipe, source_ref, tag in (
            ("master", "refs/tags/v1.4.4", ""),
            ("master", "refs/heads/master", "v1.4.4"),
            ("release", "refs/heads/feature", "v1.4.4"),
            ("release", "refs/tags/v1.4.5", "v1.4.4"),
            ("release", "refs/heads/master", ""),
        ):
            with (
                self.subTest(recipe=recipe, ref=source_ref),
                self.assertRaises(image.ImageError),
            ):
                image.publication_tags(recipe, source_ref, tag)

    def test_status_retries_latest_independently_and_ignores_unrelated_master_commits(
        self,
    ):
        binary_sha = "c" * 40
        master = image_metadata()
        latest = image_metadata(revision="d" * 40)
        nonroot = image_metadata(revision="d" * 40, variant="nonroot")
        for metadata in (latest, nonroot):
            for platform in PLATFORMS:
                labels = metadata["image"][platform]["config"]["Labels"]
                labels["org.opencontainers.image.version"] = "v1.4.4"
                labels["io.autoscan.binary.revision"] = binary_sha
        published = {
            "master": master,
            "master-nonroot": image_metadata(variant="nonroot"),
            "latest": latest,
            "latest-nonroot": nonroot,
        }
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            write_recipes(root)

            def inspect(reference):
                return published[reference.rsplit(":", 1)[1]]

            with (
                patch.object(image, "_run", return_value=REVISION),
                patch.object(
                    image, "resolve_release", return_value=("v1.4.4", binary_sha)
                ),
            ):
                status = image.refresh_status(root, inspect)
                self.assertEqual(
                    (status["master-rebuild"], status["release-rebuild"]),
                    ("false", "false"),
                )
                latest["image"]["linux/arm64"]["config"]["Labels"][
                    "org.opencontainers.image.base.name"
                ] = "old base"
                status = image.refresh_status(root, inspect)
                self.assertEqual(
                    (status["master-rebuild"], status["release-rebuild"]),
                    ("false", "true"),
                )
                latest["image"]["linux/arm64"]["config"]["Labels"][
                    "org.opencontainers.image.base.name"
                ] = BASE_PIN
                nonroot["image"]["linux/arm64"]["config"]["Labels"][
                    "org.opencontainers.image.base.name"
                ] = "old nonroot base"
                status = image.refresh_status(root, inspect)
                self.assertEqual(
                    (
                        status["release-standard-rebuild"],
                        status["release-nonroot-rebuild"],
                    ),
                    ("false", "true"),
                )
                self.assertEqual(status["release-rebuild"], "true")
                latest.clear()
                self.assertEqual(
                    image.refresh_status(root, inspect)["release-rebuild"], "true"
                )

    def test_promotion_rejects_moved_release_source_or_newer_release(self):
        binary_sha = REVISION
        with (
            patch.object(image, "remote_revision", return_value="d" * 40),
            self.assertRaisesRegex(image.ImageError, "Source ref moved"),
        ):
            image.check_promotion(
                "release", "refs/tags/v1.4.4", REVISION, "v1.4.4", REVISION
            )
        with patch.object(image, "remote_revision", return_value=REVISION):
            with patch.object(
                image, "resolve_release", return_value=("v1.4.4", binary_sha)
            ):
                image.check_promotion(
                    "release", "refs/tags/v1.4.4", REVISION, "v1.4.4", binary_sha
                )
            with (
                patch.object(
                    image, "resolve_release", return_value=("v1.4.5", "e" * 40)
                ),
                self.assertRaisesRegex(image.ImageError, "Latest release changed"),
            ):
                image.check_promotion(
                    "release", "refs/tags/v1.4.4", REVISION, "v1.4.4", binary_sha
                )


class MasterPromotionTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="autoscan-promotion-")
        self.addCleanup(directory.cleanup)
        self.repository = Path(directory.name)
        self.run_command = subprocess.run
        self.git("init", "--quiet")
        self.first = self.commit("test: create initial source")
        self.second = self.commit("test: advance source")
        self.third = self.commit("test: advance branch during publication")

    def git(self, *arguments):
        return self.run_command(
            ["git", *arguments],
            cwd=self.repository,
            capture_output=True,
            text=True,
            check=True,
        ).stdout.strip()

    def commit(self, subject):
        self.git(
            "-c",
            "user.name=Tests",
            "-c",
            "user.email=tests@example.invalid",
            "-c",
            "commit.gpgsign=false",
            "commit",
            "--allow-empty",
            "--quiet",
            "-m",
            subject,
        )
        return self.git("rev-parse", "HEAD")

    def check(self, source, published, *, variant="standard"):
        metadata = image_metadata(revision=published, variant=variant)

        def run_git(command, **kwargs):
            return self.run_command(command, cwd=self.repository, **kwargs)

        with (
            patch.object(image, "inspect_image", return_value=metadata) as inspect,
            patch.object(image, "remote_revision", return_value=self.third) as remote,
            patch.object(image.subprocess, "run", side_effect=run_git),
        ):
            result = image.check_promotion(
                "master", "refs/heads/master", source, "", source, variant=variant
            )
            remote.assert_not_called()
            tag = "master-nonroot" if variant == "nonroot" else "master"
            inspect.assert_called_once_with(f"{image.IMAGE_REPOSITORY}:{tag}")
            return result

    def test_branch_can_advance_before_candidate_is_published(self):
        for variant in image.VARIANTS:
            with self.subTest(variant=variant):
                self.assertTrue(self.check(self.second, self.first, variant=variant))

    def test_same_source_can_be_republished(self):
        self.assertTrue(self.check(self.second, self.second))

    def test_older_source_skips_promotion(self):
        self.assertFalse(self.check(self.first, self.second))

    def test_divergent_source_skips_promotion(self):
        self.git("checkout", "--quiet", "-b", "divergent", self.first)
        divergent = self.commit("test: create divergent source")
        self.assertFalse(self.check(divergent, self.second))

    def test_missing_git_history_is_an_error(self):
        with self.assertRaisesRegex(image.ImageError, "ancestry"):
            self.check(self.second, "f" * 40)

    def test_shallow_git_history_is_an_error(self):
        (self.repository / ".git" / "shallow").write_text(self.second + "\n")
        with self.assertRaisesRegex(image.ImageError, "complete Git history"):
            self.check(self.second, self.first)

    def test_registry_failure_is_an_error(self):
        with (
            patch.object(
                image, "inspect_image", side_effect=image.ImageError("unauthorized")
            ),
            patch.object(image, "remote_revision", return_value=self.second),
            self.assertRaisesRegex(image.ImageError, "unauthorized"),
        ):
            image.check_promotion(
                "master", "refs/heads/master", self.second, "", self.second
            )

    def test_missing_published_tag_allows_initial_publication(self):
        with (
            patch.object(
                image,
                "inspect_image",
                side_effect=image.ImageNotFoundError("manifest unknown"),
            ),
            patch.object(image, "remote_revision") as remote,
        ):
            self.assertTrue(
                image.check_promotion(
                    "master", "refs/heads/master", self.second, "", self.second
                )
            )
            remote.assert_not_called()

    def test_inspection_distinguishes_missing_manifests_from_registry_failures(self):
        missing = subprocess.CompletedProcess(
            [], 1, "", "ERROR: docker.io/saltydk/autoscan:master: not found"
        )
        with (
            patch.object(image.shutil, "which", return_value="/fixture/docker"),
            patch.object(image.subprocess, "run", return_value=missing),
            self.assertRaises(image.ImageNotFoundError),
        ):
            image.inspect_image("saltydk/autoscan:master")
        for detail in (
            "unauthorized: authentication required",
            "503 Service Unavailable",
            "ERROR: failed to authorize: credential helper: not found",
            "ERROR: resolving registry address: not found",
        ):
            failure = subprocess.CompletedProcess([], 1, "", detail)
            with (
                self.subTest(detail=detail),
                patch.object(image.shutil, "which", return_value="/fixture/docker"),
                patch.object(image.subprocess, "run", return_value=failure),
            ):
                with self.assertRaises(image.ImageError) as error:
                    image.inspect_image("saltydk/autoscan:master")
                self.assertNotIsInstance(error.exception, image.ImageNotFoundError)

    def test_missing_required_executable_is_not_image_absence(self):
        with (
            patch.object(image.shutil, "which", return_value=None),
            patch.object(image.subprocess, "run") as execute,
            self.assertRaisesRegex(image.ImageError, "required image tool") as error,
        ):
            image.inspect_image("saltydk/autoscan:master")
        self.assertNotIsInstance(error.exception, image.ImageNotFoundError)
        execute.assert_not_called()

    def test_unapproved_executable_is_not_launched(self):
        with (
            patch.object(image.subprocess, "run") as execute,
            self.assertRaisesRegex(image.ImageError, "unsupported image tooling"),
        ):
            image._execute(["sh", "-c", "unexpected command"])
        execute.assert_not_called()

    def check_stable_refresh(
        self, source, published_source, binary, published_binary, *, variant="standard"
    ):
        metadata = image_metadata(revision=published_source, variant=variant)
        for platform in PLATFORMS:
            metadata["image"][platform]["config"]["Labels"][
                "io.autoscan.binary.revision"
            ] = published_binary

        def run_git(command, **kwargs):
            return self.run_command(command, cwd=self.repository, **kwargs)

        with (
            patch.object(image, "inspect_image", return_value=metadata) as inspect,
            patch.object(image, "resolve_release", return_value=("v1.4.5", binary)),
            patch.object(image, "remote_revision", return_value=self.third) as remote,
            patch.object(image.subprocess, "run", side_effect=run_git),
        ):
            result = image.check_promotion(
                "release",
                "refs/heads/master",
                source,
                "v1.4.5",
                binary,
                variant=variant,
            )
            remote.assert_not_called()
            tag = "latest-nonroot" if variant == "nonroot" else "latest"
            inspect.assert_called_once_with(f"{image.IMAGE_REPOSITORY}:{tag}")
            return result

    def test_stable_runtime_refresh_allows_branch_movement(self):
        for variant in image.VARIANTS:
            with self.subTest(variant=variant):
                self.assertTrue(
                    self.check_stable_refresh(
                        self.second, self.first, self.first, self.first, variant=variant
                    )
                )

    def test_stable_runtime_refresh_skips_older_recipe(self):
        self.assertFalse(
            self.check_stable_refresh(self.first, self.second, self.first, self.first)
        )

    def test_new_release_advances_binaries_independently_of_runtime_history(self):
        self.assertTrue(
            self.check_stable_refresh(self.second, self.third, self.second, self.first)
        )

    def test_old_binaries_cannot_replace_new_release_even_with_newer_recipe(self):
        self.assertFalse(
            self.check_stable_refresh(self.third, self.second, self.first, self.second)
        )

    def test_superseded_selected_release_skips_runtime_refresh(self):
        with (
            patch.object(image, "resolve_release", return_value=("v1.4.6", self.third)),
            patch.object(image, "inspect_image") as inspect,
        ):
            self.assertFalse(
                image.check_promotion(
                    "release", "refs/heads/master", self.second, "v1.4.5", self.first
                )
            )
            inspect.assert_not_called()

    def test_superseded_cli_succeeds_and_reports_ineligible(self):
        output = self.repository / "promotion-output"
        metadata = image_metadata(revision=self.second)

        def run_git(command, **kwargs):
            return self.run_command(command, cwd=self.repository, **kwargs)

        with (
            patch.object(image, "inspect_image", return_value=metadata),
            patch.object(image.subprocess, "run", side_effect=run_git),
        ):
            status = image.main(
                [
                    "--recipe",
                    "master",
                    "check-promotion",
                    "--source-ref",
                    "refs/heads/master",
                    "--source-sha",
                    self.first,
                    "--binary-sha",
                    self.first,
                    "--github-output",
                    str(output),
                ]
            )
        self.assertEqual(status, 0)
        self.assertEqual(output.read_text(), "eligible=false\n")

    def test_inconsistent_platform_revisions_are_an_error(self):
        metadata = image_metadata(revision=self.first)
        metadata["image"]["linux/arm64"]["config"]["Labels"][
            "org.opencontainers.image.revision"
        ] = self.second
        with (
            patch.object(image, "inspect_image", return_value=metadata),
            patch.object(image, "remote_revision", return_value=self.second),
            self.assertRaisesRegex(image.ImageError, "revision"),
        ):
            image.check_promotion(
                "master", "refs/heads/master", self.second, "", self.second
            )


class NonrootBaseTests(unittest.TestCase):
    def metadata(self, **kwargs):
        platforms = ("linux/amd64", "linux/arm64/v8", "linux/arm/v7", "linux/s390x")
        return image_metadata(platforms=platforms, variant="nonroot", **kwargs)

    def test_accepts_required_platform_subset_and_arm64_alias(self):
        self.assertEqual(
            image.parse_base_image(
                f'ARG BASE_IMAGE="{NONROOT_PIN}"\n', variant="nonroot"
            ),
            NONROOT_PIN,
        )
        image.validate_nonroot_base(NONROOT_PIN, self.metadata())
        for bad in (
            image.NONROOT_BASE,
            NONROOT_PIN.replace(":nonroot@", ":debug-nonroot@"),
            BASE_PIN,
        ):
            with self.subTest(pin=bad), self.assertRaises(image.ImageError):
                image.parse_base_image(f'ARG BASE_IMAGE="{bad}"\n', variant="nonroot")

    def test_rejects_missing_architecture_wrong_user_and_digest(self):
        for failure in (
            "descriptor",
            "descriptor-digest",
            "config",
            "user",
            "digest",
            "duplicate",
        ):
            with self.subTest(failure=failure):
                metadata = self.metadata()
                if failure == "descriptor":
                    metadata["manifest"]["manifests"].pop(2)
                elif failure == "descriptor-digest":
                    metadata["manifest"]["manifests"][0]["digest"] = "invalid"
                elif failure == "config":
                    metadata["image"].pop("linux/arm/v7")
                elif failure == "user":
                    metadata["image"]["linux/arm64/v8"]["config"]["User"] = "root"
                elif failure == "digest":
                    metadata["manifest"]["digest"] = "sha256:" + "c" * 64
                else:
                    metadata["manifest"]["manifests"].append(
                        metadata["manifest"]["manifests"][0]
                    )
                with self.assertRaises(image.ImageError):
                    image.validate_nonroot_base(NONROOT_PIN, metadata)

    def test_update_changes_both_nonroot_pins_without_touching_standard(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            write_recipes(root)
            before = {
                recipe: (root / "docker" / recipe / "Dockerfile").read_bytes()
                for recipe in image.RECIPES
            }
            digest = "sha256:" + "c" * 64
            result = image.update_base(
                root,
                lambda ref: self.metadata(digest=digest),
                write=True,
                variant="nonroot",
            )
            self.assertTrue(result.changed)
            self.assertEqual(result.variant, "nonroot")
            for recipe in image.RECIPES:
                self.assertEqual(
                    image.base_pin(root, recipe, "nonroot"),
                    f"{image.NONROOT_BASE}@{digest}",
                )
                self.assertEqual(
                    (root / "docker" / recipe / "Dockerfile").read_bytes(),
                    before[recipe],
                )


if __name__ == "__main__":
    unittest.main()
