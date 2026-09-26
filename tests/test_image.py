import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts import image


PLATFORMS = ("linux/amd64", "linux/arm64", "linux/arm/v7")
REVISION = "a" * 40
BASE_DIGEST = "sha256:" + "b" * 64
BASE_PIN = f"saltydk/alpine-s6overlay:sha-{REVISION}@{BASE_DIGEST}"


def image_metadata(revision=REVISION, digest=BASE_DIGEST, platforms=PLATFORMS):
    images = {}
    manifests = []
    for index, platform in enumerate(platforms, start=1):
        os_name, architecture, *variant = platform.split("/")
        descriptor = {
            "digest": "sha256:" + str(index) * 64,
            "platform": {"os": os_name, "architecture": architecture},
        }
        if variant:
            descriptor["platform"]["variant"] = variant[0]
        manifests.append(descriptor)
        images[platform] = {
            "config": {
                "Labels": {
                    "org.opencontainers.image.revision": revision,
                    "org.opencontainers.image.base.name": BASE_PIN,
                }
            }
        }
    return {"manifest": {"digest": digest, "manifests": manifests}, "image": images}


def publication_metadata(source=REVISION, base=BASE_PIN):
    metadata = image_metadata(revision=source)
    metadata["manifest"]["digest"] = "sha256:" + "f" * 64
    attestations = []
    for platform, descriptor in zip(PLATFORMS, metadata["manifest"]["manifests"]):
        metadata["image"][platform]["config"]["Labels"]["org.opencontainers.image.base.name"] = base
        attestations.append({
            "digest": "sha256:" + "e" * 64,
            "platform": {"os": "unknown", "architecture": "unknown"},
            "annotations": {
                "vnd.docker.reference.type": "attestation-manifest",
                "vnd.docker.reference.digest": descriptor["digest"],
            },
        })
    metadata["manifest"]["manifests"].extend(attestations)
    return metadata


def publication_sbom():
    return {
        platform: {"SPDX": {"spdxVersion": "SPDX-2.3"}}
        for platform in PLATFORMS
    }


def write_recipes(root):
    for recipe in image.RECIPES:
        directory = root / "docker" / recipe
        directory.mkdir(parents=True)
        (directory / "Dockerfile").write_text(f'ARG BASE_IMAGE="{BASE_PIN}"\nFROM ${{BASE_IMAGE}}\n')
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
            with self.subTest(pin=bad):
                with self.assertRaises(image.ImageError):
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
                directory = root / "docker" / recipe
                directory.mkdir(parents=True)
                (directory / "Dockerfile").write_text(original)
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
            self.assertEqual(release_file.read_text(), dockerfile.read_text() + "# stable recipe\n")

    def test_update_outputs_report_before_after_and_change_state(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "github-output"
            summary = Path(directory) / "summary.md"
            result = image.BaseUpdate(before={"master": BASE_PIN, "release": BASE_PIN}, after=BASE_PIN, changed=False)

            image._write_update_outputs(result, output, summary)

            self.assertEqual(
                output.read_text(), f"changed=false\nbase-image={BASE_PIN}\n"
            )
            self.assertEqual(
                summary.read_text(),
                "## Base image update\n\n"
                f"- Status: unchanged\n- Before (master): `{BASE_PIN}`\n"
                f"- Before (release): `{BASE_PIN}`\n- After: `{BASE_PIN}`\n",
            )
            self.assertEqual(image.format_base_update(result), f"base image unchanged: {BASE_PIN}")

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
            targets = (("amd64", "", b"amd64"), ("arm64", "", b"arm64"), ("arm", "7", b"armv7"))
            for architecture, goarm, contents in targets:
                source = dist / f"autoscan-{architecture}-{goarm or 'default'}"
                source.write_bytes(contents)
                artifacts.append({
                    "type": "Binary", "goos": "linux", "goarch": architecture,
                    "goarm": goarm or None, "path": str(source.relative_to(root)),
                    "internal_type": 4,
                })
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
                    sources.append({
                        "type": "Binary", "goos": "linux", "goarch": architecture,
                        "goarm": goarm or None, "path": str(source.relative_to(root)),
                    })
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
    def test_validate_publication_checks_binary_identity_on_every_platform(self):
        metadata = publication_metadata()
        binary_sha = "c" * 40
        for platform in PLATFORMS:
            labels = metadata["image"][platform]["config"]["Labels"]
            labels["io.autoscan.binary.revision"] = binary_sha
            labels["org.opencontainers.image.version"] = "v1.4.4"
        reference = "saltydk/autoscan@" + metadata["manifest"]["digest"]
        image.validate_publication(reference, REVISION, BASE_PIN, metadata, publication_sbom(),
                                   binary_sha=binary_sha, version="v1.4.4")
        labels = metadata["image"]["linux/arm64"]["config"]["Labels"]
        labels["io.autoscan.binary.revision"] = "d" * 40
        with self.assertRaisesRegex(image.ImageError, "binary source"):
            image.validate_publication(reference, REVISION, BASE_PIN, metadata, publication_sbom(),
                                       binary_sha=binary_sha, version="v1.4.4")
        labels["io.autoscan.binary.revision"] = binary_sha
        labels["org.opencontainers.image.version"] = "development"
        with self.assertRaisesRegex(image.ImageError, "application version"):
            image.validate_publication(reference, REVISION, BASE_PIN, metadata, publication_sbom(),
                                       binary_sha=binary_sha, version="v1.4.4")

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
            image.validate_publication(reference, REVISION, BASE_PIN, missing, publication_sbom())

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
        for change in ("Dockerfile", "run", "base", "extra", "missing", "mode", "symlink"):
            with self.subTest(change=change), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                release = write_recipes(root)
                if change in ("Dockerfile", "run"):
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
            patch.object(image, "resolve_release", return_value=("v1.4.4", binary_sha)) as resolve,
            patch.object(image, "download_release") as download,
        ):
            development = image.prepare_image(Path("."), "master", "refs/heads/master", REVISION)
            self.assertEqual(development["binary-sha"], REVISION)
            resolve.assert_not_called()
            download.assert_not_called()
            stable = image.prepare_image(Path("."), "release", "refs/heads/master", REVISION, "v1.4.4")
            self.assertEqual(stable["binary-sha"], binary_sha)
            self.assertEqual(stable["version"], "v1.4.4")
            self.assertEqual(json.loads(stable["tags"]), ["latest"])
            download.assert_called_once_with(Path("."), "v1.4.4")
            with self.assertRaisesRegex(image.ImageError, "tagged image source"):
                image.prepare_image(Path("."), "release", "refs/tags/v1.4.4", REVISION, "v1.4.4")

    def test_stage_keeps_release_bytes_and_verifies_every_checksum_before_copying(self):
        for failure in (None, "corrupt", "missing-checksum", "missing-binary", "duplicate-checksum"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                assets = root / "assets"
                assets.mkdir()
                checksums = []
                for arch in ("amd64", "arm64", "armv7"):
                    name = f"autoscan_v1.4.4_linux_{arch}"
                    contents = f"released {arch} binary".encode()
                    (assets / name).write_bytes(contents)
                    checksums.append(f"{hashlib.sha512(contents).hexdigest()}  {name}\n")
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
                    for arch, target in (("amd64", "linux_amd64"), ("arm64", "linux_arm64"), ("armv7", "linux_arm_7")):
                        binary = root / "dist" / "docker" / target / "autoscan"
                        self.assertEqual(binary.read_bytes(), (assets / f"autoscan_v1.4.4_linux_{arch}").read_bytes())
                        self.assertEqual(binary.stat().st_mode & 0o777, 0o755)

    def test_resolve_release_uses_a_published_tag_and_peeled_commit(self):
        release = {"tagName": "v1.4.4", "isDraft": False, "isPrerelease": False}
        refs = f"{'c' * 40}\trefs/tags/v1.4.4\n{REVISION}\trefs/tags/v1.4.4^{{}}\n"
        with patch.object(image, "_run", side_effect=[json.dumps(release), refs]):
            self.assertEqual(image.resolve_release(), ("v1.4.4", REVISION))
        for field in ("isDraft", "isPrerelease"):
            with self.subTest(field=field), patch.object(image, "_run", return_value=json.dumps({**release, field: True})):
                with self.assertRaisesRegex(image.ImageError, "published stable"):
                    image.resolve_release()


class RefreshTests(unittest.TestCase):
    def test_only_releases_and_base_refreshes_publish_latest(self):
        cases = (
            ("master", "refs/heads/master", "", ["master"]),
            ("master", "refs/heads/latest", "", []),
            ("master", "refs/pull/12/merge", "", []),
            ("release", "refs/tags/v1.4.4", "v1.4.4", ["1.4.4", "latest"]),
            ("release", "refs/heads/master", "v1.4.4", ["latest"]),
        )
        for recipe, source_ref, tag, expected in cases:
            with self.subTest(recipe=recipe, ref=source_ref):
                self.assertEqual(image.publication_tags(recipe, source_ref, tag), expected)
        for recipe, source_ref, tag in (
            ("master", "refs/tags/v1.4.4", ""),
            ("master", "refs/heads/master", "v1.4.4"),
            ("release", "refs/heads/feature", "v1.4.4"),
            ("release", "refs/tags/v1.4.5", "v1.4.4"),
            ("release", "refs/heads/master", ""),
        ):
            with self.subTest(recipe=recipe, ref=source_ref), self.assertRaises(image.ImageError):
                image.publication_tags(recipe, source_ref, tag)

    def test_status_retries_latest_independently_and_ignores_unrelated_master_commits(self):
        binary_sha = "c" * 40
        master = image_metadata()
        latest = image_metadata(revision="d" * 40)
        for platform in PLATFORMS:
            labels = latest["image"][platform]["config"]["Labels"]
            labels["org.opencontainers.image.version"] = "v1.4.4"
            labels["io.autoscan.binary.revision"] = binary_sha
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            write_recipes(root)
            def inspect(reference):
                return master if reference.endswith(":master") else latest
            with patch.object(image, "_run", return_value=REVISION), patch.object(image, "resolve_release", return_value=("v1.4.4", binary_sha)):
                status = image.refresh_status(root, inspect)
                self.assertEqual((status["master-rebuild"], status["release-rebuild"]), ("false", "false"))
                latest["image"]["linux/arm64"]["config"]["Labels"]["org.opencontainers.image.base.name"] = "old base"
                status = image.refresh_status(root, inspect)
                self.assertEqual((status["master-rebuild"], status["release-rebuild"]), ("false", "true"))
                latest.clear()
                self.assertEqual(image.refresh_status(root, inspect)["release-rebuild"], "true")

    def test_promotion_rejects_moved_source_or_newer_release(self):
        binary_sha = "c" * 40
        with patch.object(image, "remote_revision", return_value="d" * 40):
            with self.assertRaisesRegex(image.ImageError, "Source ref moved"):
                image.check_promotion("master", "refs/heads/master", REVISION, "", REVISION)
        with patch.object(image, "remote_revision", return_value=REVISION):
            with patch.object(image, "resolve_release", return_value=("v1.4.4", binary_sha)):
                image.check_promotion("release", "refs/heads/master", REVISION, "v1.4.4", binary_sha)
            with patch.object(image, "resolve_release", return_value=("v1.4.5", "e" * 40)):
                with self.assertRaisesRegex(image.ImageError, "Latest release changed"):
                    image.check_promotion("release", "refs/heads/master", REVISION, "v1.4.4", binary_sha)


if __name__ == "__main__":
    unittest.main()
