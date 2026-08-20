#!/bin/sh
# Logic-zone coverage runner. Filters the aspirational LOGIC_PKGS list to
# packages that exist on disk (go test errors on missing dirs), runs them with
# -coverprofile, prints the total, and hands off to the baseline gate.
# Keeps the Makefile recipe a single call (shellcheck parses .sh cleanly).
set -eu

LOGIC_PKGS="$1"
COVER_OUT="$2"
COVER_BASELINE="$3"

# Resolve to existing directories only.
pkgs=""
for p in $LOGIC_PKGS; do
	d=$(echo "$p" | sed 's#^\./##; s#/...$##')
	if [ -d "$d" ]; then
		pkgs="$pkgs $p"
	fi
done

go test -short -coverprofile="$COVER_OUT" $pkgs
go tool cover -func="$COVER_OUT" | tail -1
sh scripts/check-coverage.sh "$COVER_OUT" "$COVER_BASELINE"
