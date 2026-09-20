# Validation — 2026-09-21

Status: targeted lifecycle acceptance passed on real PlayFlow with a game-specific Unity/FishNet adapter. Local/mock regressions and real cloud checks are distinguished below; these are not production capacity or latency certification.

## Local/mock baseline

- Official Nakama `3.41.0`, official matching pluginbuilder, actual builder Go `1.27.1`, `nakama-common v1.48.0`.
- Plugin `linux/amd64`, running under Docker Desktop on an Apple Silicon host.
- Separate real PostgreSQL databases for Nakama and the fleet; simulated PlayFlow provider and simulated game-server agents.
- Final integration-tested and packaged `playflow.so` SHA-256: `7b51b5a8cf9af836cd5531a5169dce8fb1df3cdb008eca5ea3abf062489054f3`.
- Archive: `dist/nakama-playflow-0.1.0-dev-nakama-3.41.0-linux-amd64.tar.gz`.

The archive's source revision is recorded as uncommitted; it is a local development artifact, not a public tagged release.

## Local/mock checks performed

| Check | Result and scope |
| --- | --- |
| `make test test-race` | Pass: formatting, Go package tests, vet and race checks |
| Core lifecycle regressions | 15 tests, covering readiness, room packing, concurrent admission, rollback, cancellation/late preparation, cleanup retry, callback delivery, temporary loss, ownership mismatch, safe drain, unknown creation, resume expiry, token validation, profile/lease fencing and forced TTL |
| PlayFlow adapter/mock | Pass: pagination, limits, network ports, auth/redaction, ambiguous creation, 429, timeouts and stop retries |
| PostgreSQL integration | 5 tests pass with `-race -count=1`: independent connection concurrency, admission exclusivity, rollback, namespace isolation and close/reopen persistence |
| Official Nakama runtime | `.so` actually loads; FleetManager, matched hook, custom HTTP endpoint and RPCs register |
| Real Nakama matchmaker with simulated workers | Real login + WebSocket matching creates three two-player rooms on two simulated physical workers; each user receives its own signed seat ticket |
| Authorization/cancel | Cross-user assignment denied; cancellation releases room only after simulated server cleanup acknowledgement |
| Nakama restart | Same allocation/room ownership persists in PostgreSQL; heartbeats continue and a refreshed Nakama session obtains a new resume ticket |
| Drain | Active rooms survive drain; pending results prevent provider stop; clearing work leads to stop confirmation; other worker's active room remains |
| Artifact verification | Archive checksums pass; ELF architecture checked; packaged `.so` hash equals the tested running plugin |
| Unity portable suite | 17 tests pass with official Unity Newtonsoft DLL and public NuGet dependency; Go-generated HMAC fixture and concurrency/fencing cases included |
| Unity compile | All runtime/sample C# compile against Unity2022.3.62f3 + .NET Standard2.1, zero warnings/errors |
| Nakama client example | Exact documented SDK glue compiles against official NakamaClient3.22.0, zero warnings/errors |

The first smoke run exposed an expired Nakama login token after its restart phase. The runner now uses the official session refresh endpoint on401 and retries once. The final full run passed with the same runtime behavior; token lifetimes were not bypassed or extended.

## Real PlayFlow and game acceptance

The cloud runs used official Nakama `3.41.0` with the Go plugin, a real PostgreSQL-backed controller, and PlayFlow Free `small` instances in `sea` and `us-west`. The game server was a Unity `2022.3.62f3` Linux IL2CPP release build with a game-owned Host adapter and actual FishNet UDP transport. This adapter implements authentication, authoritative gameplay, reconnect and durable results; those game-specific parts are not supplied by the generic package.

| Check | Observed result and scope |
| --- | --- |
| Single room, both regions | Two real UDP clients, four verified authoritative commands, one same-seat reconnect and durable result receipt before closure; protocol and cleanup passed, allocation reached `completed`. The default latency gate failed as recorded below. |
| Unity Editor plus one UDP companion, `us-west` | Four verified commands across both players; the Editor recovered from an injected transport disconnect into the original room/seat, continued play, then initiated leave. Editor receipt, companion protocol checks and allocation cleanup passed. |
| Two simultaneous rooms, `us-west` | Both rooms were ready concurrently on the same physical instance: four connected players, eight verified commands, two successful restores, and two of two allocations released as `completed`. This demonstrates room packing, not a measured capacity limit. |
| Result and room lifecycle | The game's durable result receipt preceded room closure; closed-state acknowledgement allowed reclamation. No pending results remained after the successful cleanup. |
| Idle instance shutdown | After the two-room run, the controller drained and stopped the only test instance within the 180-second observation window. This was a real single-instance scale-in observation, not a multi-instance scaling test. |

The Editor and two-room runs explicitly used a 3,000 ms input-to-authoritative-begin p95 threshold for functional acceptance. The two-room run observed 474 ms input p95 from eight samples and transport RTT p50/p95 of 436/440 ms. That small sample does not supersede the single-room failures or establish a production SLO.

## Latency measurements and limits

The two single-room runs retained their failed overall latency result despite successful gameplay and cleanup:

| Region | Transport RTT p50 | Input-to-authoritative-begin p95 | Input samples | Default 1,000 ms input gate |
| --- | ---: | ---: | ---: | --- |
| `sea` | 536 ms | 1,715 ms | 4 | Fail |
| `us-west` | 434 ms | 1,404 ms | 4 | Fail |

These are observations from the test network route, not regional performance benchmarks. With only four inputs per run, the reported p95 is the largest observed input delay. Raising the later functional-test threshold did not change or waive these failures.

Remaining acceptance:

- Sustained capacity and latency under representative player load, simultaneous simulation bursts, deferred audits and storage pressure.
- Real horizontal scale-out and multi-instance drain/scale-in. The test account allowed one concurrent Free instance with a one-hour lifetime; the earlier mock run covered three rooms on two simulated workers, not real multi-worker cloud capacity.
- Real cloud fault campaigns, including controller restart during active gameplay, extended callback/storage outages and lifetime-expiry recovery. The restart and forced-TTL regressions above used simulated game workers.
- Multi-build/region routing, production rolling deployment, high-throughput storage and operator repair workflows.

The successful mock workers and real cloud test instances were stopped after validation. Only anonymous aggregates are published here; private game source, raw reports, endpoints, instance/player identifiers and credentials are excluded. The earlier local package archive remains a development artifact, not a tagged release.
