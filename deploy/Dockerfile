# The runtime, builder, Go version, and shared Go modules are one compatibility
# unit. See deploy/compatibility.env and docs/compatibility.md before updating.
ARG NAKAMA_IMAGE=registry.heroiclabs.com/heroiclabs/nakama:3.41.0@sha256:ca9f3fe65c90f56860fe5d7cd024868a395cb1a001abb9912bf87ef47d605349
ARG PLUGIN_BUILDER_IMAGE=registry.heroiclabs.com/heroiclabs/nakama-pluginbuilder:3.41.0@sha256:eb506121ec2e67f39febee3a4252ae05598a0b39d3517b713ec1577714a9e5b5
FROM ${PLUGIN_BUILDER_IMAGE} AS dependencies
WORKDIR /backend
ENV GOTOOLCHAIN=local CGO_ENABLED=1
RUN test "$(go env GOVERSION)" = "go1.27.1"
COPY go.mod go.sum ./
RUN go mod download

FROM dependencies AS builder
COPY . .
RUN ./scripts/check-compatibility.sh --plugin
RUN --mount=type=cache,id=nakama-playflow-go1.27.1-linux-amd64,target=/root/.cache/go-build mkdir -p /out && go build --trimpath --mod=readonly --buildmode=plugin -o /out/playflow.so ./cmd/nakama-plugin
RUN --mount=type=cache,id=nakama-playflow-go1.27.1-linux-amd64,target=/root/.cache/go-build CGO_ENABLED=0 go build --trimpath --mod=readonly -o /out/fleet-migrate ./cmd/fleet-migrate
RUN --mount=type=cache,id=nakama-playflow-go1.27.1-linux-amd64,target=/root/.cache/go-build CGO_ENABLED=0 go build --trimpath --mod=readonly -o /out/playflow-mock ./cmd/playflow-mock

# Tools run only as explicit migration/development services.
FROM ${NAKAMA_IMAGE} AS tools
ARG PLUGIN_VERSION=0.1.0-dev
ARG VCS_REF=uncommitted
LABEL org.opencontainers.image.title="Nakama PlayFlow migration tools" \
      org.opencontainers.image.version="${PLUGIN_VERSION}" \
      org.opencontainers.image.source="https://github.com/xuhuanhello/nakama-playflow" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.licenses="MIT"
HEALTHCHECK NONE
COPY --from=builder /out/fleet-migrate /usr/local/bin/fleet-migrate
COPY --from=builder /out/playflow-mock /usr/local/bin/playflow-mock
ENTRYPOINT ["/usr/local/bin/fleet-migrate"]

# Extract both the module and its independent database migration tool.
FROM scratch AS artifact
COPY --from=builder /out/playflow.so /playflow.so
COPY --from=builder /out/fleet-migrate /fleet-migrate

FROM ${NAKAMA_IMAGE} AS runtime
ARG PLUGIN_VERSION=0.1.0-dev
ARG VCS_REF=uncommitted
LABEL org.opencontainers.image.title="Nakama PlayFlow FleetManager" \
      org.opencontainers.image.version="${PLUGIN_VERSION}" \
      org.opencontainers.image.source="https://github.com/xuhuanhello/nakama-playflow" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.description="Official Nakama runtime with the PlayFlow FleetManager module" \
      io.nakama.version="3.41.0" \
      io.nakama.go.version="1.27.1" \
      io.nakama.common.version="1.48.0"
COPY --from=builder /out/playflow.so /nakama/data/modules/playflow.so
