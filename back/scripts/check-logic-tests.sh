#!/bin/sh
# §1.1a: every package in the "Logic" zone must ship at least one _test.go.
# golangci-lint does not model test-presence, so this is a small shell check.
# It lists the .go (non-test) files per directory and fails if a directory has
# source but no test. Generated/gen dirs are out of scope.
#
# Usage: check-logic-tests.sh "<space-separated package globs>"
set -eu

LOGIC_PKGS="$1"
status=0

for pkg in $LOGIC_PKGS; do
	# Strip the leading ./ and trailing /... to get the directory prefix.
	dir=$(echo "$pkg" | sed 's#^\./##; s#/\.\.\.$##')
	[ -d "$dir" ] || continue

	# Find immediate subdirectories under the prefix that contain .go files.
	find "$dir" -type f -name '*.go' ! -name '*_test.go' ! -path '*/gen/*' \
		-exec dirname {} \; | sort -u | while read -r d; do
		has_src=$(ls "$d"/*.go 2>/dev/null | grep -v '_test\.go$' | head -1)
		has_test=$(ls "$d"/*_test.go 2>/dev/null | head -1)
		if [ -n "$has_src" ] && [ -z "$has_test" ]; then
			echo "check-logic-tests: $d has source but no _test.go (§0.1, §1.1a)"
			status=1
		fi
	done
done

exit $status
