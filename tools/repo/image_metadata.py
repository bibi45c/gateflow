"""Write deterministic image tags to GITHUB_OUTPUT without shell interpolation."""

import argparse
import os
from pathlib import Path
import re

SERVICES = ("gateway", "access", "metering", "mock-upstream", "console")


def image_tags(service: str, sha: str, ref: str) -> list[str]:
    if service not in SERVICES or not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("Expected a known service and full lowercase commit SHA")
    image = f"ghcr.io/bibi45c/gateflow-{service}"
    tags = [f"{image}:{sha}"]
    if ref.startswith("refs/tags/"):
        version = ref.removeprefix("refs/tags/")
        if not re.fullmatch(r"v[A-Za-z0-9_.-]{0,127}", version):
            raise ValueError("Release tag is not a valid v-prefixed Docker tag")
        tags.append(f"{image}:{version}")
    return tags


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--service", required=True, choices=SERVICES)
    parser.add_argument("--digest", help="Record a published image in the job summary")
    args = parser.parse_args()
    tags = image_tags(args.service, os.environ["GITHUB_SHA"], os.environ["GITHUB_REF"])
    if args.digest is not None:
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", args.digest):
            raise ValueError("Expected a sha256 image digest")
        text = (f"### {args.service}\n\nCommit: `{os.environ['GITHUB_SHA']}`\n\n"
                f"Image: `ghcr.io/bibi45c/gateflow-{args.service}@{args.digest}`\n")
        with Path(os.environ["GITHUB_STEP_SUMMARY"]).open("a", encoding="utf-8", newline="\n") as summary:
            summary.write(text)
        return
    with Path(os.environ["GITHUB_OUTPUT"]).open("a", encoding="utf-8", newline="\n") as output:
        output.write("tags<<IMAGE_TAGS\n" + "\n".join(tags) + "\nIMAGE_TAGS\n")


if __name__ == "__main__":
    main()
