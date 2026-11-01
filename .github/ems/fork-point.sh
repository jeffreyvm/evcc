#!/usr/bin/env bash
# Prints the upstream commit the fork's own changes start from: the newest
# upstream release tag in HEAD's history, or the merge base with upstream
# master when that is newer. ems sits on a release, which upstream tags on a
# release branch, so the merge base with master alone would count the bugfix
# commits of that release as changes of the fork.
set -euo pipefail

UPSTREAM_URL=${UPSTREAM_URL:-https://github.com/evcc-io/evcc.git}

git fetch --no-tags --quiet "$UPSTREAM_URL" master
mb=$(git merge-base HEAD FETCH_HEAD)

git fetch --quiet --tags "$UPSTREAM_URL"
if tag=$(git describe --tags --abbrev=0 --match '[0-9]*' --exclude '*-ems.*' HEAD 2> /dev/null); then
	t=$(git rev-parse "$tag^{commit}")
	if git merge-base --is-ancestor "$mb" "$t"; then
		echo "$t"
		exit 0
	fi
fi
echo "$mb"
