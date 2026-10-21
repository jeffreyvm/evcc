# Releases van de fork en bijwerken in de LXC

De fork `jeffreyvm/evcc` maakt zijn eigen releases. evcc in de LXC ziet die releases en toont "nieuwe versie beschikbaar". Er wordt nooit iets vanzelf geïnstalleerd: jij typt `update` in de LXC wanneer het jou past.

## Kort overzicht

| Wat | Waar | Wanneer |
|---|---|---|
| Release bouwen en publiceren | workflow **EMS release** (`.github/workflows/ems-release.yml`) | als jij een tag `…-ems.N` pusht |
| Upstream in de gaten houden | workflow **EMS upstream watch** (`.github/workflows/ems-upstream.yml`) | elke dag om 06:17 UTC |
| Melding in evcc | updatecheck in `server/updater` | elke 6 uur |
| Installeren in de LXC | `tools/ems-update.sh`, als commando `update` | als jij het start |

## Versies

Een release van de fork heet `<upstream-versie>-ems.<n>`, **zonder `v` ervoor** (zoals de tags van upstream):

- `0.317.2-ems.1`: de eerste release van de fork op basis van evcc 0.317.2
- `0.317.2-ems.2`: een tweede release op dezelfde basis, bijvoorbeeld na een fout in de EMS-code
- `0.317.3-ems.1`: de eerste release nadat upstream 0.317.3 in `ems` zit (de teller begint opnieuw bij 1)

Wat je waar ziet:

| Plaats | Voorbeeld |
|---|---|
| git-tag en GitHub-release | `0.317.2-ems.1` |
| evcc zelf (`evcc --version`, het venster "Over") | `0.317.2-ems.1`; de waarde komt uit de build: `-X github.com/evcc-io/evcc/util.Version=0.317.2-ems.1` |
| bestandsnaam van het pakket | `evcc_0.317.2-ems.1_amd64.deb` |
| versie van het Debian-pakket (`dpkg -l evcc`) | `0.317.2~ems.1` |

Het `~` is de Debian-schrijfwijze voor een voorversie: `0.317.2~ems.1` is voor apt **ouder** dan `0.317.2`. Daarom installeert `update` met `--allow-downgrades` (nodig bij de eerste overstap van 0.317.2 naar 0.317.2~ems.1) en moet de apt-bron van dl.evcc.io uit of vastgepind worden (zie verder), anders zet `apt upgrade` de gewone evcc terug.

De bestandsnaam gebruikt `-` en niet `~`, omdat GitHub een `~` in de naam van een bijlage zou veranderen en `checksums.txt` dan niet meer klopt.

## Een release maken

1. Zorg dat `ems` klaar is: upstream binnengehaald (zoals nu: `git merge upstream/master` in `ems`, jouw keuze) en de workflow **EMS tests** op `ems` groen.

   ```bash
   gh run list -R jeffreyvm/evcc --branch ems --workflow ems-test.yml --limit 1
   ```

2. Kies de versie. De basis is de nieuwste upstream-release die in `ems` zit. Zit `ems` op upstream-master ná 0.317.2 (zoals nu), dan blijft de basis 0.317.2 tot upstream 0.317.3 uitbrengt en jij die binnenhaalt. De release notes vermelden dan "evcc 0.317.2 plus N upstream commits".

3. Tag en push alleen die ene tag:

   ```bash
   git switch ems
   git pull
   git tag -a 0.317.2-ems.1 -m "evcc 0.317.2-ems.1"
   git push origin 0.317.2-ems.1
   ```

   Gebruik nooit `git push --tags` of `git push origin --tags`: dat duwt ook de tags van upstream (bijvoorbeeld `0.317.2`) naar de fork, en dan start de release-workflow van upstream op de fork. Die blijft hangen omdat hij runners van evcc-io vraagt.

4. Volg de workflow in GitHub, tab Actions, **EMS release**. Na ongeveer 15 minuten staat de release op <https://github.com/jeffreyvm/evcc/releases>.

Een tag weigert de workflow met een duidelijke fout als hij met `v` begint, niet de vorm `0.317.2-ems.1` heeft, niet op `ems` staat, of als `ems` de upstream-versie uit de tag niet bevat (bijvoorbeeld tag `0.317.3-ems.1` terwijl 0.317.3 nog niet binnengehaald is). Verkeerde tag weghalen: `git push origin :refs/tags/<tag>` en `git tag -d <tag>`.

## Wat de workflow doet

| Job | Wat |
|---|---|
| Mode | leest de tag en beslist: echte release of proefdraai |
| Tests | de volledige **EMS tests** op de getagde commit (alleen bij een echte release) |
| Build | webinterface en Go bouwen, goreleaser met `.goreleaser.ems.yml`, controle van alle bestanden |
| Publish | maakt de GitHub-release (geen pre-release, wel "Latest"); alleen als Tests en Build groen zijn |

De release bevat:

- `evcc_<versie>_amd64.deb` en `evcc_<versie>_arm64.deb`: hetzelfde pakket als upstream (naam `evcc`, service `evcc.service`, gebruiker `evcc`, data in `/var/lib/evcc`)
- `evcc_<versie>_linux-amd64.tar.gz` en `…_linux-arm64.tar.gz`: het programma zelf
- `ems-update.sh`: het updatescript voor de LXC
- `checksums.txt`: sha256 van al die bestanden
- `checksums.txt.minisig`: handtekening, alleen als de minisign-sleutel als secret bestaat

De release notes: eerst de EMS-commits sinds de vorige `…-ems.*`-tag (alleen commits van de fork, niet wat uit upstream kwam), daarna de release notes van upstream voor die versie.

Alleen de job Publish mag schrijven in de repo; de build zelf heeft een token dat alleen kan lezen. Docker, Homebrew, de APT-repo van evcc, de Home Assistant-add-on en de website van upstream worden niet aangeraakt.

### Proefdraai

Een proefdraai bouwt alles (als snapshot, versie bijvoorbeeld `0.317.2-ems.0.dryrun.12`), tekent met een wegwerpsleutel en controleert de bestanden, maar publiceert niets. Hij start:

- vanzelf bij een push naar `ems` of `ems-*` die de release-bestanden wijzigt (de workflow, `.goreleaser.ems.yml`, `.github/ems/release.sh` of `tools/ems-update.sh`);
- met de hand: Actions, **EMS release**, Run workflow, "dry-run" aangevinkt laten.

Het resultaat staat als artefact `evcc-<versie>` bij de run (7 dagen), met een overzicht in de samenvatting van de run.

## Melding in evcc

De updatecheck kijkt elke 6 uur naar de laatste release van `jeffreyvm/evcc` (in plaats van `evcc-io/evcc`) en meldt een nieuwe versie met de release notes. Het venster "Over" linkt naar de releases van de fork.

- `0.317.2-ems.2` is nieuwer dan `0.317.2-ems.1`, en `0.317.3-ems.1` nieuwer dan `0.317.2-ems.5`: melding.
- Releases van upstream ziet de check niet. Voor de versievergelijking (semver) is `0.317.2` nieuwer dan `0.317.2-ems.1`; daarom kijkt de check alleen naar de fork.
- Om dezelfde reden meldt de gewone evcc 0.317.2 de eerste fork-release niet: de eerste overstap doe je bewust met de hand.

## Eerste overstap in de LXC

Eenmalig, als root in de LXC (ongeveer 10 minuten).

1. **Snapshot** op de Proxmox-host (of in de webinterface: LXC, Snapshots, Take Snapshot):

   ```bash
   pct snapshot <ctid> voor-ems-fork
   ```

2. **Updatescript** ophalen en controleren:

   ```bash
   cd /root
   curl -fsSLO https://github.com/jeffreyvm/evcc/releases/latest/download/ems-update.sh
   curl -fsSLO https://github.com/jeffreyvm/evcc/releases/latest/download/checksums.txt
   sha256sum -c --ignore-missing checksums.txt     # moet "ems-update.sh: OK" geven
   install -m 0755 ems-update.sh /usr/local/sbin/ems-update
   mkdir -p /etc/evcc-ems
   ```

3. **apt-bron van upstream vastpinnen**, zodat `apt upgrade` de gewone evcc niet terugzet:

   ```bash
   cat > /etc/apt/preferences.d/evcc-ems <<'EOF'
   Package: evcc
   Pin: origin dl.evcc.io
   Pin-Priority: -1
   EOF
   apt-cache policy evcc     # dl.evcc.io moet nu op -1 staan
   ```

   Of de bron helemaal uitzetten: `grep -rl dl.evcc.io /etc/apt/sources.list.d/` en dat bestand hernoemen naar `….disabled`. De pin is handiger, want terug naar upstream is dan één bestand weghalen.

4. **Het commando `update` vervangen.** Nu is `/usr/bin/update` de wrapper van community-scripts: die zet de bron van dl.evcc.io opnieuw op en installeert de gewone evcc. Bewaar hem en zet er een doorverwijzing in de plaats:

   ```bash
   mv /usr/bin/update /usr/bin/update.community-scripts
   printf '#!/bin/sh\nexec /usr/local/sbin/ems-update "$@"\n' > /usr/bin/update
   chmod 755 /usr/bin/update
   ```

5. **Proef**, verandert niets: `update --dry-run`. Je ziet "Eerste overstap van upstream 0.317.2 naar de fork".

6. **Overstappen**: `update`, bevestigen met `j`.

Lukt het niet, dan zet het script zelf de vorige versie en de database terug. Gaat er toch iets mis: snapshot terugzetten met `pct rollback <ctid> voor-ems-fork`.

## Bijwerken (daarna, telkens)

evcc toont "nieuwe versie beschikbaar". In de LXC:

```bash
update             # vraagt bevestiging
update --dry-run   # alleen kijken: downloaden en controleren, niets veranderen
update --yes       # zonder vragen, bijvoorbeeld vanuit een script
update --version 0.317.2-ems.1   # een bepaalde release, ook om bewust terug te gaan
```

Wat `update` doet:

1. de laatste release van `jeffreyvm/evcc` opzoeken; staat die al, dan stopt het ("Al bijgewerkt, niets te doen");
2. `evcc_<versie>_amd64.deb` en `checksums.txt` downloaden, de sha256 controleren, en de handtekening als `/etc/evcc-ems/minisign.pub` bestaat;
3. een pakket van de huidige versie klaarzetten om naar terug te gaan (eigen kopie in `/var/lib/evcc-ems/packages`, de apt-cache of een download van GitHub); zonder terugvalpakket gaat de update niet door;
4. evcc stoppen en `/var/lib/evcc/evcc.db` kopiëren naar `/var/backups/evcc-ems/<datum>-<versie>/`;
5. `apt-get install --allow-downgrades ./evcc_<versie>_amd64.deb` en evcc herstarten;
6. tot 5 minuten wachten tot `http://localhost:7070/api/state` de nieuwe versie meldt zonder startfout;
7. lukt dat niet: vorige pakket en database terugzetten, evcc starten, ntfy-melding, en stoppen met foutcode 1.

De laatste 5 back-ups en pakketten blijven bewaard.

Instellingen (allemaal optioneel) in `/etc/evcc-ems/update.conf`, van root en alleen door root schrijfbaar (`chmod 600`):

```bash
NTFY_TOPIC=evcc-jeffrey-…        # melding na elke update of terugzetting
NTFY_SERVER=https://ntfy.sh      # standaard
NTFY_TOKEN=tk_…                  # alleen voor een afgeschermd topic
EVCC_URL=http://localhost:7070   # standaard
```

## Terug naar de gewone evcc

```bash
rm /etc/apt/preferences.d/evcc-ems           # of de bron weer aanzetten
apt update
apt-cache policy evcc                        # beschikbare versies van dl.evcc.io
apt install evcc=0.317.3                     # dezelfde basis of nieuwer dan de fork
mv /usr/bin/update.community-scripts /usr/bin/update
```

Neem een upstream-versie die minstens de basis van de fork is (bij `0.317.2-ems.3` dus 0.317.2 of nieuwer): een oudere evcc kan een database die al bijgewerkt is soms niet lezen. De instellingen van de fork blijven in de database staan; de gewone evcc negeert ze.

## Upstream volgen

De workflow **EMS upstream watch** kijkt elke dag of evcc-io/evcc een nieuwere release heeft dan de fork. Is dat zo, dan opent hij één issue "upstream 0.317.3 is uit, de fork volgt nog niet" (of werkt het bestaande bij) en stuurt hij een ntfy-melding. Het issue zegt wat er ontbreekt: upstream nog niet in `ems`, of wel in `ems` maar nog geen release. Zodra de release `0.317.3-ems.1` er is, sluit het issue vanzelf.

De workflow merget, rebaset of pusht nooit iets. Upstream binnenhalen in `ems` blijft jouw werk, op jouw manier (nu: merge van `upstream/master`).

## Wat jij nog moet instellen

Deze instellingen kan alleen jij zetten (GitHub, Settings van `jeffreyvm/evcc`):

1. **Standaardbranch op `ems`** (Settings, General, Default branch). GitHub start dagelijkse workflows alleen vanaf de standaardbranch; nu is dat `master`, en daar mag geen eigen code op. Gevolg van de wissel: ook de dagelijkse workflows van upstream (Nightly, Stale, templates) lopen dan vanaf `ems`; ze lopen nu ook al, en worden afgebroken omdat ze runners van evcc-io vragen. Wil je `master` als standaard houden, dan loopt de upstream watch alleen met de hand (Actions, EMS upstream watch, Run workflow).
2. **Issues aanzetten** (Settings, General, Features, Issues). Bij een fork staan ze standaard uit. Zonder issues stuurt de upstream watch alleen een ntfy-melding, en dan elke dag zolang de fork achterloopt.
3. **Secrets** (Settings, Secrets and variables, Actions), allemaal optioneel:

| Secret | Waarvoor | Zonder |
|---|---|---|
| `MINISIGN_SECRET_KEY` | inhoud van de geheime minisign-sleutel, tekent `checksums.txt` | release zonder handtekening (waarschuwing in de run) |
| `MINISIGN_PASSWORD` | wachtwoord van die sleutel | alleen voor een sleutel zonder wachtwoord |
| `NTFY_TOPIC` | ntfy-topic voor de upstream watch | geen melding, alleen het issue |
| `NTFY_SERVER` | eigen ntfy-server | `https://ntfy.sh` |
| `NTFY_TOKEN` | token voor een afgeschermd topic | geen |

### minisign-sleutel maken

Op je eigen pc (`apt install minisign`, of `brew install minisign`):

```bash
minisign -G -p evcc-ems.pub -s evcc-ems.key     # vraagt twee keer een wachtwoord
gh secret set MINISIGN_SECRET_KEY -R jeffreyvm/evcc < evcc-ems.key
gh secret set MINISIGN_PASSWORD -R jeffreyvm/evcc  # typ het wachtwoord
```

Bewaar `evcc-ems.key` en het wachtwoord buiten GitHub (bijvoorbeeld in je wachtwoordbeheer). In de LXC:

```bash
apt install minisign
cat > /etc/evcc-ems/minisign.pub     # plak de inhoud van evcc-ems.pub, Ctrl-D
```

Vanaf dan weigert `update` elke release zonder geldige handtekening.

Wil je de sleutel helemaal niet in GitHub: zet geen secret, en teken na elke release zelf op je pc:

```bash
gh release download 0.317.2-ems.1 -R jeffreyvm/evcc -p checksums.txt
minisign -S -s evcc-ems.key -m checksums.txt
gh release upload 0.317.2-ems.1 -R jeffreyvm/evcc checksums.txt.minisig
```

Tot die handtekening er is, weigert `update` in een LXC met `minisign.pub` die release.

### ntfy

Kies een topic dat niemand raadt, bijvoorbeeld `evcc-fork-` gevolgd door 12 willekeurige tekens, en abonneer je erop in de ntfy-app. Dan `gh secret set NTFY_TOPIC -R jeffreyvm/evcc`. Hetzelfde topic mag in `/etc/evcc-ems/update.conf`.

## Workflows van upstream op de fork

- **Release** (upstream) start alleen op tags als `0.317.2`; de tags van de fork (`0.317.2-ems.1`) vallen daarbuiten.
- **Docs, website, Home Assistant-add-on** starten bij het maken of wijzigen van een release. Releases die de workflow maakt starten geen andere workflows (zo werkt het token van GitHub Actions). Wijzig je een release met de hand, dan zou de add-on-changelog starten; die is daarom beperkt tot `evcc-io/evcc`. De andere vragen runners van evcc-io en blijven hooguit in de wachtrij hangen tot GitHub ze afbreekt.
