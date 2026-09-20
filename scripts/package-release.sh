#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"
requested_version=${PLUGIN_VERSION:-}
# shellcheck source=../deploy/compatibility.env
. ./deploy/compatibility.env
plugin_version=${requested_version:-$PLUGIN_VERSION}
case "$plugin_version" in
  *[!a-zA-Z0-9._-]*|'') printf 'Invalid PLUGIN_VERSION.\n' >&2; exit 1 ;;
esac

artifact_name="nakama-playflow-$plugin_version-nakama-$NAKAMA_VERSION-linux-amd64"
artifact_dir="$repo_dir/dist/$artifact_name"
mkdir -p "$artifact_dir"

docker buildx build \
  --platform "$TARGET_PLATFORM" \
  --file deploy/Dockerfile \
  --target artifact \
  --build-arg "PLUGIN_VERSION=$plugin_version" \
  --metadata-file "$artifact_dir/build-metadata.json" \
  --output "type=local,dest=$artifact_dir" .

source_revision=$(git rev-parse HEAD 2>/dev/null || printf uncommitted)
source_dirty=true
if git rev-parse --is-inside-work-tree >/dev/null 2>&1 && [ -z "$(git status --porcelain)" ]; then
  source_dirty=false
fi
cat > "$artifact_dir/compatibility.json" <<EOF
{
  "plugin_version": "$plugin_version",
  "nakama_version": "$NAKAMA_VERSION",
  "nakama_common_version": "$NAKAMA_COMMON_VERSION",
  "go_version": "$GO_VERSION",
  "platform": "$TARGET_PLATFORM",
  "source_revision": "$source_revision",
  "source_dirty": $source_dirty,
  "builder_image": "$PLUGIN_BUILDER_IMAGE",
  "runtime_image": "$NAKAMA_IMAGE"
}
EOF

cd "$artifact_dir"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum playflow.so compatibility.json build-metadata.json > SHA256SUMS
else
  shasum -a 256 playflow.so compatibility.json build-metadata.json > SHA256SUMS
fi
cd "$repo_dir/dist"
tar -czf "$artifact_name.tar.gz" \
  "$artifact_name/playflow.so" \
  "$artifact_name/compatibility.json" \
  "$artifact_name/build-metadata.json" \
  "$artifact_name/SHA256SUMS"
printf 'Created %s/dist/%s.tar.gz\n' "$repo_dir" "$artifact_name"
