#!/usr/bin/env bash
# ems-update.sh: install a release of the evcc EMS fork (jeffreyvm/evcc) in a
# Debian LXC, with checks, backup and automatic rollback. Runs as root.
#
#   ems-update.sh [--dry-run] [--version <v>] [--yes]
#
#   --dry-run       download and verify, show what would happen, change nothing
#   --version <v>   install this release (e.g. 0.317.2-ems.1) instead of the latest
#   --yes           do not ask for confirmation
#
# Steps: find the release, download evcc_<v>_<arch>.deb and checksums.txt, verify
# sha256 (and minisign when /etc/evcc-ems/minisign.pub exists), stop evcc, back up
# /var/lib/evcc/evcc.db, apt install the package (--allow-downgrades), start evcc
# and wait up to 5 minutes until http://localhost:7070/api/state reports the new
# version. Otherwise the previous package and database are restored and the script
# exits with 1. Nothing to do when the release is already installed.
#
# Optional settings in /etc/evcc-ems/update.conf (shell syntax) or the environment:
#   NTFY_TOPIC=...       ntfy topic for a message after each update or rollback
#   NTFY_SERVER=...      default https://ntfy.sh
#   NTFY_TOKEN=...       ntfy access token
#   EVCC_URL=...         default http://localhost:7070
#   GITHUB_TOKEN=...     only against GitHub rate limits

set -euo pipefail

REPO=${EMS_REPO:-jeffreyvm/evcc}
UPSTREAM=evcc-io/evcc
CONF=/etc/evcc-ems/update.conf
PUBKEY=/etc/evcc-ems/minisign.pub
DB=/var/lib/evcc/evcc.db
STATE_DIR=/var/lib/evcc-ems
BACKUP_DIR=/var/backups/evcc-ems
HEALTH_TIMEOUT=${HEALTH_TIMEOUT:-300}
KEEP=5
TAG_RE='^[0-9]+\.[0-9]+\.[0-9]+-ems\.[0-9]+$'

dry_run=false
assume_yes=false
want=""

say() { printf '%s\n' "$*"; }
warn() { printf 'LET OP: %s\n' "$*" >&2; }
die() {
	printf 'FOUT: %s\n' "$*" >&2
	exit 1
}

usage() {
	sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

while [ $# -gt 0 ]; do
	case "$1" in
		--dry-run) dry_run=true ;;
		--yes | -y) assume_yes=true ;;
		--version)
			[ $# -ge 2 ] || die "--version vraagt een versie, bijvoorbeeld 0.317.2-ems.1"
			want=$2
			shift
			;;
		--version=*) want=${1#--version=} ;;
		-h | --help) usage 0 ;;
		*)
			warn "onbekende optie: $1"
			usage 2
			;;
	esac
	shift
done

# settings: only from a root-owned file that nobody else can change
if [ -f "$CONF" ]; then
	if [ "$(stat -c %u "$CONF")" = 0 ] && [ $(($(stat -c %#a "$CONF") & 8#022)) -eq 0 ]; then
		# shellcheck source=/dev/null
		. "$CONF"
	else
		warn "$CONF genegeerd: moet van root zijn en mag niet door anderen schrijfbaar zijn"
	fi
fi
EVCC_URL=${EVCC_URL:-http://localhost:7070}

notify() {
	[ -n "${NTFY_TOPIC:-}" ] || return 0
	local args=(-fsS -m 15 -o /dev/null -H "Title: evcc update" -d "$1")
	[ -n "${2:-}" ] && args+=(-H "Tags: $2")
	[ -n "${NTFY_TOKEN:-}" ] && args+=(-H "Authorization: Bearer $NTFY_TOKEN")
	curl "${args[@]}" "${NTFY_SERVER:-https://ntfy.sh}/$NTFY_TOPIC" || warn "ntfy-melding niet verstuurd"
}

github() {
	local auth=()
	[ -n "${GITHUB_TOKEN:-}" ] && auth=(-H "Authorization: Bearer $GITHUB_TOKEN")
	curl -fsSL --retry 3 "${auth[@]}" "$@"
}

for c in curl sha256sum dpkg dpkg-query apt-get systemctl flock; do
	command -v "$c" > /dev/null || die "$c ontbreekt"
done

if [ "$dry_run" = false ]; then
	[ "$(id -u)" = 0 ] || die "start als root (of met --dry-run om enkel te kijken)"
	exec 9> /run/evcc-ems-update.lock
	flock -n 9 || die "er loopt al een update"
fi

arch=$(dpkg --print-architecture)
case "$arch" in
	amd64 | arm64) ;;
	*) die "architectuur $arch wordt niet gebouwd (enkel amd64 en arm64)" ;;
esac

# release tag: given, or the latest release (the redirect of /releases/latest)
if [ -n "$want" ]; then
	tag=${want#v}
else
	url=$(github -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") ||
		die "kan de laatste release van $REPO niet ophalen"
	tag=${url##*/}
	[ "$tag" != releases ] || die "$REPO heeft nog geen release"
fi
[[ $tag =~ $TAG_RE ]] || die "'$tag' is geen release van de fork (<upstream>-ems.<n>, bijvoorbeeld 0.317.2-ems.1)"

# Debian writes the semver pre-release with "~": 0.317.2-ems.1 is 0.317.2~ems.1
target=${tag/-/\~}
installed=$(dpkg-query -W -f='${Status} ${Version}' evcc 2> /dev/null | awk '$3 == "installed" { print $4 }' || true)

say "evcc geïnstalleerd: ${installed:-geen}"
say "release van $REPO: $tag (pakketversie $target, $arch)"

if [ "$installed" = "$target" ]; then
	say "Al bijgewerkt, niets te doen."
	exit 0
fi

if [ -n "$installed" ] && dpkg --compare-versions "$installed" gt "$target"; then
	base=${target%%~*}
	inst_base=${installed%%[~+-]*}
	if [[ $installed != *"~ems."* ]] && [ "$inst_base" = "$base" ]; then
		say "Eerste overstap van upstream $installed naar de fork: technisch een downgrade (~ems is ouder dan $base)."
	elif [ -z "$want" ]; then
		say "De geïnstalleerde versie $installed is nieuwer dan de laatste release $tag. Niets te doen."
		say "Bewust teruggaan: --version <versie>."
		exit 0
	else
		warn "$tag is ouder dan de geïnstalleerde $installed."
		dpkg --compare-versions "$inst_base" gt "$base" &&
			warn "dat gaat terug naar een oudere upstream-versie ($base < $inst_base): de database kan al nieuwer zijn. Neem eerst een Proxmox-snapshot."
	fi
fi

if grep -rqs 'dl\.evcc\.io' /etc/apt/sources.list /etc/apt/sources.list.d/ &&
	! grep -rqs 'dl\.evcc\.io' /etc/apt/preferences.d/; then
	warn "de apt-bron dl.evcc.io staat aan zonder pin: apt upgrade zet dan de gewone evcc terug. Zie docs/ems/release.md."
fi

tmp=$(mktemp -d)
stopped=false
finished=false
cleanup() {
	rm -rf "$tmp"
	# never leave evcc stopped after an unexpected error
	if [ "$stopped" = true ] && [ "$finished" = false ]; then
		systemctl start evcc || true
	fi
}
trap cleanup EXIT
chmod 755 "$tmp" # apt reads local packages as user _apt

deb="evcc_${tag}_${arch}.deb"
dl="https://github.com/$REPO/releases/download/$tag"
say "Downloaden van $deb ..."
github -o "$tmp/$deb" "$dl/$deb" || die "download van $deb mislukt"
github -o "$tmp/checksums.txt" "$dl/checksums.txt" || die "download van checksums.txt mislukt"

if [ -f "$PUBKEY" ]; then
	command -v minisign > /dev/null || die "$PUBKEY bestaat maar minisign ontbreekt: apt install minisign"
	github -o "$tmp/checksums.txt.minisig" "$dl/checksums.txt.minisig" ||
		die "release $tag heeft geen handtekening (checksums.txt.minisig), terwijl $PUBKEY bestaat"
	minisign -Vq -p "$PUBKEY" -m "$tmp/checksums.txt" -x "$tmp/checksums.txt.minisig" ||
		die "handtekening van checksums.txt klopt NIET"
	say "Handtekening OK."
else
	say "Geen handtekeningcontrole ($PUBKEY bestaat niet)."
fi

line=$(awk -v f="$deb" '$2 == f || $2 == "*" f' "$tmp/checksums.txt")
[ -n "$line" ] || die "$deb staat niet in checksums.txt"
(cd "$tmp" && printf '%s\n' "$line" | sha256sum -c --quiet -) || die "sha256 van $deb klopt NIET"
say "Checksum OK."

[ "$(dpkg-deb -f "$tmp/$deb" Package)" = evcc ] || die "$deb is geen evcc-pakket"
[ "$(dpkg-deb -f "$tmp/$deb" Version)" = "$target" ] || die "$deb heeft versie $(dpkg-deb -f "$tmp/$deb" Version), verwacht $target"

# the package to go back to: kept by an earlier run, apt's cache, or a download
previous=""
if [ -n "$installed" ]; then
	prev_tag=${installed/\~/-}
	for f in "$STATE_DIR/packages/evcc_${prev_tag}_${arch}.deb" \
		"/var/cache/apt/archives/evcc_${installed//:/%3a}_${arch}.deb"; do
		if [ -f "$f" ] && [ "$(dpkg-deb -f "$f" Version 2> /dev/null)" = "$installed" ]; then
			previous=$f
			break
		fi
	done
	if [ -z "$previous" ]; then
		if [[ $installed == *"~ems."* ]]; then
			from="https://github.com/$REPO/releases/download/$prev_tag/evcc_${prev_tag}_${arch}.deb"
		else
			from="https://github.com/$UPSTREAM/releases/download/$installed/evcc_${installed}_${arch}.deb"
		fi
		if github -o "$tmp/previous.deb" "$from" && [ "$(dpkg-deb -f "$tmp/previous.deb" Version 2> /dev/null)" = "$installed" ]; then
			previous=$tmp/previous.deb
		fi
	fi
	[ -n "$previous" ] || die "geen pakket van de huidige versie $installed gevonden om naar terug te gaan; update afgebroken"
	say "Terugvalpakket: $previous"
fi

if [ "$dry_run" = true ]; then
	say "Proef (--dry-run): zou $installed vervangen door $target, met back-up van $DB. Niets veranderd."
	exit 0
fi

if [ "$assume_yes" = false ]; then
	[ -t 0 ] || die "geen terminal: bevestig met --yes"
	read -r -p "evcc ${installed:-} -> $target installeren? [j/N] " answer
	case "$answer" in
		j* | J* | y* | Y*) ;;
		*) die "afgebroken" ;;
	esac
fi

# backup
stamp=$(date +%Y%m%d-%H%M%S)
backup="$BACKUP_DIR/$stamp-${installed:-none}"
mkdir -p "$backup" "$STATE_DIR/packages"
chmod 700 "$BACKUP_DIR"

was_active=false
systemctl is-active --quiet evcc && was_active=true
say "evcc stoppen voor een consistente back-up ..."
stopped=true
systemctl stop evcc || true

for f in "$DB" "$DB-wal" "$DB-shm"; do
	[ -f "$f" ] && cp -a "$f" "$backup/"
done
[ -n "$previous" ] && cp "$previous" "$backup/"
say "Back-up: $backup"

install_pkg() {
	local args=(-y -q --allow-downgrades -o Dpkg::Options::=--force-confold)
	if [ "$assume_yes" = true ]; then
		DEBIAN_FRONTEND=noninteractive apt-get install "${args[@]}" "$1" < /dev/null
	else
		apt-get install "${args[@]}" "$1"
	fi
}

healthy() {
	local want_version=$1 deadline=$((SECONDS + HEALTH_TIMEOUT)) v
	while [ $SECONDS -lt $deadline ]; do
		if systemctl is-active --quiet evcc &&
			v=$(curl -fsS -m 5 "$EVCC_URL/api/state?jq=.version" 2> /dev/null) &&
			[ "$v" = "\"$want_version\"" ]; then
			# a configuration error still answers, but reports fatal
			if [ "$(curl -fsS -m 5 "$EVCC_URL/api/state?jq=.fatal" 2> /dev/null)" = null ]; then
				return 0
			fi
		fi
		sleep 5
	done
	return 1
}

rollback() {
	warn "$1: terug naar ${installed:-de vorige toestand}"
	systemctl stop evcc || true
	if [ -n "$previous" ]; then
		install_pkg "$backup/$(basename "$previous")" || warn "terugzetten van het pakket mislukt"
	fi
	for f in evcc.db evcc.db-wal evcc.db-shm; do
		rm -f "/var/lib/evcc/$f"
		[ -f "$backup/$f" ] && cp -a "$backup/$f" /var/lib/evcc/
	done
	systemctl start evcc || true
	local ok="evcc antwoordt niet na het terugzetten, kijk het na: journalctl -u evcc"
	if [ -n "$installed" ] && healthy "${installed/\~/-}"; then
		ok="evcc ${installed/\~/-} draait weer"
	fi
	warn "$ok"
	notify "Update naar $tag mislukt ($1), teruggezet. $ok" warning
	finished=true
	exit 1
}

say "Installeren van $deb ..."
if ! install_pkg "$tmp/$deb"; then
	rollback "apt install mislukt"
fi

systemctl daemon-reload || true
systemctl enable --quiet evcc || true
systemctl restart evcc || true
finished=true # from here on a failure is handled by rollback
say "Wachten (tot $((HEALTH_TIMEOUT / 60)) min) tot $EVCC_URL/api/state versie $tag meldt ..."
if ! healthy "$tag"; then
	rollback "evcc $tag antwoordt niet correct binnen $((HEALTH_TIMEOUT / 60)) minuten"
fi
[ "$was_active" = true ] || warn "evcc draaide niet voor de update, nu wel"

# keep the installed package for a later rollback, prune old copies and backups
cp "$tmp/$deb" "$STATE_DIR/packages/"
find "$STATE_DIR/packages" -name 'evcc_*.deb' -printf '%T@ %p\n' | sort -rn | tail -n +$((KEEP + 1)) | cut -d' ' -f2- | xargs -r rm -f
find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -type d -printf '%T@ %p\n' | sort -rn | tail -n +$((KEEP + 1)) | cut -d' ' -f2- | xargs -r rm -rf

say "Klaar: evcc $tag draait. Back-up blijft in $backup."
notify "evcc bijgewerkt naar $tag" white_check_mark
