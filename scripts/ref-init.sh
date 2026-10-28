#!/usr/bin/env sh
# Checks out one of Comfy-Org's Python references at its pinned commit.
#
#   scripts/ref-init.sh DIR URL COMMIT
#
# With a .gitmodules entry for DIR it is a submodule update. Without one (a
# clone that does not carry the submodules) it clones URL into DIR and checks
# out COMMIT, the same commit the submodule pins.
set -eu

if [ $# -ne 3 ]; then
	echo "usage: $0 DIR URL COMMIT" >&2
	exit 2
fi
dir=$1
url=$2
commit=$3

if [ -f .gitmodules ] && git config -f .gitmodules --get "submodule.$dir.path" >/dev/null 2>&1; then
	git submodule update --init "$dir"
	exit 0
fi
if [ ! -d "$dir/.git" ] && [ ! -f "$dir/.git" ]; then
	git clone --quiet "$url" "$dir"
fi
git -C "$dir" fetch --quiet origin "$commit"
git -C "$dir" -c advice.detachedHead=false checkout --quiet "$commit"
echo "ref-init: $dir at $commit"
