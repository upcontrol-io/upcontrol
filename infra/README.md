# Self-hosting UpControl

One box, Docker Compose: Postgres, the API (`ucapi`), the worker
(`ucworker`), one probe (`ucprobe`), plus the web app and a Caddy edge. Prod
mode, HTTPS, single-user by default — no sign-in screen, no SMTP required.

## Requirements

- Docker with Compose v2 (`docker compose version`)
- **1GB RAM recommended; 512MB + active swap is the minimum.** Below ~900MB
  without swap the stack cannot ride out load peaks, and the installer
  refuses to start there.
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
openssl rand -hex 32 > secrets/node_token
openssl rand -hex 32 > secrets/secret_key_hex
: > secrets/telegram_bot_token             # empty = feature off
: > secrets/smtp_password
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

Retention is the ring, not TTL: each plan carries a log window, and the
worker recomputes `project_window.cutoff_seq` from the line ledger; every
read filters `seq >= cutoff_seq`. Physically, `logs` is range-partitioned by
day and partitions behind the 48-hour floor are dropped, so disk stays
bounded no matter what the window says. `events` (deploys, webhooks, custom
`track()` events) have no window by design: they are the "why" next to an
incident and are tiny compared to logs.

## Country data (optional)

Product analytics stamps an ISO country code onto each `web_events` row. The
lookup needs an MMDB country database — an 8 MB monthly download, deliberately
**not** shipped in this repository. Without one the country column stays empty
and nothing else changes.

To install one, drop it in `geoip/` next to `docker-compose.yml` — the compose
file mounts that directory read-only at the path ucapi reads once at startup
(`/var/lib/upcontrol/geoip/country.mmdb`, overridable with `UC_GEOIP_DB`):

```sh
mkdir -p geoip
curl -fsSL "https://download.db-ip.com/free/dbip-country-lite-$(date -u +%Y-%m).mmdb.gz" \
  | gunzip > geoip/country.mmdb
docker compose up -d ucapi
```

DB-IP publishes monthly and the new file appears partway through the month, so
fall back to the previous month if that URL 404s. Their free databases are
**CC BY 4.0**: crediting DB-IP is a condition of using one. Any MaxMind-format
country database works — GeoLite2-Country is the other common choice.

## Backups

Two things hold state; back up both:

```sh
# 1. Postgres (accounts, monitors, incidents, channels, logs, checks, metrics)
docker compose exec postgres pg_dump -U upcontrol upcontrol > upcontrol.sql

# 2. secrets/ — without secret_key_hex the encrypted columns are unreadable
cp -r secrets/ somewhere-safe/
```

## Updating

```sh
./install.sh --update    # = git pull && docker compose pull && docker compose up -d
```

## Resource tuning

The compose ships memory limits per service, sized so the whole stack fits a
1GB box. On a bigger machine raise the limits together.

## One box watching itself

This instance watches from one box; an outside-in check from
[upcontrol.io](https://upcontrol.io)'s free plan covers the box itself.
