#!/bin/sh
# cover gate (§1.1a): fail if total coverage dropped below the saved baseline.
# The baseline file holds a single float (e.g. "42.3"). If it is absent we
# initialize it — the first `make cover` writes the floor, later runs guard it.
set -eu
COVER_OUT="$1"
BASELINE="$2"

# Extract total coverage percentage, e.g. "total:	(stmts)	68.4%".
total=$(go tool cover -func="$COVER_OUT" | awk '/^total:/ {gsub("%","",$3); print $3}')
[ -n "$total" ] || {
	echo "check-coverage: no total in $COVER_OUT"
	exit 1
}

if [ ! -f "$BASELINE" ]; then
	echo "$total" >"$BASELINE"
	echo "check-coverage: initialized baseline to ${total}% (no prior floor)"
	exit 0
fi

floor=$(cat "$BASELINE")
# awk for float compare.
if awk -v t="$total" -v f="$floor" 'BEGIN { exit (t+0 >= f+0) ? 0 : 1 }'; then
	echo "check-coverage: ${total}% >= baseline ${floor}%"
	# Ratchet the baseline up if coverage improved.
	if awk -v t="$total" -v f="$floor" 'BEGIN { exit (t+0 > f+0) ? 0 : 1 }'; then
		echo "$total" >"$BASELINE"
	fi
	exit 0
else
	echo "check-coverage: ${total}% < baseline ${floor}% — coverage regressed" >&2
	exit 1
fi
