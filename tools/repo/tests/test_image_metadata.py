import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class ImageMetadataTests(unittest.TestCase):
    def setUp(self):
        path = Path(__file__).resolve().parents[1] / "image_metadata.py"
        spec = importlib.util.spec_from_file_location("image_metadata", path)
        self.module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.module)

    def test_main_and_pull_request_use_full_sha(self):
        sha = "a" * 40
        image = "ghcr.io/bibi45c/gateflow-gateway"
        for ref in ("refs/heads/main", "refs/pull/21/merge"):
            self.assertEqual(self.module.image_tags("gateway", sha, ref), [f"{image}:{sha}"])

    def test_release_adds_exact_version_tag(self):
        tags = self.module.image_tags("mock-upstream", "b" * 40, "refs/tags/v0.1.0")
        self.assertEqual(tags, ["ghcr.io/bibi45c/gateflow-mock-upstream:" + "b" * 40,
                                "ghcr.io/bibi45c/gateflow-mock-upstream:v0.1.0"])

    def test_invalid_inputs_fail_before_build(self):
        for service, sha, ref in [("unknown", "a" * 40, "refs/heads/main"),
                                  ("access", "short", "refs/heads/main"),
                                  ("access", "a" * 40, "refs/tags/vbad/tag")]:
            with self.subTest(service=service, sha=sha, ref=ref):
                with self.assertRaises(ValueError):
                    self.module.image_tags(service, sha, ref)

    def test_cli_writes_tags_and_digest_summary(self):
        script = Path(__file__).resolve().parents[1] / "image_metadata.py"
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "output"
            summary = Path(directory) / "summary"
            env = dict(os.environ, GITHUB_SHA="a" * 40, GITHUB_REF="refs/tags/v0.1.0",
                       GITHUB_OUTPUT=str(output), GITHUB_STEP_SUMMARY=str(summary))
            command = [sys.executable, str(script), "--service", "metering"]
            subprocess.run(command, env=env, check=True, capture_output=True)
            self.assertEqual(output.read_text(encoding="utf-8"),
                             "tags<<IMAGE_TAGS\nghcr.io/bibi45c/gateflow-metering:" + "a" * 40
                             + "\nghcr.io/bibi45c/gateflow-metering:v0.1.0\nIMAGE_TAGS\n")
            subprocess.run(command + ["--digest", "sha256:" + "b" * 64],
                           env=env, check=True, capture_output=True)
            self.assertIn("gateflow-metering@sha256:" + "b" * 64,
                          summary.read_text(encoding="utf-8"))
            result = subprocess.run(command + ["--digest", "bad\nvalue"],
                                    env=env, capture_output=True)
            self.assertNotEqual(result.returncode, 0)
