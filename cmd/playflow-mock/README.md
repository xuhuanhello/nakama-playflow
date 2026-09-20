# Local PlayFlow v3 simulator

This standard-library-only process models the subset of the official v3 API used by the adapter. It does not contact PlayFlow, launch game servers, or use a Docker socket. Its memory is reset on restart. Public endpoint ports are illustrative and have no UDP listener; the separate game-server simulator drives the fleet callbacks.

```sh
PLAYFLOW_MOCK_TEST_MODE=true go run ./cmd/playflow-mock
```

Defaults:

| Environment | Default | Meaning |
|---|---|---|
| `PLAYFLOW_MOCK_ADDR` | `:8090` | HTTP bind address; publish to loopback only in Docker |
| `PLAYFLOW_MOCK_API_KEY` | `test-playflow-key` | Required `api-key` header |
| `PLAYFLOW_MOCK_TEST_MODE` | `false` | Enable authenticated test controls and environment introspection |
| `PLAYFLOW_MOCK_PUBLIC_HOST` | `127.0.0.1` | Simulated public game host |
| `PLAYFLOW_MOCK_READY_DELAY_MS` | `500` | Delay before new instances become `running` |
| `PLAYFLOW_MOCK_AMBIGUOUS_CREATES` | `0` | First N creates succeed internally but return 503 |
| `PLAYFLOW_MOCK_RATE_LIMIT_REQUESTS` | `0` | Next N public API requests return 429 with `Retry-After: 1` |
| `PLAYFLOW_MOCK_STOP_FAILURES` | `0` | Next N DELETE requests return 503 and keep the instance alive |

Point the adapter at `http://playflow-mock:8090/api` in Compose or `http://127.0.0.1:8090/api` on the host. The mock supports `POST /api/v3/servers/start`, `GET /api/v3/servers`, and `GET/DELETE /api/v3/servers/{instance_id}`. Listings include launching servers only when `include_launching=true` and use `limit`, `offset`, and `has_more`. The simulated default mapping has name `game`, internal UDP port `7770`, and external port `30000`; custom `port_configs` are supported. An explicit TTL is enforced from creation.

Only when test mode is enabled:

- `GET /__mock/instances` returns `{"instances":[...]}` with normal instance fields and an extra `environment_variables` object. This lets the local worker simulator read its generated per-worker credentials. These values never appear in normal start/get/list responses.
- `GET /__mock/faults` reads remaining fault counters.
- `POST /__mock/faults` updates only fields present in the JSON body: `ready_delay_ms`, `ambiguous_creates`, `rate_limit_requests`, `stop_failures`. Readiness delay changes affect future instances.

All three endpoints require the same test API key. The unauthenticated `GET /healthz` returns only process health. Do not enable test introspection or expose this mock on a public production network.

The mock does not implement provider billing limits, build upload, machine metrics, pooling, real proxy networking, or provider-internal failures. A successful mock test is not a PlayFlow cloud acceptance test.
