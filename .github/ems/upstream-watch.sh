#!/usr/bin/env bash
# Upstream watch of .github/workflows/ems-upstream.yml
#
# Compares the newest upstream release of evcc-io/evcc with the fork: does branch
# ems contain it, and is there a release <upstream>-ems.<n> for it? If not, one
# issue "upstream <v> is uit, de fork volgt nog niet" is opened or updated and an
# ntfy message is sent (NTFY_TOPIC, optional NTFY_SERVER and NTFY_TOKEN). Nothing
# is merged, rebased or pushed. REPORT_ONLY=true only reports (annotations).

set -euo pipefail

UPSTREAM=evcc-io/evcc
REPO=${GITHUB_REPOSITORY:-jeffreyvm/evcc}
BRANCH=${EMS_BRANCH:-ems}
SUFFIX="de fork volgt nog niet"
TAG_RE='^[0-9]+\.[0-9]+\.[0-9]+-ems\.[0-9]+$'

notify() {
	[ -n "${NTFY_TOPIC:-}" ] || return 0
	local args=(-fsS -m 15 -o /dev/null -H "Title: evcc-fork" -H "Tags: warning" -d "$1")
	[ -n "${2:-}" ] && args+=(-H "Click: $2")
	[ -n "${NTFY_TOKEN:-}" ] && args+=(-H "Authorization: Bearer $NTFY_TOKEN")
	curl "${args[@]}" "${NTFY_SERVER:-https://ntfy.sh}/$NTFY_TOPIC" ||
		echo "::warning title=ntfy::sending the notification failed"
}

up=$(gh api "repos/$UPSTREAM/releases/latest" --jq .tag_name)

latest=$(git tag -l '*-ems.*' | grep -E "$TAG_RE" | sort -V | tail -n 1 || true)
base=${latest%%-ems.*}

git fetch --no-tags --quiet origin "$BRANCH"
ems=$(git rev-parse FETCH_HEAD)
git fetch --no-tags --quiet "https://github.com/$UPSTREAM.git" "refs/tags/$up"
contains=false
git merge-base --is-ancestor FETCH_HEAD "$ems" && contains=true

released=false
if [ -n "$base" ] && dpkg --compare-versions "$base" ge "$up"; then
	released=true
fi

echo "::notice title=upstream watch::upstream $up, fork release ${latest:-none}, $BRANCH contains $up: $contains"

title="upstream $up is uit, $SUFFIX"
if [ "$contains" = false ]; then
	status="Branch \`$BRANCH\` bevat upstream $up nog niet. Haal upstream binnen in \`$BRANCH\` (zelf, met de hand), laat de EMS-tests groen worden en maak dan de release \`$up-ems.1\`."
elif [ "$released" = false ]; then
	status="Branch \`$BRANCH\` bevat upstream $up al, maar er is nog geen release \`$up-ems.<n>\` (laatste: ${latest:-geen}). Maak de release volgens docs/ems/release.md."
else
	status=""
fi

if [ "${REPORT_ONLY:-false}" = true ]; then
	echo "::notice title=upstream watch::report only, no issue or notification${status:+: $status}"
	exit 0
fi

if [ "$(gh api "repos/$REPO" --jq .has_issues)" != true ]; then
	if [ -n "$status" ]; then
		echo "::warning title=upstream watch::issues are disabled in $REPO, only ntfy: $title"
		notify "$title. $status"
	fi
	exit 0
fi

# the one open watch issue, if any
issue=$(gh issue list -R "$REPO" --state open --limit 200 --json number,title \
	--jq "[.[] | select(.title | endswith(\"$SUFFIX\"))][0] | select(. != null) | \"\(.number) \(.title)\"")
number=${issue%% *}
current=${issue#* }

if [ -z "$status" ]; then
	if [ -n "$issue" ]; then
		gh issue close "$number" -R "$REPO" --comment "De fork volgt weer: release $latest bevat upstream $up."
	fi
	exit 0
fi

body="Upstream [evcc $up](https://github.com/$UPSTREAM/releases/tag/$up) is uit.

$status

- laatste release van de fork: ${latest:-geen}
- \`$BRANCH\` staat op $ems

Dit issue wordt elke dag bijgewerkt door de workflow EMS upstream watch en sluit vanzelf zodra er een release \`$up-ems.<n>\` is."

if [ -z "$issue" ]; then
	url=$(gh issue create -R "$REPO" --title "$title" --body "$body")
	notify "$title" "$url"
elif [ "$current" != "$title" ]; then
	gh issue edit "$number" -R "$REPO" --title "$title" --body "$body" > /dev/null
	notify "$title" "https://github.com/$REPO/issues/$number"
elif [ "$(gh issue view "$number" -R "$REPO" --json body --jq .body)" != "$body" ]; then
	gh issue edit "$number" -R "$REPO" --body "$body" > /dev/null
fi
