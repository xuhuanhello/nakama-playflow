import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock
import urllib.error


SPEC = importlib.util.spec_from_file_location("release", Path(__file__).with_name("release.py"))
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def test_semver_and_registry_identity(self):
        self.assertEqual(release.version_from_tag("v0.1.0-rc.1"), "0.1.0-rc.1")
        self.assertEqual(release.version_from_tag("v1.0.0"), "1.0.0")
        for tag in ("main", "v01.0.0", "v1.0.0-rc.01", "v1.0.0+other", "v1.0.0;id", "v1.0.0\n"):
            with self.subTest(tag=tag), self.assertRaises(release.ReleaseError):
                release.version_from_tag(tag)
        for image in ("docker.io/owner/image", "ghcr.io/owner/image:latest", "ghcr.io/../image", "ghcr.io/owner/image\n"):
            with self.subTest(image=image), self.assertRaises(release.ReleaseError):
                release.image_repository(image)

    def test_registry_mirror_preserves_exact_digest(self):
        digest = "sha256:" + "a" * 64
        original = "registry.heroiclabs.com/heroiclabs/nakama:3.41.0@" + digest
        self.assertEqual(release.dockerhub_copy(original), "docker.io/heroiclabs/nakama:3.41.0@" + digest)
        for image in (original.split("@")[0], original[:-1], original.replace("heroiclabs.com", "other.example")):
            with self.assertRaises(release.ReleaseError):
                release.dockerhub_copy(image)

    def test_packaging_rejects_wrong_architecture(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "binary"
            header = bytearray(20)
            header[:6] = b"\x7fELF\x02\x01"
            header[18:20] = b"\x3e\x00"
            path.write_bytes(header)
            release.check_elf(path)
            header[18:20] = b"\xb7\x00"  # ARM64 cannot be released as amd64.
            path.write_bytes(header)
            with self.assertRaises(release.ReleaseError):
                release.check_elf(path)

    def test_archive_is_reproducible_and_contains_only_release_files(self):
        with tempfile.TemporaryDirectory() as directory:
            bundle = Path(directory) / "bundle"
            bundle.mkdir()
            for filename in release.BUNDLE_FILES:
                (bundle / filename).write_bytes((filename + "\n").encode())
            (bundle / ".env").write_text("PRIVATE_FIXTURE=not-for-release\n")
            (bundle / "prepared.json").write_text("not a public release asset\n")
            first = release.archive(bundle, "candidate").read_bytes()
            for filename in release.BUNDLE_FILES:
                (bundle / filename).touch()
            self.assertEqual(first, release.archive(bundle, "candidate").read_bytes())
            with tarfile.open(fileobj=io.BytesIO(first), mode="r:gz") as tar:
                self.assertEqual(set(tar.getnames()), {"candidate/" + f for f in release.BUNDLE_FILES + ("SHA256SUMS",)})
                sums = tar.extractfile("candidate/SHA256SUMS").read().decode().splitlines()
                for line in sums:
                    digest, filename = line.split("  ")
                    self.assertEqual(digest, hashlib.sha256(tar.extractfile("candidate/" + filename).read()).hexdigest())
                self.assertEqual(tar.getmember("candidate/fleet-migrate").mode, 0o755)
                self.assertEqual(tar.getmember("candidate/playflow.so").mode, 0o644)

    def test_public_manifest_checks_content_digest(self):
        body = b'{"schemaVersion":2}'
        digest = "sha256:" + hashlib.sha256(body).hexdigest()
        with mock.patch.object(release.urllib.request, "urlopen", side_effect=[
            io.BytesIO(b'{"token":"anonymous-fixture-token"}'), io.BytesIO(body)
        ]):
            release.anonymous_manifest("ghcr.io/owner/image@" + digest)
        with mock.patch.object(release.urllib.request, "urlopen", side_effect=[
            io.BytesIO(b'{"token":"anonymous-fixture-token"}'), io.BytesIO(body + b"wrong")
        ]), self.assertRaisesRegex(release.ReleaseError, "digest differs"):
            release.anonymous_manifest("ghcr.io/owner/image@" + digest)

    def test_private_package_failure_does_not_print_auth_material(self):
        failure = urllib.error.HTTPError("https://ghcr.io/token", 403, "fixture-sensitive-data", {}, None)
        with mock.patch.object(release.urllib.request, "urlopen", side_effect=failure):
            with self.assertRaises(release.ReleaseError) as raised:
                release.anonymous_manifest("ghcr.io/owner/image@sha256:" + "a" * 64)
        self.assertIn("HTTP 403", str(raised.exception))
        self.assertNotIn("fixture-sensitive-data", str(raised.exception))

    def test_changed_tested_image_cannot_be_published(self):
        with tempfile.TemporaryDirectory() as directory:
            bundle = Path(directory)
            prepared = {"tag": "v0.1.0-rc.1", "repository": "ghcr.io/owner/image", "source_revision": "source",
                        "images": {"runtime": {"id": "tested-image"}}, "binaries": {}}
            release.write_json(bundle / "prepared.json", prepared)
            context = ("0.1.0-rc.1", "ghcr.io/owner/image", {}, "bundle", bundle, {"runtime": "image"})
            with mock.patch.object(release, "context", return_value=context), \
                 mock.patch.object(release, "source_revision", return_value="source"), \
                 mock.patch.object(release, "image_info", return_value={"Id": "changed-image", "Config": {"Labels": {}}}), \
                 mock.patch.object(release, "run") as run:
                with self.assertRaisesRegex(release.ReleaseError, "tested image changed"):
                    release.publish(SimpleNamespace(tag="v0.1.0-rc.1"))
                run.assert_not_called()

    def test_changed_extracted_binary_cannot_be_published(self):
        with tempfile.TemporaryDirectory() as directory:
            bundle = Path(directory)
            (bundle / "playflow.so").write_bytes(b"changed")
            prepared = {"tag": "v0.1.0-rc.1", "repository": "ghcr.io/owner/image", "source_revision": "source",
                        "images": {}, "binaries": {"playflow.so": hashlib.sha256(b"tested").hexdigest()}}
            release.write_json(bundle / "prepared.json", prepared)
            context = ("0.1.0-rc.1", "ghcr.io/owner/image", {}, "bundle", bundle, {})
            with mock.patch.object(release, "context", return_value=context), \
                 mock.patch.object(release, "source_revision", return_value="source"), \
                 mock.patch.object(release, "run") as run:
                with self.assertRaisesRegex(release.ReleaseError, "prepared binary changed"):
                    release.publish(SimpleNamespace(tag="v0.1.0-rc.1"))
                run.assert_not_called()


if __name__ == "__main__":
    unittest.main()
