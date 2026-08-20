# Self-hosting Upcontrol

One box, Docker Compose, five services: Postgres, ClickHouse, the API
(`ucapi`), the worker (`ucworker`), one probe (`ucprobe`), plus the web app
and a Caddy edge. Prod mode, HTTPS, single-user by default — no sign-in
screen, no SMTP required.

## Requirements

- Docker with Compose v2 (`docker compose version`)
- **4GB RAM recommended; 2GB + active swap is the minimum.** Below that
  ClickHouse gets OOM-killed under merge load and the box is not usable.
- 10GB+ free disk (logs and metrics grow with your traffic; see Retention).

## Quickstart

```sh
git clone https://github.com/upcontrol-io/upcontrol
cd upcontrol/infra
./install.sh
```

The installer checks the floor above, generates secrets, asks four questions
(internet-facing? domain? SMTP? Telegram?), writes `.env`, pulls the images
and waits for `/health`. Building from source instead of pulling:
`./install.sh --from-source`.

Manual equivalent:

```sh
cp .env.example .env                       # edit to taste
mkdir -p secrets
openssl rand -hex 32 > secrets/pg_password
openssl rand -hex 32 > secrets/ch_password
openssl rand -hex 32 > secrets/node_token
openssl rand -hex 32 > secrets/secret_key_hex
: > secrets/telegram_bot_token             # empty = feature off
: > secrets/smtp_password
: > secrets/ai_api_key
docker compose pull && docker compose up -d
```

Then open `https://localhost` (or `https://$UC_DOMAIN`).

## Auth: single-user by default

The package defaults to `UC_AUTH=none`: the instance provisions one owner
account at boot and every request acts as them. There is no sign-in and no
session to steal, which is the right trade for a box that is not reachable
from the internet. **If the instance is internet-facing, set
`UC_AUTH=magic-link`** — otherwise anyone who can reach the app IS the owner.

### Signing in (magic-link mode)

With SMTP configured, sign-in codes arrive by email. SMTP can be set at
install time or later, from the app's Settings screen (Email relay) — no
restart either way. Without SMTP the code is written to the ucapi log
instead — fish it out with:

```sh
docker compose logs ucapi | grep "sign-in code"
```

Too many sign-in attempts from one IP answer 429 for five minutes (it can
surface as assorted 401s in the app). Reset the window:

```sh
docker compose exec postgres psql -U upcontrol -d upcontrol -c "DELETE FROM magic_link_ip;"
```

## TLS

- `UC_DOMAIN` set: Caddy provisions a real certificate automatically.
- No domain: Caddy signs with its own internal CA. Browsers warn until you
  trust it; export it with
  `docker compose exec caddy cat /data/caddy/pki/authorities/local/root.crt`.
- **Ingest over plain HTTP**: on domainless installs `POST /i` and
  `POST /v1/event` also answer on `http://<host>/`, so the SDK and CLI work
  without trusting the internal CA. Everything else on :80 redirects to
  HTTPS. Full-TLS alternative for Node agents: point `NODE_EXTRA_CA_CERTS`
  at the exported root certificate and use the `https://` endpoint.

## Retention

What the database keeps, straight from the schema:

| Data | Table | Kept |
| --- | --- | --- |
| Raw log lines | `logs` | 31 days |
| Raw probe checks | `checks` | 7 days |
| Log rates, per minute | `series_1m` | 90 days |
| Log rates, per hour | `series_1h` | 730 days |
| Check aggregates, per minute | `checks_1m` | 365 days |
| Check aggregates, per hour | `checks_1h` | 730 days |
| Raw metrics | `metrics` | 31 days |
| Metric aggregates | `metrics_5m` / `metrics_1h` | 90 / 730 days |
| Product analytics | `web_events` | 730 days |
| **Named events** | **`events`** | **forever — no TTL** |

The `events` table (deploys, webhooks, custom `track()` events) has no TTL by
design: events are the "why" next to an incident and are tiny compared to
logs. If yours are not tiny, add a TTL yourself:
`ALTER TABLE events MODIFY TTL toDateTime(ts) + INTERVAL 365 DAY;`

## Backups

Three things hold state; back up all three:

```sh
# 1. Postgres (accounts, monitors, incidents, channels)
docker compose exec postgres pg_dump -U upcontrol upcontrol > upcontrol.sql

# 2. ClickHouse (logs, checks, metrics) — cold copy of the volume
docker compose stop clickhouse
docker run --rm -v oss_chdata:/data -v "$PWD":/backup alpine \
  tar czf /backup/chdata.tar.gz -C /data .
docker compose start clickhouse

# 3. secrets/ — without secret_key_hex the encrypted columns are unreadable
cp -r secrets/ somewhere-safe/
```

## Updating

```sh
./install.sh --update    # = git pull && docker compose pull && docker compose up -d
```

ClickHouse is pinned to `clickhouse-server:24.8` and an update never jumps it
across a major version on its own: when the pin moves in a release, the
changelog says so and names the upgrade steps. Never edit the pin to `latest`.

## Resource tuning

The compose ships memory limits sized for a 4GB box and a ClickHouse
drop-in (`clickhouse/config.xml`) that caps the server at 800M. On a bigger
machine raise `max_server_memory_usage` and the compose limits together.

## One box watching itself

This instance watches from one box; an outside-in check from
[upcontrol.io](https://upcontrol.io)'s free plan covers the box itself.
