# shellcheck shell=bash
# Shared helpers of release.sh and upstream-watch.sh

# missing_upstream <upstream tag ref> <head>
#
# Prints the commits of an upstream release that <head> lacks, one per line
# (short sha and subject), nothing when <head> contains the release. Bugfix
# releases are cut from release branches, so a commit counts as present when it
# is an ancestor, a patch-equivalent commit exists (git cherry), or a commit with
# the same subject exists: backports keep the subject with the PR number.
missing_upstream() {
	local tag=$1 head=$2 base subjects sign sha subject
	git merge-base --is-ancestor "$tag" "$head" && return 0

	base=$(git merge-base "$tag" "$head")
	subjects=$(git log --format=%s "$base..$head")

	git cherry "$head" "$tag" | while read -r sign sha; do
		[ "$sign" = + ] || continue
		subject=$(git log -1 --format=%s "$sha")
		grep -Fxq -- "$subject" <<< "$subjects" || echo "$(git rev-parse --short "$sha") $subject"
	done
}
