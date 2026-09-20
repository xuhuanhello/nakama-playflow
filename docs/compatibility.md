# Compatibility, builds, and release management

## The compatibility boundary

The distributable `playflow.so` is a Go plugin loaded into the official Nakama
process. Go plugins require the host and plugin to use the same Go toolchain and
compatible build settings, as well as identical versions of packages they share.
This is a property of Go's plugin mechanism, not an extra restriction imposed by
PlayFlow. See the [Go plugin documentation](https://pkg.go.dev/plugin) and
[Nakama dependency requirements](https://heroiclabs.com/docs/nakama/server-framework/go-runtime/go-dependencies/).

This repository currently targets one combination:

| Component | Pinned value |
| --- | --- |
| Official Nakama runtime image | `registry.heroiclabs.com/heroiclabs/nakama:3.41.0` |
| Official builder image | `registry.heroiclabs.com/heroiclabs/nakama-pluginbuilder:3.41.0` |
| Go toolchain | `1.27.1` |
| `nakama-common` | `v1.48.0` |
| PostgreSQL Go driver (`pgx/v5`) | `v5.11.0` |
| Protobuf | `v1.36.12` |
| Plugin runtime architecture | `linux/amd64` |
| Local database image | `postgres:16-alpine` |

The pins are taken from [Nakama 3.41.0's go.mod](https://github.com/heroiclabs/nakama/blob/v3.41.0/go.mod).
Additional shared transitive dependencies are pinned in `go.mod` and checked by
`scripts/check-compatibility.sh`. Versions in `deploy/compatibility.env` describe
a single target, not a supported version range. A release is valid only when the
actual runtime-load and integration tests pass for that target.

The official images were pulled for Linux/amd64 on 2026-09-21. The builder
reported `go version go1.27.1 linux/amd64`. Their resolved digests are pinned in
the Dockerfile and `deploy/compatibility.env`:

```text
nakama:3.41.0
sha256:ca9f3fe65c90f56860fe5d7cd024868a395cb1a001abb9912bf87ef47d605349
nakama-pluginbuilder:3.41.0
sha256:eb506121ec2e67f39febee3a4252ae05598a0b39d3517b713ec1577714a9e5b5
```

Installing the source library in another Go runtime module is supported, but its
complete dependency graph must remain compatible with Nakama. `go get -u` or
adding an unrelated plugin can select a newer shared dependency and break loading.
Do not resolve that by editing only the `.so` filename or image tag.

The Unity package communicates over HTTP and the game's transport; it does not
share Nakama's Go process. It therefore tracks the fleet wire protocol and Unity
package version. It does not need rebuilding merely because the matching Go
plugin was rebuilt for a new Nakama patch. Any protocol change still needs its own
compatibility review.

`nakama-common v1.48.0` retains the existing `FleetManager` interface and adds
named manager registration/retrieval (`RegisterFleetManagerByName` and
`GetFleetManagerByName`). This plugin registers the default manager. Applications
with other fleet providers must explicitly compose registration and routing; do
not load two standalone plugins that both assume ownership of the default
manager and the game's matchmaker hook.

For an existing Go runtime module, import
`github.com/xuhuanhello/nakama-playflow/pkg/fleetmanager` and call the public
`fleetmanager.RegisterFromEnv(ctx, logger, db, nk, initializer)` from its
`InitModule`. The helper opens the separate fleet database and provider client,
registers the fleet and included two-player bridge, and arranges shutdown cleanup.
Run the separate fleet migration first. `NewFromEnv` is available when application
code needs the manager before hook registration. Neither path requires importing
this repository's `internal` packages. Keep the consuming module's dependency pins
aligned with the table above and compile the combined runtime module with the
matching official builder.

## Reproducible build workflow

Use Docker to build Linux plugins, including on an Apple Silicon Mac:

Local prerequisites are Docker with Compose/Buildx, Go 1.27.1 or newer for source
tests, Node.js 22 or newer for the dependency-free integration runner, `curl`, and
`make`. The exact pinned toolchain is used inside Docker for release binaries.

```sh
make test
make test-race
make build
make integration
PLUGIN_VERSION=0.1.0 make package
```

`make test` uses the host Go toolchain for logic tests. Plugin builds use the
official builder, disable automatic toolchain upgrades with `GOTOOLCHAIN=local`,
assert the exact version, and build with `--trimpath --mod=readonly` and
`CGO_ENABLED=1`. A host build on macOS is not a Linux runtime artifact.

`make integration` starts the isolated Compose stack, waits for Nakama to become
healthy, checks a plugin-owned authenticated status endpoint, and executes the
PostgreSQL integration tests plus the smoke runner. The database tests verify
cross-connection locking, admission uniqueness, rollback, namespace isolation,
and persistence; they create/delete only their own random test namespaces.
Unit compilation alone cannot detect a host/plugin package hash
mismatch. CI therefore performs the real runtime load in addition to race tests.

`make package` writes an archive such as:

```text
dist/nakama-playflow-0.1.0-nakama-3.41.0-linux-amd64.tar.gz
```

The archive contains `playflow.so`, version/source compatibility metadata,
BuildKit metadata, and SHA-256 checksums. Official runtime and builder references
include both the release tag and the verified immutable digest. Package metadata
records those exact references so the source build can be traced to its runtime.
Metadata also records whether the source tree was dirty or had no Git commit;
use a clean tagged commit when producing a public release.

The manual package workflow uploads a GitHub Actions artifact for review. It does
not publish a GitHub Release, upload to PlayFlow, or push an image to a registry.

## Running locally

```sh
make local-up
make plugin-load
make integration
make local-logs
make local-down
```

The Compose project name is `pf-nakama-local`; it has its own database volume and
network. Host access is bound only to loopback:

| Service | Local address | Development credential |
| --- | --- | --- |
| Nakama HTTP / WebSocket API | `http://127.0.0.1:17350` | Server key `local-server-key` |
| Nakama Console | `http://127.0.0.1:17351` | `admin` / `local-console-password` |
| PlayFlow mock | `http://127.0.0.1:18090/api` | `test-playflow-key` |
| PostgreSQL | `127.0.0.1:15432` | `postgres` / `local-postgres-password` |

The local stack uses separate `nakama` and `fleet` databases. Its database init
script runs when the local volume is first created. `nakama-migrate` owns Nakama's
schema; `fleet-migrate` owns the plugin's schema. Both must exit successfully
before Nakama starts. Production should use separate database credentials and
grant the plugin access only to its own database.

`make local-down` keeps the volume so restart/recovery can be tested. To reset
only this development stack and delete its saved state, explicitly run:

```sh
docker compose -f deploy/compose.yml down --volumes
```

The mock does not create Unity servers or access the Docker socket. The smoke
runner supplies simulated agent events. A real Linux server must still implement
the server-agent protocol and expose the configured FishNet game port. Timing
measurements in an emulated amd64 container on Apple Silicon are not production
capacity measurements.

## Installing with the official image

Use a packaged plugin for exactly the corresponding Nakama version and
architecture. Mount it read-only in the standard module directory:

```yaml
services:
  nakama:
    image: registry.heroiclabs.com/heroiclabs/nakama:3.41.0
    platform: linux/amd64
    volumes:
      - ./playflow.so:/nakama/data/modules/playflow.so:ro
```

This fragment describes only plugin installation. A complete deployment still
needs Nakama's database, migration, configuration, and the fleet environment. The
`deploy/production.env.example` file lists configuration without usable secrets.
Use your normal secret management system, a real HTTPS control address reachable
from PlayFlow, and `FLEET_MODE=production`; never deploy the local Compose file to
a public host. `FLEET_SIGNING_KEY` is a cryptographically random 32-byte key
encoded with unpadded base64url. `FLEET_ADMIN_TOKEN` is a separate random secret
of at least 32 characters. HTTPS is mandatory in production.

Alternatively build `deploy/Dockerfile`'s `runtime` target. It derives from the
same official Nakama image and adds only the plugin. Replacing the plugin requires
a Nakama restart; in-process `.so` hot replacement is not supported. Neither path
modifies Nakama core or the official Unity SDK.

Set Nakama's `shutdown_grace_sec` to a positive value so registered shutdown
hooks run, and give the container a longer stop grace period. Local defaults are
10 seconds and 15 seconds respectively. This stops the control process cleanly;
it does not by itself drain active Unity matches or replace the fleet's drain
protocol.

## Upgrading Nakama or the plugin

1. Read the target Nakama release's `go.mod` and builder image. Update the runtime
   image, builder image, exact Go version, and every shared dependency together.
2. Check the `FleetManager` interface for changes and compile all adapters.
3. Run the unit/race tests and real Docker runtime-load/integration tests on every
   architecture being released. Adding ARM64 requires a separately tested build;
   an amd64 `.so` cannot be reused.
4. Review database migration backward compatibility and the Unity wire protocol.
   Apply migrations before Nakama starts. Avoid destructive migrations while an
   old plugin might need to be restored.
5. Publish a new compatibility-specific artifact and a release note with the
   tested images, Go version, common version, dependency hashes, schema version,
   protocol version, and Unity package version.
6. Deploy to staging, perform real PlayFlow admission/reconnect/drain probes, then
   promote the same image. Keep the preceding image and its schema compatibility
   information for rollback.

A new Unity game build changes the game's build hash and instance pool, not the
Nakama Go toolchain. Route new allocations to the new build, drain the old pool,
and preserve active matches/reconnect windows until release conditions are met.
Provider/agent failures and destructive cloud operations require separate
operational testing beyond loading the plugin.
