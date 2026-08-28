#!/bin/sh
# UpControl self-host installer. POSIX sh on purpose: it must run on a bare
# Debian/Alpine box before anything else is installed. Idempotent — a re-run
# keeps existing secrets and .env answers.
#
#   ./install.sh                fresh install (pull images from ghcr.io)
#   ./install.sh --from-source  build the images from this checkout instead
#   ./install.sh --update       git pull && docker compose pull && up -d
set -eu

cd "$(dirname "$0")"

COMPOSE="docker compose"
COMPOSE_FILES="-f docker-compose.yml"

say()  { printf '%s\n' "$*"; }
fail() { printf 'install: %s\n' "$*" >&2; exit 1; }

# --- flags -------------------------------------------------------------------

MODE=install
FROM_SOURCE=0
for arg in "$@"; do
	case "$arg" in
	--update)      MODE=update ;;
	--from-source) FROM_SOURCE=1 ;;
	*) fail "unknown flag: $arg (known: --update, --from-source)" ;;
	esac
done
if [ "$FROM_SOURCE" = 1 ]; then
	COMPOSE_FILES="-f docker-compose.yml -f docker-compose.build.yml"
fi

# --- preflight ---------------------------------------------------------------

command -v docker >/dev/null 2>&1 || fail "docker is not installed (https://docs.docker.com/engine/install/)"
docker compose version >/dev/null 2>&1 || fail "docker compose v2 is not available (the 'docker-compose' v1 binary is not enough)"

# RAM floor: 3.5GB, or 1.5GB with active swap. Below that ClickHouse gets
# OOM-killed under merge load and the box is not usable — better to refuse
# now than to fall over on the first busy hour.
if [ -r /proc/meminfo ]; then
	mem_kb=$(awk '/^MemTotal:/{print $2}' /proc/meminfo)
	swap_kb=$(awk '/^SwapTotal:/{print $2}' /proc/meminfo)
elif command -v sysctl >/dev/null 2>&1 && sysctl -n hw.memsize >/dev/null 2>&1; then
	mem_kb=$(( $(sysctl -n hw.memsize) / 1024 ))
	swap_kb=1 # macOS swaps dynamically; treat as active
else
	mem_kb=0 swap_kb=0
	say "warning: cannot read total RAM on this OS; skipping the memory check"
fi
if [ "$mem_kb" -gt 0 ]; then
	if [ "$mem_kb" -lt 3670016 ] && { [ "$mem_kb" -lt 1572864 ] || [ "${swap_kb:-0}" -eq 0 ]; }; then
		fail "not enough memory: 4GB RAM recommended, 2GB + active swap minimum (found $((mem_kb / 1024))MB RAM, $((${swap_kb:-0} / 1024))MB swap)"
	fi
fi

# Disk floor: 10GB free where the volumes will live.
free_kb=$(df -Pk . | awk 'NR==2{print $4}')
[ "$free_kb" -ge 10485760 ] || fail "not enough disk: 10GB free required (found $((free_kb / 1024 / 1024))GB)"

# --- update path -------------------------------------------------------------

# Bring the checkout to its upstream tip.
#
# This was `git pull --ff-only`, which is right until a release rewrites
# history — and then it aborts with "Not possible to fast-forward" and the
# update path is dead for everybody who already cloned. That is not
# hypothetical: it is how the first --update of rehearsal #3 failed, 14
# seconds in (2026-08-20).
#
# So: fast-forward whenever it is possible, and when upstream has been
# rewritten, land on it — but never by quietly throwing something away.
# Uncommitted work stops the update with instructions. Commits that are about
# to leave the branch are printed AND tagged first, so they stay reachable by
# name rather than surviving only until the next gc.
#
# .env and secrets/ are untracked (see .gitignore), so no path through this
# function touches them.
update_checkout() {
	if ! git rev-parse --git-dir >/dev/null 2>&1; then
		say "warning: not a git checkout — updating images only, source left alone"
		return 0
	fi
	upstream=$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null) || \
		fail "this branch tracks nothing upstream. Set it once:
      git branch --set-upstream-to=origin/master"
	remote=${upstream%%/*}
	git fetch --quiet "$remote" || fail "could not reach '$remote'"

	if git merge-base --is-ancestor HEAD "$upstream"; then
		git merge --ff-only "$upstream"
		return 0
	fi
	if git merge-base --is-ancestor "$upstream" HEAD; then
		say "This checkout is ahead of $upstream — leaving the source as it is."
		return 0
	fi

	# Diverged. Either upstream was rewritten or there are local commits, and
	# from here the two are indistinguishable, so both get the same care.
	# Tracked changes only: a reset clobbers those and leaves untracked files
	# alone, so a stray note or a backup in this directory must not be what
	# blocks somebody's update.
	if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
		fail "$upstream has been rewritten, and this checkout has uncommitted changes.
  Nothing has been touched. Save them first, then re-run:
      git stash push -u -m before-upcontrol-update
      ./install.sh --update
  (.env and secrets/ are untracked and are never touched either way.)"
	fi
	rescue="pre-update-$(git rev-parse --short HEAD)"
	say "warning: $upstream has been rewritten. These commits are leaving this branch:"
	git --no-pager log --oneline "$upstream..HEAD" | sed 's/^/         /'
	git tag -f "$rescue" HEAD >/dev/null
	say "         They are kept as the tag '$rescue' — 'git log $rescue' still finds them."
	git reset --hard "$upstream"
}

if [ "$MODE" = update ]; then
	[ -f .env ] || fail "no .env here — run ./install.sh first"
	say "Updating: sync the source, then pull images and restart."
	update_checkout
	$COMPOSE $COMPOSE_FILES pull
	$COMPOSE $COMPOSE_FILES up -d
	say "Updated. The ClickHouse pin only moves when a release moves it — majors never jump on their own."
	exit 0
fi

# --- secrets -----------------------------------------------------------------

command -v openssl >/dev/null 2>&1 || fail "openssl is required to generate secrets"
mkdir -p secrets
for s in pg_password ch_password node_token secret_key_hex; do
	if [ ! -s "secrets/$s" ]; then
		openssl rand -hex 32 > "secrets/$s"
		say "secrets/$s: generated"
	fi
done
for s in telegram_bot_token smtp_password; do
	[ -f "secrets/$s" ] || : > "secrets/$s"
done

# --- four questions ----------------------------------------------------------

if [ -f .env ]; then
	say ".env already exists — keeping it (delete it to be asked again)."
else
	say ""
	say "Four questions. Enter accepts the default in brackets."
	say ""

	printf 'Is this instance reachable from the internet? [y/N] '
	read -r internet_facing || internet_facing=""
	case "$internet_facing" in
	[yY]*) UC_AUTH=magic-link ;;
	*)     UC_AUTH=none ;;
	esac

	printf 'Public domain, e.g. status.example.com (Enter for none): '
	read -r UC_DOMAIN || UC_DOMAIN=""

	printf 'SMTP relay host for email alerts/sign-in (Enter to skip): '
	read -r UC_SMTP_HOST || UC_SMTP_HOST=""
	UC_SMTP_PORT=587 UC_SMTP_USERNAME="" UC_SMTP_FROM=""
	if [ -n "$UC_SMTP_HOST" ]; then
		printf 'SMTP port [587]: ';      read -r UC_SMTP_PORT || true; UC_SMTP_PORT=${UC_SMTP_PORT:-587}
		printf 'SMTP username: ';        read -r UC_SMTP_USERNAME || true
		printf 'SMTP from address: ';    read -r UC_SMTP_FROM || true
		printf 'SMTP password (stored in secrets/smtp_password): '
		read -r smtp_pass || smtp_pass=""
		printf '%s' "$smtp_pass" > secrets/smtp_password
	fi

	printf 'Telegram bot token from @BotFather (Enter to skip): '
	read -r tg_token || tg_token=""
	UC_TELEGRAM_BOT_USERNAME=""
	if [ -n "$tg_token" ]; then
		printf '%s' "$tg_token" > secrets/telegram_bot_token
		printf 'Telegram bot username (without @): '
		read -r UC_TELEGRAM_BOT_USERNAME || true
	fi

	if [ -n "$UC_DOMAIN" ]; then
		UC_PUBLIC_ORIGIN="https://$UC_DOMAIN"
	else
		UC_PUBLIC_ORIGIN="https://localhost"
	fi

	cat > .env <<EOF
# Written by install.sh $(date -u +%Y-%m-%dT%H:%M:%SZ). See .env.example for
# every knob and what it means.
UC_VERSION=latest
UC_AUTH=$UC_AUTH
UC_OWNER_EMAIL=owner@localhost
UC_DOMAIN=$UC_DOMAIN
UC_PUBLIC_ORIGIN=$UC_PUBLIC_ORIGIN
UC_SMTP_HOST=$UC_SMTP_HOST
UC_SMTP_PORT=$UC_SMTP_PORT
UC_SMTP_USERNAME=$UC_SMTP_USERNAME
UC_SMTP_FROM=$UC_SMTP_FROM
UC_SMTP_FROM_NAME=UpControl
UC_TELEGRAM_BOT_USERNAME=$UC_TELEGRAM_BOT_USERNAME
EOF
	say ""
	say ".env written."
fi

# --- bring it up -------------------------------------------------------------

if [ "$FROM_SOURCE" = 1 ]; then
	say "Building images from source..."
	$COMPOSE $COMPOSE_FILES up -d --build
else
	say "Pulling images..."
	$COMPOSE $COMPOSE_FILES pull
	$COMPOSE $COMPOSE_FILES up -d
fi

# --- wait for /health --------------------------------------------------------

say "Waiting for the API to answer /health..."
tries=0
until curl -ksf https://localhost/health >/dev/null 2>&1; do
	tries=$((tries + 1))
	[ "$tries" -lt 60 ] || fail "the API did not become healthy in 2 minutes — check: docker compose logs ucapi"
	sleep 2
done

# /health alone is not proof: a ucapi whose database wiring failed still
# serves /health (and nothing else). /v1/me answering 200 (UC_AUTH=none) or
# 401 (magic-link, no cookie) is what proves the API is actually wired.
say "Waiting for the API to answer /v1/me..."
tries=0
while :; do
	code=$(curl -kso /dev/null -w '%{http_code}' https://localhost/v1/me 2>/dev/null) || code=000
	case "$code" in 200|401) break ;; esac
	tries=$((tries + 1))
	[ "$tries" -lt 60 ] || fail "the API answers /health but /v1/me gives $code — check: docker compose logs ucapi"
	sleep 2
done

# --- done --------------------------------------------------------------------

domain=$(grep '^UC_DOMAIN=' .env | cut -d= -f2-)
if [ -n "$domain" ]; then
	url="https://$domain"
else
	url="https://localhost"
fi
auth=$(grep '^UC_AUTH=' .env | cut -d= -f2-)

say ""
say "UpControl is up: $url"
say ""
if [ "$auth" = none ]; then
	say "Auth is off (single-user mode): everyone who can reach the app is the"
	say "owner. If this box ever becomes internet-facing, set UC_AUTH=magic-link"
	say "in .env and run: docker compose up -d"
else
	say "Sign-in is by emailed magic link. Without SMTP the code lands in the"
	say "API log: docker compose logs ucapi | grep \"sign-in code\""
fi
say ""
say "Next steps: open $url, add your first website check, and wire the SDK"
say "(the app's Settings screen has the install command)."
say ""
say "This instance watches from one box; an outside-in check from"
say "upcontrol.io's free plan covers the box itself."
