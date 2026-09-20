GO ?= go
DOCKER ?= docker
COMPOSE = $(DOCKER) compose -f deploy/compose.yml

.PHONY: test test-race test-postgres fmt-check vet build local-up local-down local-logs plugin-load integration package clean

test: fmt-check
	$(GO) test -mod=readonly ./...
	$(GO) vet -mod=readonly ./...

test-race:
	$(GO) test -mod=readonly -race ./...

fmt-check:
	@test -z "$$(gofmt -l cmd internal pkg)" || { echo 'Run gofmt on the files listed below:'; gofmt -l cmd internal pkg; exit 1; }

vet:
	$(GO) vet -mod=readonly ./...

build:
	$(DOCKER) build --platform linux/amd64 -f deploy/Dockerfile --target runtime -t pf-nakama-local:dev .

local-up:
	$(COMPOSE) up -d --build --wait --wait-timeout 180

# Keep the local database volume. Use the documented reset command explicitly
# when a fresh database is needed; ordinary shutdown must not delete state.
local-down:
	$(COMPOSE) down

local-logs:
	$(COMPOSE) logs --tail 100 -f nakama playflow-mock

plugin-load:
	./scripts/check-plugin-load.sh

test-postgres: plugin-load
	FLEET_TEST_DATABASE_URL='postgres://postgres:local-postgres-password@127.0.0.1:15432/fleet?sslmode=disable' $(GO) test -mod=readonly -race -count=1 -timeout 90s ./internal/state

integration: test-postgres
	node scripts/smoke.mjs

package:
	./scripts/package-release.sh

clean:
	rm -rf dist
