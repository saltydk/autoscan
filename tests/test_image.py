import json
from pathlib import Path
import tempfile
import unittest

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
            (root / "docker").mkdir()
            dockerfile = root / "docker" / "Dockerfile"
            original = f'ARG BASE_IMAGE="{BASE_PIN}"\n\nFROM ${{BASE_IMAGE}}\n'
            dockerfile.write_text(original)

            def inspect(reference):
                if reference.endswith(":latest"):
                    return image_metadata(revision=new_revision, digest=new_digest)
                return {"manifest": {"digest": "sha256:" + "e" * 64}}

            with self.assertRaisesRegex(image.ImageError, "SHA tag"):
                image.update_base(root, inspect, write=True)
            self.assertEqual(dockerfile.read_text(), original)

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

    def test_update_outputs_report_before_after_and_change_state(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "github-output"
            summary = Path(directory) / "summary.md"
            result = image.BaseUpdate(before=BASE_PIN, after=BASE_PIN, changed=False)

            image._write_update_outputs(result, output, summary)

            self.assertEqual(
                output.read_text(), f"changed=false\nbase-image={BASE_PIN}\n"
            )
            self.assertEqual(
                summary.read_text(),
                "## Base image update\n\n"
                f"- Status: unchanged\n- Before: `{BASE_PIN}`\n- After: `{BASE_PIN}`\n",
            )
            self.assertEqual(image.format_base_update(result), f"base image unchanged: {BASE_PIN}")

            changed = image.BaseUpdate(
                before=BASE_PIN,
                after=BASE_PIN.replace("b" * 64, "c" * 64),
                changed=True,
            )
            self.assertEqual(
                image.format_base_update(changed),
                f"base image changed: {changed.before} -> {changed.after}",
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


if __name__ == "__main__":
    unittest.main()
