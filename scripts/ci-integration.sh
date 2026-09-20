#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"
. ./deploy/compatibility.env

# Heroic Labs publishes the same images to both registries. Keep the tag and
# content digest intact so a registry outage cannot select another runtime.
dockerhub_image() {
  case "$1" in
    registry.heroiclabs.com/heroiclabs/*@sha256:*) ;;
    *) printf 'Expected a digest-pinned official Heroic Labs image.\n' >&2; return 1 ;;
  esac
  image_digest=${1##*@sha256:}
  if [ "${#image_digest}" -ne 64 ]; then
    printf 'Expected a 64-character SHA-256 image digest.\n' >&2
    return 1
  fi
  case "$image_digest" in
    *[!0-9a-f]*) printf 'Expected a lowercase hexadecimal image digest.\n' >&2; return 1 ;;
  esac
  printf 'docker.io/%s\n' "${1#registry.heroiclabs.com/}"
}

CI_NAKAMA_IMAGE=$(dockerhub_image "$NAKAMA_IMAGE")
CI_PLUGIN_BUILDER_IMAGE=$(dockerhub_image "$PLUGIN_BUILDER_IMAGE")
FLEET_COMPOSE_OVERRIDE=deploy/compose.ci.yml
export CI_NAKAMA_IMAGE CI_PLUGIN_BUILDER_IMAGE FLEET_COMPOSE_OVERRIDE
exec make integration
