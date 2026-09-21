# Fleet protocol v1

All times are Unix seconds. All routes use JSON and require HTTPS outside the local mock. Bodies are bounded to128KiB; command responses contain at most64 pending commands. Server credentials and admission tickets must not be logged.

## Player RPCs

The official Nakama SDK calls these as authenticated user RPCs. The payload is the JSON object below encoded as the normal Nakama RPC payload string; the result is a JSON string in Nakama's `payload` wrapper. The Unity helper consumes the decoded payload, not that outer wrapper.

| RPC | Payload | Result |
| --- | --- | --- |
| `fleet_assignment_get_v1` | `{}` or `{"allocation_id":"...","build_hash":"game-build","region":"sea"}` | Own current allocation; signed admission fields only when assigned/active and eligible |
| `fleet_resume_v1` | `{"allocation_id":"...","build_hash":"game-build","region":"sea"}` | Same owned seat, fresh nonce and short resume ticket within reconnect policy |
| `fleet_assignment_cancel_v1` | `{"allocation_id":"..."}` | `{"accepted":true}`; cleanup completion must still be queried |

Errors: unauthenticated16, missing5, forbidden7, unavailable9, internal13 (Nakama codes). An unrelated user's allocation cannot be queried. Active game departure uses the game protocol; allocation-cancel rejects active rooms.

`build_hash` is the existing wire name for the application compatibility version; it is an opaque configured string, such as `dm-v1` or `1`, and need not be a content hash. `build_hash` and `region` are optional on assignment-get and resume. When present, each must be a string exactly matching the configured fleet pool. Empty strings and `null` are mismatches. Omitting either field preserves its previous behavior, so existing SDK requests remain supported. An incompatible build returns `fleet_build_mismatch`; an incompatible region returns `fleet_region_mismatch`. Both use Nakama runtime code9. Authentication is checked first, then these fields, then allocation ownership/state. Wrong JSON field types return invalid-payload code3.

Clients can call `fleet_assignment_get_v1` with their build and region after login and before entering matchmaking. A compatible player without an allocation still receives the existing missing code5: it means no assignment exists and is not a profile mismatch. Handle the two stable mismatch messages explicitly and stop waiting/retrying with incompatible settings. This uses the existing RPC; no separate preflight endpoint is required.

Assignment fields:

```json
{
  "schema_version": 1,
  "allocation_id": "allocation-id",
  "revision": 4,
  "state": "assigned",
  "worker_id": "worker-id",
  "boot_id": "process-boot-id",
  "room_id": "room-id",
  "seat": 0,
  "endpoint": {"host": "game-host", "port": 30000, "transport": "tugboat-udp"},
  "build_hash": "game-build",
  "admission_token": "payload.signature",
  "expires_at": 1900000060,
  "reconnect_seconds": 90
}
```

`endpoint`, `boot_id` and `admission_token` are absent while waiting or terminal. Notifications use code1001, subject `fleet_assignment`, and only `{allocation_id,revision,state}`. Treat them as hints and use the RPC for recovery. Tokens and endpoints are never broadcast to the whole match.

## Matchmaker queue admission

The plugin registers a `RegisterBeforeRt("MatchmakerAdd", ...)` hook. Every ticket must include `string_properties.build_hash` and `string_properties.region` matching the pool. Missing or mismatched values return the same `fleet_build_mismatch` / `fleet_region_mismatch` errors (runtime code9) before Nakama accepts the ticket. The matched hook repeats this validation as a final check.

The hook preserves the accepted envelope, correlation ID, count fields, query, additional string properties and numeric properties. Existing group filters and other valid matchmaker constraints remain supported. If the application already registers a `MatchmakerAdd` before hook or a matched hook, compose its behavior with this plugin's hooks; do not silently replace either validation step. Clients must handle a rejected add request as a failed enqueue operation instead of waiting for a matchmaker event.

## Agent bootstrap

PlayFlow receives these environment variables per worker:

`FLEET_WORKER_ID`, `FLEET_BOOTSTRAP_TOKEN`, `FLEET_ADMISSION_KEY`, `FLEET_CONTROL_URL`, `FLEET_BUILD_HASH`, `FLEET_MAX_ROOMS`.

POST `/fleet/v1/agent/bootstrap`:

```json
{"worker_id":"worker-id","bootstrap_token":"secret","boot_id":"new-random-process-id","build_hash":"game-build"}
```

Response: `{ "agent_token":"secret", "heartbeat_interval_seconds":2 }`. Retrying with the same boot ID returns the same credentials. A new boot ID cannot adopt an old worker's rooms. Keep secrets in server memory/environment only.

## Heartbeat and commands

POST `/fleet/v1/agent/heartbeat`, `Authorization: Bearer <agent_token>`:

```json
{
  "worker_id":"worker-id", "boot_id":"process-boot-id", "sequence":1, "ready":true,
  "rooms":[{"room_id":"room-id","state":"playing","user_ids":["user-a","user-b"]}],
  "metrics":{
    "simulation_pending":0,"simulation_active":0,"simulation_oldest_seconds":0,
    "frame_p99_ms":10,"memory_bytes":100000000,
    "audit_pending":0,"audit_active":0,"pending_results":0
  },
  "command_results":[{"command_id":"command-id","success":true}]
}
```

Response:

```json
{
  "commands":[{
    "command_id":"new-command-id","type":"prepare_room","room_id":"room-id",
    "allocation_id":"allocation-id","epoch":1,"user_ids":["user-a","user-b"],
    "expires_at":1900000020
  }],
  "draining":false,"revision":12
}
```

The room snapshot is complete and contains at most the configured local room limit, including retained closed entries. Accepted states are `waiting_players`, `playing`, `between_rounds`, `closing`, `closed`. Exact heartbeat replay returns success without refreshing liveness or reapplying outcomes. An HTTP200 means the included observations/outcomes were committed; only then can that exact request be released.

Command types are `prepare_room`, `cancel_room`, `drain`. `prepare_room.expires_at` is a preparation deadline, not the final seat expiry. Late preparations must fail. Cleanup/drain commands remain relevant after their displayed expiry. Execute once per command ID; repeat delivery repeats its outcome. After a failed execution the controller may create a new command ID with backoff. Fence cancelled room/allocation/epoch even when no local room existed at cancellation time.

The response may include additional internal metadata such as worker identity and retry attempt; consumers ignore unknown response fields. Request schemas are strict. Local capacity and command/nonce/fence ledgers are bounded and must refuse new work rather than discard live safety state.

## Admission format

```text
payload   = base64url_without_padding(UTF8(JSON(claims)))
signature = base64url_without_padding(HMAC_SHA256(decoded_worker_admission_key, ASCII(payload)))
ticket    = payload + "." + signature
```

Claims: `schema_version=1`, `user_id`, `worker_id`, `boot_id`, `room_id`, `allocation_id`, `reservation_id`, `seat`, `epoch>=1`, `exp`, `nonce`, `resume`. Verify the exact encoded payload; do not reserialize it before checking the signature. `exp` must be strictly in the future and no more than60s ahead. Then validate process/room ownership, roster and seat, atomically consume the nonce and attach the connection. Maintain clocks with normal UTC synchronization.

## Operator routes

All use `Authorization: Bearer <FLEET_ADMIN_TOKEN>` and should be access-restricted:

- GET `/fleet/v1/admin/status`: state and safe error codes, no provider or admission secrets.
- POST `/fleet/v1/admin/drain` with `{"worker_id":"..."}`: graceful drain of a ready/suspect/draining worker. Unknown/lost resources require separate investigation.
- POST `/fleet/v1/admin/retry-creation`: clear a configuration failure circuit after correcting the provider configuration.
- POST `/fleet/v1/admin/allocate`: only in explicit mock mode, accepts `{user_ids:[...],request_key:"..."}` for tests. Not registered as an active handler in production.

The plugin's HTTP handlers implement their own auth. Proxy TLS termination, request rate limits and admin network restrictions belong to the deployment.
