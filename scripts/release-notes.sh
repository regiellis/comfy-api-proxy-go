#!/usr/bin/env sh
# Prints one version's section of CHANGELOG.md, for the GitHub release body.
#
#   scripts/release-notes.sh v0.1.0
#
# Fails when the section is missing or empty, so a tag cannot ship without
# its changelog entry.
set -eu

if [ $# -ne 1 ]; then
	echo "usage: $0 vX.Y.Z" >&2
	exit 2
fi
version=${1#v}
root=$(cd "$(dirname "$0")/.." && pwd)

notes=$(awk -v v="$version" '
	$0 ~ "^## \\[" v "\\]" { found = 1; next }
	found && /^## \[/ { exit }
	found && /^\[[^]]+\]: / { exit }
	found { print }
' "$root/CHANGELOG.md")

if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
	echo "release-notes: no CHANGELOG.md section for $version" >&2
	exit 1
fi
printf '%s\n' "$notes"
