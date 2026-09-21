#!/usr/bin/env python3
"""Build, validate and publish a tagged release without game credentials."""

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import urllib.error
import urllib.parse
import urllib.request


ROOT = Path(__file__).resolve().parents[1]
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")
BUNDLE_FILES = (
    "playflow.so", "fleet-migrate", "compatibility.json", "images.json",
    "runtime-build-metadata.json", "tools-build-metadata.json",
)


class ReleaseError(Exception):
    pass


def run(args, capture=False, env=None):
    result = subprocess.run(args, cwd=ROOT, check=True, text=True,
                            encoding="utf-8", stdout=subprocess.PIPE if capture else None,
                            env=env)
    return result.stdout.strip() if capture else None


def version_from_tag(tag):
    if not re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?", tag):
        raise ReleaseError("Use a SemVer tag such as v0.1.0-rc.1, without build metadata.")
    if "-" in tag and any(x.isdigit() and len(x) > 1 and x[0] == "0"
                         for x in tag.split("-", 1)[1].split(".")):
        raise ReleaseError("Numeric prerelease identifiers cannot contain leading zeroes.")
    return tag[1:]


def image_repository(value):
    value = value.lower()
    if not re.fullmatch(r"ghcr\.io/[a-z0-9][a-z0-9_-]*/[a-z0-9][a-z0-9._-]*", value):
        raise ReleaseError("Expected ghcr.io/OWNER/REPOSITORY.")
    return value


def dockerhub_copy(image):
    match = re.fullmatch(r"registry\.heroiclabs\.com/(heroiclabs/[a-z-]+:[0-9.]+)@(sha256:[0-9a-f]{64})", image)
    if not match:
        raise ReleaseError("Official runtime and builder must be pinned by SHA-256 digest.")
    return "docker.io/" + match[1] + "@" + match[2]


def pins():
    result = {}
    for line in (ROOT / "deploy/compatibility.env").read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#"):
            key, value = line.split("=", 1)
            result[key] = value
    if result["TARGET_PLATFORM"] != "linux/amd64":
        raise ReleaseError("This release pipeline validates linux/amd64 only.")
    return result


def source_revision(tag):
    if run(["git", "status", "--porcelain"], capture=True):
        raise ReleaseError("Release builds require a clean tagged checkout.")
    revision = run(["git", "rev-parse", "HEAD"], capture=True)
    tagged = run(["git", "rev-parse", "refs/tags/" + tag + "^{commit}"], capture=True)
    if revision != tagged:
        raise ReleaseError("The checkout must exactly match the release tag.")
    run(["git", "merge-base", "--is-ancestor", revision, "origin/main"])
    return revision


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def digest_file(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def check_elf(path):
    header = path.read_bytes()[:20]
    if header[:6] != b"\x7fELF\x02\x01" or header[18:20] != b"\x3e\x00":
        raise ReleaseError("Release binaries must be little-endian Linux x86_64 ELF files.")


def image_info(reference):
    value = json.loads(run(["docker", "image", "inspect", reference], capture=True))[0]
    if value["Os"] != "linux" or value["Architecture"] != "amd64":
        raise ReleaseError("Unexpected release image platform.")
    return value


def context(args):
    version = version_from_tag(args.tag)
    repository = image_repository(args.repository)
    config = pins()
    name = f"nakama-playflow-{version}-nakama-{config['NAKAMA_VERSION']}-linux-amd64"
    tag = f"{version}-nakama-{config['NAKAMA_VERSION']}"
    if len(tag) > 128:
        raise ReleaseError("The compatibility-specific image tag exceeds 128 characters.")
    return version, repository, config, name, ROOT / "dist" / name, {
        "runtime": repository + ":" + tag,
        "tools": repository + "-tools:" + tag,
    }


def extract(reference, source, destination):
    container = run(["docker", "create", "--entrypoint", "/bin/true", reference], capture=True)
    try:
        run(["docker", "cp", container + ":" + source, str(destination)])
    finally:
        run(["docker", "rm", "--force", container])
    check_elf(destination)


def prepare(args):
    version, repository, config, name, bundle, images = context(args)
    revision = source_revision(args.tag)
    if bundle.exists():
        raise ReleaseError("Release output already exists; use a fresh checkout or inspect it first.")
    bundle.mkdir(parents=True)
    runtime = dockerhub_copy(config["NAKAMA_IMAGE"])
    builder = dockerhub_copy(config["PLUGIN_BUILDER_IMAGE"])
    run(["make", "test", "test-race"])
    for target, reference in images.items():
        run(["docker", "buildx", "build", "--platform", config["TARGET_PLATFORM"],
             "--file", "deploy/Dockerfile", "--target", target, "--tag", reference,
             "--load", "--build-arg", "PLUGIN_VERSION=" + version,
             "--build-arg", "VCS_REF=" + revision, "--build-arg", "NAKAMA_IMAGE=" + runtime,
             "--build-arg", "PLUGIN_BUILDER_IMAGE=" + builder,
             "--metadata-file", str(bundle / (target + "-build-metadata.json")), "."])
    override = bundle / "compose.release.json"
    write_json(override, {"services": {
        "nakama-migrate": {"image": runtime},
        "fleet-migrate": {"image": images["tools"]},
        "playflow-mock": {"image": images["tools"]},
        "nakama": {"image": images["runtime"]},
    }})
    env = dict(os.environ, COMPOSE_PROJECT_NAME="pf-release-" + str(os.getpid()),
               FLEET_COMPOSE_OVERRIDE=str(override), FLEET_COMPOSE_SKIP_BUILD="true")
    try:
        run(["make", "integration"], env=env)
    finally:
        run(["docker", "compose", "-f", "deploy/compose.yml", "-f", str(override),
             "down", "--volumes"], env=env)
    extract(images["runtime"], "/nakama/data/modules/playflow.so", bundle / "playflow.so")
    extract(images["tools"], "/usr/local/bin/fleet-migrate", bundle / "fleet-migrate")
    compatibility = {
        "plugin_version": version, "nakama_version": config["NAKAMA_VERSION"],
        "go_version": config["GO_VERSION"], "nakama_common_version": config["NAKAMA_COMMON_VERSION"],
        "pgx_version": config["PGX_VERSION"], "protobuf_version": config["PROTOBUF_VERSION"],
        "platform": config["TARGET_PLATFORM"], "source_revision": revision, "source_dirty": False,
        "runtime_image": config["NAKAMA_IMAGE"], "builder_image": config["PLUGIN_BUILDER_IMAGE"],
        "build_runtime_image": runtime, "build_builder_image": builder, "fleet_protocol_version": 1,
    }
    write_json(bundle / "compatibility.json", compatibility)
    write_json(bundle / "prepared.json", {
        "tag": args.tag, "repository": repository, "source_revision": revision,
        "images": {role: {"tag": reference, "id": image_info(reference)["Id"]}
                   for role, reference in images.items()},
        "binaries": {filename: digest_file(bundle / filename) for filename in ("playflow.so", "fleet-migrate")},
    })
    print("Prepared and integration-tested " + name)


def archive(bundle, name):
    checksums = "".join(digest_file(bundle / f) + "  " + f + "\n" for f in sorted(BUNDLE_FILES))
    (bundle / "SHA256SUMS").write_text(checksums, encoding="utf-8")
    destination = bundle.parent / (name + ".tar.gz")
    with destination.open("wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as zipped:
        with tarfile.open(fileobj=zipped, mode="w") as tar:
            for filename in sorted(BUNDLE_FILES + ("SHA256SUMS",)):
                data = (bundle / filename).read_bytes()
                info = tarfile.TarInfo(name + "/" + filename)
                info.size = len(data)
                info.mode = 0o755 if filename == "fleet-migrate" else 0o644
                tar.addfile(info, io.BytesIO(data))
    return destination


def anonymous_manifest(reference):
    repository, separator, digest = reference.partition("@")
    image_repository(repository)
    if not separator or not DIGEST.fullmatch(digest):
        raise ReleaseError("Public verification requires an immutable GHCR digest reference.")
    name = repository.removeprefix("ghcr.io/")
    query = urllib.parse.urlencode({"service": "ghcr.io", "scope": "repository:" + name + ":pull"})
    try:
        with urllib.request.urlopen("https://ghcr.io/token?" + query, timeout=30) as response:
            token = json.load(response)["token"]
        request = urllib.request.Request("https://ghcr.io/v2/" + name + "/manifests/" + digest, headers={
            "Authorization": "Bearer " + token,
            "Accept": "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json",
        })
        with urllib.request.urlopen(request, timeout=30) as response:
            actual = "sha256:" + hashlib.sha256(response.read()).hexdigest()
        if actual != digest:
            raise ReleaseError("Public manifest digest differs from the published release.")
    except (urllib.error.URLError, KeyError, ValueError) as exc:
        status = getattr(exc, "code", "unavailable")
        raise ReleaseError(f"Anonymous pull failed for {repository} (HTTP {status}); check package visibility and network.") from None


def verify_public(manifest):
    data = json.loads(manifest.read_text(encoding="utf-8"))
    if data.get("platform") != "linux/amd64":
        raise ReleaseError("Unexpected manifest platform.")
    for role in ("runtime", "tools"):
        anonymous_manifest(data[role]["digest"])
    print("Both image manifests are publicly accessible with verified immutable digests.")


def publish(args):
    version, repository, config, name, bundle, images = context(args)
    prepared = json.loads((bundle / "prepared.json").read_text(encoding="utf-8"))
    revision = source_revision(args.tag)
    if prepared["tag"] != args.tag or prepared["repository"] != repository or prepared["source_revision"] != revision:
        raise ReleaseError("Prepared source identity does not match this release.")
    for role, reference in images.items():
        info = image_info(reference)
        labels = info["Config"]["Labels"]
        if info["Id"] != prepared["images"][role]["id"] or labels.get("org.opencontainers.image.revision") != revision:
            raise ReleaseError("A tested image changed; refusing to publish.")
    for filename, digest in prepared["binaries"].items():
        if filename not in ("playflow.so", "fleet-migrate") or digest_file(bundle / filename) != digest:
            raise ReleaseError("A prepared binary changed; refusing to publish.")
    gh_repo = repository.removeprefix("ghcr.io/")
    # Claim a new release before any push. Existing drafts/releases are never overwritten.
    run(["gh", "release", "create", args.tag, "--repo", gh_repo, "--verify-tag", "--target", revision,
         "--title", f"{args.tag} · Nakama {config['NAKAMA_VERSION']} · linux/amd64", "--draft",
         "--notes", "Images are being published and checked for anonymous access. Do not deploy this draft."])
    manifest = {"schema_version": 1, "source_revision": revision, "platform": "linux/amd64"}
    for role, reference in images.items():
        run(["docker", "push", reference])
        prefix = reference.rsplit(":", 1)[0] + "@"
        digests = [x for x in image_info(reference).get("RepoDigests", []) if x.startswith(prefix) and DIGEST.fullmatch(x[len(prefix):])]
        if len(digests) != 1:
            raise ReleaseError("The pushed image did not resolve to one immutable registry digest.")
        manifest[role] = {"tag": reference, "digest": digests[0]}
    write_json(bundle / "images.json", manifest)
    package = archive(bundle, name)
    assets = [package, bundle / "images.json", bundle / "compatibility.json"]
    sums = bundle.parent / "SHA256SUMS"
    sums.write_text("".join(digest_file(p) + "  " + p.name + "\n" for p in assets), encoding="utf-8")
    assets.append(sums)
    notes = bundle / "release-notes.md"
    notes.write_text(
        f"Nakama {config['NAKAMA_VERSION']} / Linux amd64, Fleet protocol v1. Source: `{revision}`.\n\n"
        "This candidate passed unit, race, PostgreSQL and actual Nakama runtime-load/integration checks. "
        "Game-specific cloud acceptance is separate; this is not a production capacity certification.\n\n"
        + "\n".join(f"- {role}: `{manifest[role]['digest']}`" for role in ("runtime", "tools"))
        + "\n\nRun the matching tools image for the fleet database migration before starting Nakama. "
        "Keep real credentials in server-side secret configuration.\n\n"
        f"[Deployment and rollback instructions](https://github.com/{gh_repo}/blob/{args.tag}/docs/releases.md).\n",
        encoding="utf-8")
    run(["gh", "release", "upload", args.tag, "--repo", gh_repo] + [str(p) for p in assets])
    run(["gh", "release", "edit", args.tag, "--repo", gh_repo, "--notes-file", str(notes)])
    try:
        verify_public(bundle / "images.json")
    except ReleaseError:
        print("The release remains a draft. Make BOTH GHCR packages public, verify images.json, then publish the existing draft; see docs/releases.md.", file=sys.stderr)
        raise
    run(["gh", "release", "edit", args.tag, "--repo", gh_repo, "--draft=false",
         "--prerelease=" + ("true" if "-" in version else "false")])
    print("Published " + args.tag + " without creating a latest tag.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for command in ("prepare", "publish"):
        sub = commands.add_parser(command)
        sub.add_argument("--tag", required=True)
        sub.add_argument("--repository", default="ghcr.io/xuhuanhello/nakama-playflow")
    sub = commands.add_parser("verify-public")
    sub.add_argument("--manifest", required=True, type=Path)
    args = parser.parse_args()
    try:
        if args.command == "verify-public":
            verify_public(args.manifest)
        else:
            {"prepare": prepare, "publish": publish}[args.command](args)
    except (ReleaseError, subprocess.CalledProcessError) as exc:
        print("Release failed: " + str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
