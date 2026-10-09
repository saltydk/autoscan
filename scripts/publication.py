"""Promote verified images and record publication and acceptance outcomes."""

from __future__ import annotations

import argparse
import json
import sys
from collections.abc import Sequence
from pathlib import Path

from scripts import image


def promote(args: argparse.Namespace) -> bool:
    if image.DIGEST_PATTERN.fullmatch(args.image_digest) is None:
        raise image.ImageError("invalid publication digest")
    tags = json.loads(args.tags)
    if tags != image.publication_tags(
        args.recipe, args.source_ref, args.release_tag, variant=args.variant
    ):
        raise image.ImageError("publication tags do not match the source policy")
    reference = f"{image.IMAGE_REPOSITORY}@{args.image_digest}"
    image.verify_publication(
        Path.cwd(),
        reference,
        args.source_sha,
        recipe=args.recipe,
        binary_sha=args.binary_sha,
        version=args.version,
        variant=args.variant,
    )
    if not image.check_promotion(
        args.recipe,
        args.source_ref,
        args.source_sha,
        args.release_tag,
        args.binary_sha,
        variant=args.variant,
    ):
        return False
    for tag in tags:
        alias = f"{image.IMAGE_REPOSITORY}:{tag}"
        record = f"{alias}@{args.image_digest}\n"
        with Path("publication-attempts.txt").open("a", encoding="utf-8") as stream:
            stream.write(record)
        image._run(
            ["docker", "buildx", "imagetools", "create", "--tag", alias, reference]
        )
        manifest = json.loads(
            image._run(
                [
                    "docker",
                    "buildx",
                    "imagetools",
                    "inspect",
                    alias,
                    "--format",
                    "{{json .Manifest}}",
                ]
            )
        )
        if manifest.get("digest") != args.image_digest:
            raise image.ImageError(
                f"{alias} did not resolve to the verified digest after promotion"
            )
        with Path("published.txt").open("a", encoding="utf-8") as stream:
            stream.write(record)
    return True


def candidate(args: argparse.Namespace) -> None:
    report = {
        key: getattr(args, key) or "unavailable"
        for key in (
            "platform",
            "source_sha",
            "binary_sha",
            "variant",
            "runtime",
            "security_report",
            "kev",
        )
    }
    args.report.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    with args.summary.open("a", encoding="utf-8") as stream:
        stream.write(f"### {report['platform']}\n\n")
        for key, value in report.items():
            stream.write(f"- {key}: `{value}`\n")


def summarize(args: argparse.Namespace) -> bool:
    tags = json.loads(args.tags)
    if not isinstance(tags, list) or not all(isinstance(tag, str) for tag in tags):
        raise image.ImageError("acceptance tags must be an array of strings")
    publication = args.publish
    if publication == "success":
        publication = "published" if args.published == "true" else "skipped"
    with args.summary.open("a", encoding="utf-8") as stream:
        stream.write(
            "| Stage | Result |\n| --- | --- |\n"
            f"| Inputs | {args.prepare} |\n| Image acceptance | {args.tests} |\n"
            f"| Publication | {publication} |\n"
        )
    return (
        args.prepare == "success"
        and args.tests == "success"
        and (not tags or args.publish == "success")
    )


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    promotion = commands.add_parser("promote")
    promotion.add_argument("--recipe", choices=image.RECIPES, required=True)
    promotion.add_argument("--variant", choices=image.VARIANTS, required=True)
    for name in (
        "source-sha",
        "source-ref",
        "binary-sha",
        "image-digest",
        "version",
        "tags",
    ):
        promotion.add_argument("--" + name, required=True)
    promotion.add_argument("--release-tag", default="")
    promotion.add_argument("--github-output", type=Path, required=True)
    report = commands.add_parser("candidate")
    for name in (
        "platform",
        "source-sha",
        "binary-sha",
        "variant",
        "runtime",
        "security-report",
        "kev",
    ):
        report.add_argument("--" + name, required=True)
    report.add_argument("--report", type=Path, default=Path("candidate.json"))
    report.add_argument("--summary", type=Path, required=True)
    acceptance = commands.add_parser("summarize")
    for name in ("prepare", "tests", "publish"):
        acceptance.add_argument(
            "--" + name,
            choices=("success", "failure", "cancelled", "skipped"),
            required=True,
        )
    acceptance.add_argument("--published", choices=("", "true", "false"), default="")
    acceptance.add_argument("--tags", required=True)
    acceptance.add_argument("--summary", type=Path, required=True)
    build = commands.add_parser("summarize-build")
    build.add_argument("--build", required=True)
    build.add_argument("--images", required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "promote":
            published = str(promote(args)).lower()
            with args.github_output.open("a", encoding="utf-8") as stream:
                stream.write(f"published={published}\n")
            print(json.dumps({"published": published == "true"}))
        elif args.command == "candidate":
            candidate(args)
        elif args.command == "summarize-build":
            if args.build != "success" or args.images != "success":
                raise image.ImageError(
                    "binary and image acceptance stages did not succeed"
                )
        elif not summarize(args):
            raise image.ImageError("image acceptance stages did not succeed")
    except (image.ImageError, OSError, ValueError) as error:
        print(f"Image publication: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
