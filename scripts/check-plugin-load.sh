#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"
set -- -f deploy/compose.yml
if [ -n "${FLEET_COMPOSE_OVERRIDE:-}" ]; then
  set -- "$@" -f "$FLEET_COMPOSE_OVERRIDE"
fi
docker compose "$@" up -d --build

response_file=$(mktemp)
trap 'rm -f "$response_file"' EXIT HUP INT TERM

# Probe the APIs directly: some Compose releases treat the tools image's
# HEALTHCHECK NONE as a missing health check and fail `up --wait`.
probe_http() {
  response_code=$(curl --silent --show-error \
    --max-time 10 --retry 60 --retry-delay 2 \
    --retry-all-errors --retry-max-time 180 \
    --output "$response_file" --write-out '%{http_code}' "$@") || return "$?"
  # Without --fail, curl retries transient transport/HTTP failures but leaves
  # permanent HTTP errors for this check, so a 401 does not consume the timeout.
  case "$response_code" in
    2??) ;;
    *) printf 'API readiness probe returned HTTP %s.\n' "$response_code" >&2; return 1 ;;
  esac
}
probe_http http://127.0.0.1:18090/healthz

# A health check alone does not prove that the plugin's InitModule succeeded.
# The core exposes this protected endpoint only after registration is complete.
probe_http \
  -H 'Authorization: Bearer local-admin-token-for-development-only' \
  http://127.0.0.1:17350/fleet/v1/admin/status
if ! node -e 'const value=JSON.parse(require("node:fs").readFileSync(process.argv[1],"utf8")); if (!value || typeof value !== "object" || Array.isArray(value)) process.exit(1)' "$response_file"; then
  printf 'Nakama loaded, but the FleetManager status response was invalid.\n' >&2
  exit 1
fi
printf 'Official Nakama runtime is healthy and the FleetManager endpoint responds.\n'
