#!/bin/sh
# sqlc needs a clean schema (no goose Up/Down, no DROPs). This script derives
# internal/storage/pg/schema.sql by concatenating the Up blocks of EVERY goose
# migration in db/postgres (in order), so multiple migrations compose. The
# migrations are the single source; schema.sql is generated.
# Run by `make generate` before `sqlc generate`.
set -eu
SRC_DIR="${1:-../db/postgres}"
DST="${2:-internal/storage/pg/schema.sql}"
: >"$DST.tmp"
for f in "$SRC_DIR"/[0-9][0-9][0-9]_*.sql; do
	[ -e "$f" ] || continue
	# Append the Up block (between -- +goose Up and -- +goose Down), minus the
	# StatementBegin/End markers.
	awk '/^-- \+goose Up/{f=1;next} /^-- \+goose Down/{f=0} f && !/^-- \+goose Statement/' "$f" >>"$DST.tmp"
done
# Keep the DDL itself; drop leading comment headers (plan commentary, not types).
awk '
  /^CREATE TABLE/ { in_ddl=1 }
  /^ALTER TABLE/ { in_ddl=1 }
  /^CREATE INDEX/ { in_ddl=1 }
  /^CREATE UNIQUE/ { in_ddl=1 }
  /^INSERT INTO plan_entitlement/ { in_ddl=1 }
  in_ddl { print }
' "$DST.tmp" >"$DST"
rm -f "$DST.tmp"
echo "extract-schema: $DST ($(wc -l <"$DST") lines) <- $SRC_DIR/*.sql"
