# upcontrol backend

Three Go services — `ucapi`, `ucworker`, `ucprobe` — plus the contracts and
infrastructure for the upcontrol monitoring product. See
[`docs/plans/backend-build-plan.md`](../docs/plans/backend-build-plan.md) for the
authoritative build plan; this README is the operator/developer quickstart.

## Layout

```
back/
├── cmd/{ucapi,ucworker,ucprobe}/   one main.go per service; thin shells over app.Run
├── internal/platform/              config, logging, clock, health, shutdown, app
├── api/openapi.yaml                front contract (Phase 2)
├── rpc/probe/v1/probe.proto        probe contract (Phase 2)
├── gen/{pg,api,rpc}/               generated — DO NOT EDIT
└── Makefile, .golangci.yml, Dockerfile
```

## Build & develop

```sh
go mod download
make build        # compile the three binaries
make check        # vet + lint + unit tests — the per-commit gate
make cover        # coverage, ratchets against .coverage-baseline
```

`GOEXPERIMENT=jsonv2` is exported by the Makefile; the runtime JSON v2
implementation is default from Go 1.27, until then we set it explicitly. Codegen
and migration tools (`sqlc`, `oapi-codegen`, `buf`, `goose`, `golangci-lint`) are
pinned in `go.mod`'s `tool` block and run via `go tool X` — no global installs.

The container-backed targets (`make test-integration`, `make test-ha`) need a
reachable Docker daemon. testcontainers does not read the docker CLI's active
context, so the Makefile resolves `DOCKER_HOST` from `docker context inspect` —
which is what makes them pass on OrbStack/Colima/rootless setups where the
socket is not at `/var/run/docker.sock`. Running `go test -tags=integration`
directly still needs `DOCKER_HOST` in the environment.

## Run locally

```sh
cd ../infra/compose
mkdir -p secrets
openssl rand -hex 16 > secrets/pg_password
openssl rand -hex 16 > secrets/ch_password
openssl rand -hex 32 > secrets/node_token
openssl rand -hex 32 > secrets/secret_key_hex
docker compose up -d          # postgres, clickhouse, 2× ucapi, ucworker, ucprobe, caddy
curl -s http://localhost/health
```

## Connect your own Telegram bot

Optional, and off until both halves are present — with neither, the bot never
starts and the Alerts screen offers e-mail, Discord and Slack only.

```sh
# 1. the token from @BotFather
printf '123456:ABC-your-token' > ../infra/compose/secrets/telegram_bot_token

# 2. the bot's handle, so the deep link points at YOUR bot
echo 'UC_TELEGRAM_BOT_USERNAME=yourbot' > ../infra/compose/.env

docker compose up -d ucapi ucworker
```

Then in the app: **Alerts → Telegram → Open the chat → Start**. Pressing Start is
what connects it — a chat id is not something a person can type into a form, so
the deep link (`t.me/<bot>?start=prj-<id>`) carries the project and the bot binds
the chat as a destination when it hears from you. The buttons on an alert work
from that chat afterwards: acknowledging writes the incident's second mark,
resolving closes it through the same lifecycle the detector uses.

One instance polls at a time (a `pg_advisory_lock`), so running two `ucapi`
replicas does not double-deliver.

## Invariants (§2 of the plan)

Build-breaking, enforced here:

- `ucprobe` may not import `internal/storage/**` — depguard `no-storage-in-probe`.
- `internal/detect` may not read raw `logs` — depguard `no-raw-logs-in-detect`.
- The only path to the logs table is `internal/ring.QueryBuilder` — depguard
  `logs-only-via-ring`.
- Every "Logic" zone package ships a `_test.go` — `scripts/check-logic-tests.sh`.
- Secrets never enter logs — ingest scrubber test (Phase 3) + sloglint style.
