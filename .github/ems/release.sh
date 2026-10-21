#!/usr/bin/env bash
# Helpers of .github/workflows/ems-release.yml
#
#   release.sh mode            decide tag/version and whether to publish (GITHUB_OUTPUT)
#   release.sh notes <out>     release notes: EMS commits plus the upstream notes
#   release.sh check <dir>     verify and list the built files (annotations)
#
# Tags are <upstream>-ems.<n> without a leading "v", like upstream's tags: the
# about dialog and the update check use the tag as the version.

set -euo pipefail

UPSTREAM=evcc-io/evcc
UPSTREAM_URL=https://github.com/$UPSTREAM.git
TAG_RE='^[0-9]+\.[0-9]+\.[0-9]+-ems\.[0-9]+$'
RELEASE_BRANCH=${RELEASE_BRANCH:-ems}

out() { echo "$1=$2" >> "${GITHUB_OUTPUT:-/dev/stdout}"; }
fail() { echo "::error title=release::$*"; exit 1; }

# latest ems tag reachable from HEAD, optionally excluding one tag
previous_tag() {
	git tag -l '*-ems.*' --merged HEAD | grep -E "$TAG_RE" | grep -vxF "${1:-}" | sort -V | tail -n 1 || true
}

upstream_latest() {
	gh api "repos/$UPSTREAM/releases/latest" --jq .tag_name
}

cmd_mode() {
	local publish=false tag version base

	if [ "${REF_TYPE:-}" = tag ]; then
		tag=$REF_NAME
		if [[ $tag == v* ]]; then
			fail "tag $tag: use ${tag#v} without the v, the version shown in evcc is the tag"
		fi
		[[ $tag =~ $TAG_RE ]] || fail "tag $tag does not look like <upstream>-ems.<n>, e.g. 0.317.2-ems.1"

		git fetch --no-tags --quiet origin "$RELEASE_BRANCH"
		git merge-base --is-ancestor HEAD FETCH_HEAD ||
			fail "tag $tag is not on branch $RELEASE_BRANCH, releases are cut from $RELEASE_BRANCH"

		version=$tag
		if [ "$EVENT" = push ] || { [ "$EVENT" = workflow_dispatch ] && [ "${DRY_RUN:-true}" != true ]; }; then
			publish=true
		fi
	else
		# dry run: a pre-release of the newest upstream release
		base=$(upstream_latest)
		version="$base-ems.0.dryrun.${GITHUB_RUN_NUMBER:-0}"
		tag=$version
	fi

	echo "::notice title=release mode::tag $tag, version $version, publish $publish"
	out tag "$tag"
	out version "$version"
	out publish "$publish"
}

cmd_notes() {
	local file=$1 tag=${TAG:?} base prev range upstream_head
	base=${tag%%-ems.*}
	prev=$(previous_tag "$tag")

	git fetch --no-tags --quiet "$UPSTREAM_URL" master
	upstream_head=$(git rev-parse FETCH_HEAD)

	local basis="evcc $base"
	if git fetch --no-tags --quiet "$UPSTREAM_URL" "refs/tags/$base:refs/ems-upstream/$base" 2> /dev/null; then
		if git merge-base --is-ancestor "refs/ems-upstream/$base" HEAD; then
			local mb ahead
			mb=$(git merge-base HEAD "$upstream_head")
			ahead=$(git rev-list --count "refs/ems-upstream/$base..$mb")
			if [ "$ahead" -gt 0 ]; then
				basis="evcc $base plus $ahead upstream commits up to evcc-io/evcc@$(git rev-parse --short "$mb")"
			fi
		elif [ "${PUBLISH:-false}" = true ]; then
			fail "HEAD does not contain upstream $base: merge it into $RELEASE_BRANCH first or fix the tag"
		else
			echo "::warning title=release notes::HEAD does not contain upstream $base yet"
		fi
	else
		echo "::warning title=release notes::upstream has no tag $base"
	fi

	range=HEAD
	[ -n "$prev" ] && range="$prev..HEAD"

	{
		echo "## EMS-fork (jeffreyvm/evcc)"
		echo
		echo "Basis: $basis. Kwartierpiek en laadpaal voor het Vlaamse capaciteitstarief, zie docs/ems in de repo."
		echo
		if [ -n "$prev" ]; then
			echo "### EMS-wijzigingen sinds $prev"
		else
			echo "### EMS-wijzigingen"
		fi
		echo
		# commits of the fork only: upstream commits merged into ems are excluded
		local commits
		commits=$(git log --no-merges --reverse --format='- %s (%h)' "$range" "^$upstream_head")
		if [ -n "$commits" ]; then
			echo "$commits"
		else
			echo "- geen"
		fi
		echo
		echo "## Upstream evcc $base"
		echo
		if ! gh api "repos/$UPSTREAM/releases/tags/$base" --jq .body 2> /dev/null; then
			echo "Geen release notes van upstream gevonden: https://github.com/$UPSTREAM/releases"
		fi
	} > "$file"

	echo "::notice title=release notes::$(wc -l < "$file") lines, EMS commits in $range not in upstream master"
}

cmd_check() {
	local dir=$1 version=${VERSION:?} f n=0
	local expect=(
		"evcc_${version}_amd64.deb"
		"evcc_${version}_arm64.deb"
		"evcc_${version}_linux-amd64.tar.gz"
		"evcc_${version}_linux-arm64.tar.gz"
		checksums.txt
		ems-update.sh
	)
	for f in "${expect[@]}"; do
		[ -f "$dir/$f" ] || fail "missing $f"
	done

	(cd "$dir" && sha256sum --quiet -c checksums.txt) || fail "checksums.txt does not match"
	for f in "${expect[@]}"; do
		[ "$f" = checksums.txt ] && continue
		grep -qE "  $f\$" "$dir/checksums.txt" || fail "$f is not in checksums.txt"
	done

	if [ -f "$dir/checksums.txt.minisig" ]; then
		[ -n "${MINISIGN_PUB:-}" ] || fail "signature without public key"
		minisign -Vq -p "$MINISIGN_PUB" -m "$dir/checksums.txt" || fail "minisign signature does not verify"
		echo "::notice title=minisign::checksums.txt.minisig verifies with $(tail -n 1 "$MINISIGN_PUB")"
	fi

	for f in "$dir"/*.deb; do
		echo "::notice title=$(basename "$f")::$(stat -c %s "$f") bytes, package $(dpkg-deb -f "$f" Package) version $(dpkg-deb -f "$f" Version) arch $(dpkg-deb -f "$f" Architecture)"
		dpkg-deb -c "$f" | grep -qE '\./usr/bin/evcc$' || fail "$f has no /usr/bin/evcc"
		dpkg-deb -c "$f" | grep -qE '\./lib/systemd/system/evcc.service$' || fail "$f has no evcc.service"
		n=$((n + 1))
	done
	for f in "$dir"/*.tar.gz "$dir"/checksums.txt* "$dir"/ems-update.sh; do
		echo "::notice title=$(basename "$f")::$(stat -c %s "$f") bytes"
	done

	# the binary must report the release version
	local tmp
	tmp=$(mktemp -d)
	tar -xzf "$dir/evcc_${version}_linux-amd64.tar.gz" -C "$tmp" evcc
	local reported
	reported=$("$tmp/evcc" --version 2>&1 || true)
	rm -rf "$tmp"
	[[ $reported == *"$version"* ]] || fail "evcc --version reports '$reported', expected $version"
	echo "::notice title=evcc --version::$reported"

	[ "$n" -eq 2 ] || fail "expected 2 .deb files, found $n"
}

case "${1:-}" in
	mode) cmd_mode ;;
	notes) cmd_notes "${2:?output file}" ;;
	check) cmd_check "${2:?release dir}" ;;
	*)
		echo "usage: $0 mode|notes <file>|check <dir>" >&2
		exit 2
		;;
esac
