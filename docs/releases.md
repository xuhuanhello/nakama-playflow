# Tagged releases and deployment

The published candidate is [v0.1.0-rc.1](https://github.com/xuhuanhello/nakama-playflow/releases/tag/v0.1.0-rc.1),
for **Nakama 3.41.0 / linux/amd64**.
It remains a prerelease: real lifecycle acceptance does not establish production
capacity, latency or high availability. The standard release workflow publishes:

| Artifact | Candidate name |
| --- | --- |
| Nakama with the plugin | `ghcr.io/xuhuanhello/nakama-playflow:0.1.0-rc.1-nakama-3.41.0` |
| Database migration tools | `ghcr.io/xuhuanhello/nakama-playflow-tools:0.1.0-rc.1-nakama-3.41.0` |
| Standalone binaries | `nakama-playflow-0.1.0-rc.1-nakama-3.41.0-linux-amd64.tar.gz` |
| Release metadata | `images.json`, `compatibility.json`, `SHA256SUMS` |

Download the assets from [v0.1.0-rc.1](https://github.com/xuhuanhello/nakama-playflow/releases/tag/v0.1.0-rc.1).
The runtime and tools images are public. No `latest` tag is created. `images.json`
records the immutable runtime and tools references; pin those digests in deployment
configuration.

## Deployment

1. Download the release assets and verify `sha256sum -c SHA256SUMS`. Extract the
   archive and check its internal `SHA256SUMS` when using standalone binaries.
2. Keep Nakama's database and configuration. Create a separate PostgreSQL fleet
   database and role; configure its connection as `FLEET_DATABASE_URL`.
3. Run the release's tools image once with that variable, on the database's
   private network. Its default entrypoint is `fleet-migrate`. Apply Nakama's own
   migrations separately. Do not start the plugin before both schemas are ready.
4. Use the release runtime image, or mount its `playflow.so` read-only into the
   matching official Nakama image. Do not hide the runtime image's module with
   an empty mount over `/nakama/data` or `/nakama/data/modules`.
5. Supply [production configuration](../deploy/production.env.example) through
   server-side secrets. Set a new namespace, the exact game build/PlayFlow version,
   region, capacity and port name. Set `FLEET_MODE=production` and a real HTTPS
   `FLEET_CONTROL_URL`; the PlayFlow API base ends in `/api`, without `/v3`.
6. Terminate TLS at the ingress and preserve WebSocket upgrades for Nakama clients.
   Route `/fleet/v1/agent/bootstrap` and `/fleet/v1/agent/heartbeat` to Nakama's
   HTTP listener. Keep Console and `/fleet/v1/admin/*` private. No PlayFlow provider
   webhook or public Unity management port is required: the Agent calls back.
7. Deploy the game's external result storage before accepting matches. This
   generic plugin does not include a result database or business settlement
   service. Acknowledge durable results before reporting safe drain. Supply any
   game-specific configuration through `FLEET_SERVER_ENV_JSON`.
8. Set positive `shutdown_grace_sec` and a longer container stop grace period.
   Check actual module registration, then a real two-player admission, reconnect,
   result receipt and drain before enabling normal traffic.

The tools image also contains the local provider simulator, but its default
entrypoint only performs fleet migration. Do not start the simulator in production.
Database and result volumes need independent retention/backup. Changing game
build, region, room capacity, signing key or custom server environment requires
a new namespace; this release does not implement multi-build rolling rollout.
Keep the previous image digests and compatible configuration for rollback.
Drain active cloud rooms first; restarting Nakama alone does not drain them.

## Maintainer release procedure

Commit the reviewed release changes on `main`, pass Validate and Secret scan, then
create and push an unused version tag from that clean commit. For example, the
next candidate could be `v0.1.0-rc.2`; do not reuse the published `v0.1.0-rc.1`:

```sh
git tag -a v0.1.0-rc.2 -m "Nakama 3.41.0 release candidate 2"
git push origin v0.1.0-rc.2
```

The release workflow accepts tag pushes or a manual dispatch for an existing tag.
It requires a clean tagged checkout contained in `origin/main`, builds both images
once, tests those exact images with PostgreSQL and the real Nakama runtime, and
extracts their binaries. A final identity check rejects changed images/binaries.
It then creates a new draft, pushes only compatibility-specific version tags,
attaches checksums and digest metadata, and publishes after anonymous registry
verification. Runtimes use the digest-identical official Docker Hub mirrors to
avoid shared-runner rate limits; canonical Heroic Labs pins remain in metadata.

Publishing uses the workflow's `GITHUB_TOKEN` with `contents:write` and
`packages:write`. No PlayFlow key, game credentials or extra personal access token
is needed. Standard artifacts contain only generic plugin binaries and metadata.

GitHub makes a newly created container package private by default. When publishing
a new package namespace, make **both** packages public in their settings, following
[GitHub's visibility instructions](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility).
If anonymous verification failed, the already uploaded release remains a draft.
Do not rerun its publishing step or move/reuse the tag. Download its `images.json`
and finish the existing draft after verification:

```sh
gh release download v0.1.0-rc.2 --repo xuhuanhello/nakama-playflow --pattern images.json --dir /tmp/playflow-release-check
python3 scripts/release.py verify-public --manifest /tmp/playflow-release-check/images.json
gh release edit v0.1.0-rc.2 --repo xuhuanhello/nakama-playflow --draft=false --prerelease
```

If failure occurred before all images/assets were uploaded, inspect that draft
and publish a new candidate tag after fixing the cause. Existing drafts/releases
are never overwritten automatically. Later releases can complete unattended once
package visibility is public. Local release reproduction requires Docker/Buildx,
Compose, make, the pinned Go version, Node.js 22+, Python 3, curl and free local
integration ports. The Actions runner provides this isolated environment.
