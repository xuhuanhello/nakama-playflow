#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"
# shellcheck source=../deploy/compatibility.env
. ./deploy/compatibility.env

check_module() {
  module_path=$1
  expected_version=$2
  actual_version=$(go list -mod=readonly -m -f '{{.Version}}' "$module_path")
  if [ "$actual_version" != "$expected_version" ]; then
    printf 'Compatibility error: %s requires %s, selected %s\n' "$module_path" "$expected_version" "$actual_version" >&2
    exit 1
  fi
}

actual_go=$(go env GOVERSION)
if [ "$actual_go" != "go$GO_VERSION" ]; then
  printf 'Compatibility error: Nakama %s requires Go %s, running %s. Build the plugin with deploy/Dockerfile.\n' "$NAKAMA_VERSION" "$GO_VERSION" "$actual_go" >&2
  exit 1
fi

check_module github.com/heroiclabs/nakama-common "$NAKAMA_COMMON_VERSION"
check_module github.com/jackc/pgx/v5 "$PGX_VERSION"
check_module google.golang.org/protobuf "$PROTOBUF_VERSION"
check_module github.com/jackc/pgpassfile v1.0.0
check_module github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761
check_module github.com/jackc/puddle/v2 v2.2.2
check_module golang.org/x/sync v0.23.0
check_module golang.org/x/text v0.42.0

if [ "${1:-}" = "--plugin" ]; then
  actual_platform="$(go env GOOS)/$(go env GOARCH)"
  if [ "$actual_platform" != "$TARGET_PLATFORM" ] || [ "$(go env CGO_ENABLED)" != 1 ]; then
    printf 'Compatibility error: plugin must build for %s with CGO_ENABLED=1; got %s.\n' "$TARGET_PLATFORM" "$actual_platform" >&2
    exit 1
  fi
fi

printf 'Compatible build inputs: Nakama %s, Go %s, common %s.\n' "$NAKAMA_VERSION" "$GO_VERSION" "$NAKAMA_COMMON_VERSION"
