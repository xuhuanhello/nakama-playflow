# Validation — 2026-09-21

Status: initial development integration accepted locally. Real PlayFlow and an actual game's FishNet adapter are not yet acceptance-tested.

## Tested baseline

- Official Nakama `3.41.0`, official matching pluginbuilder, actual builder Go `1.27.1`, `nakama-common v1.48.0`.
- Plugin `linux/amd64`, running under Docker Desktop on an Apple Silicon host.
- Separate real PostgreSQL databases for Nakama and the fleet; simulated PlayFlow provider and simulated game-server agents.
- Final integration-tested and packaged `playflow.so` SHA-256: `7b51b5a8cf9af836cd5531a5169dce8fb1df3cdb008eca5ea3abf062489054f3`.
- Archive: `dist/nakama-playflow-0.1.0-dev-nakama-3.41.0-linux-amd64.tar.gz`.

The archive's source revision is recorded as uncommitted; it is a local development artifact, not a public tagged release.

## Checks performed

| Check | Result and scope |
| --- | --- |
| `make test test-race` | Pass: formatting, Go package tests, vet and race checks |
| Core lifecycle regressions | 15 tests, covering readiness, room packing, concurrent admission, rollback, cancellation/late preparation, cleanup retry, callback delivery, temporary loss, ownership mismatch, safe drain, unknown creation, resume expiry, token validation, profile/lease fencing and forced TTL |
| PlayFlow adapter/mock | Pass: pagination, limits, network ports, auth/redaction, ambiguous creation, 429, timeouts and stop retries |
| PostgreSQL integration | 5 tests pass with `-race -count=1`: independent connection concurrency, admission exclusivity, rollback, namespace isolation and close/reopen persistence |
| Official Nakama runtime | `.so` actually loads; FleetManager, matched hook, custom HTTP endpoint and RPCs register |
| Real matchmaker flow | Real login + WebSocket matching creates three two-player rooms on two simulated physical workers; each user receives its own signed seat ticket |
| Authorization/cancel | Cross-user assignment denied; cancellation releases room only after simulated server cleanup acknowledgement |
| Nakama restart | Same allocation/room ownership persists in PostgreSQL; heartbeats continue and a refreshed Nakama session obtains a new resume ticket |
| Drain | Active rooms survive drain; pending results prevent provider stop; clearing work leads to stop confirmation; other worker's active room remains |
| Artifact verification | Archive checksums pass; ELF architecture checked; packaged `.so` hash equals the tested running plugin |
| Unity portable suite | 17 tests pass with official Unity Newtonsoft DLL and public NuGet dependency; Go-generated HMAC fixture and concurrency/fencing cases included |
| Unity compile | All runtime/sample C# compile against Unity2022.3.62f3 + .NET Standard2.1, zero warnings/errors |
| Nakama client example | Exact documented SDK glue compiles against official NakamaClient3.22.0, zero warnings/errors |

The first smoke run exposed an expired Nakama login token after its restart phase. The runner now uses the official session refresh endpoint on401 and retries once. The final full run passed with the same runtime behavior; token lifetimes were not bypassed or extended.

## Remaining acceptance

- Actual FishNet transport handshake, authenticated room routing and game Host integration.
- Real Linux game process startup on PlayFlow, actual account permissions, port mapping and returned TTL behavior.
- Real game reconnect/replacement and result/replay upload ownership before room closure.
- Capacity and timing measurements on the selected PlayFlow compute size, including simultaneous simulation bursts and deferred audits.
- Multi-build/region routing, production rolling deployment, normalized high-throughput storage, operator repair tooling and additional scaling policy.

All simulated cloud workers used by the successful smoke run were stopped. The isolated Compose services and database volume were retained for inspection after validation. This validation did not create real PlayFlow resources or publish a GitHub Release or registry image.
