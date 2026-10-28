#!/usr/bin/env sh
# Pushes an annotated release tag for a commit that only the mirror has.
#
#   scripts/mirror-tag.sh vX.Y.Z REF REMOTE
#
# The private repository keeps its own tag of the same name on its own
# commit, so the public tag is built with git mktag and pushed by object id,
# never created as a local ref.
set -eu

if [ $# -ne 3 ]; then
	echo "usage: $0 vX.Y.Z REF REMOTE" >&2
	exit 2
fi
tag=$1
commit=$(git rev-parse --verify "$2^{commit}")
remote=$3

if git ls-remote --exit-code --tags "$remote" "refs/tags/$tag" >/dev/null 2>&1; then
	echo "mirror-tag: $remote already has $tag" >&2
	exit 1
fi

tagger="$(git config user.name) <$(git config user.email)> $(date +%s) $(date +%z)"
obj=$(printf 'object %s\ntype commit\ntag %s\ntagger %s\n\n%s\n' "$commit" "$tag" "$tagger" "$tag" | git mktag)
git push "$remote" "${obj}:refs/tags/${tag}"
echo "mirror-tag: $tag on $remote points at $commit"
