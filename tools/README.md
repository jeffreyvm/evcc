# F0: meten vóór we sturen

Drie scripts, alleen Python 3.11-standaardbibliotheek (geen pip). Ze draaien in de evcc-LXC.

| Script | Wat | Wanneer |
|---|---|---|
| `f0_logger.py` | elke seconde P1, Alfen en beide Indevolts in een CSV per dag | **vandaag** installeren; moet lopen op 25 oktober (wintertijd) en 1 november (maandgrens) |
| `f0_businesscase.py` | haalbare maandpiek en €/jaar uit Fluvius-kwartieren en laadsessies | één avond, zodra je de Fluvius-CSV hebt |
| `f0_indevolt_test.py` | hold, charge, ontladen op vast vermogen, "stil" in real-time control, latentie, staptest | één zonnige middag, evcc gestopt |

## 1. Logscript installeren (10 minuten)

```bash
# in de evcc-LXC, als root
mkdir -p /opt/f0 /etc/f0 /var/log/f0
cp f0_common.py f0_logger.py /opt/f0/
cp f0_logger.example.json /etc/f0/logger.json
nano /etc/f0/logger.json        # IP's van P1, Alfen en beide Indevolts; HA-token mag leeg blijven
python3 /opt/f0/f0_logger.py -c /etc/f0/logger.json --once     # verbindingstest: alles moet zonder error zijn
cp f0-logger.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now f0-logger
journalctl -u f0-logger -f      # "log naar /var/log/f0/f0-2026-10-18.csv" en daarna stilte = goed
```

Wat je in de eerste minuten controleert:

- `p1_avg15_w` en `p1_monthpeak_w` gevuld? Zo niet: de HomeWizard-firmware geeft ze niet in API v1; dan rekenen we zelf (M1 uit de review vervalt deels).
- `alfen_valid_s` en `alfen_safe_a`: dit zijn de registers 1208 en 1212. Als ze onzin geven, zet `"alfen_extended": false` (de registernummers komen uit het Alfen Modbus-document, niet uit evcc; ik heb ze niet op jouw paal kunnen testen).
- `garage_mode` / `zolder_mode`: staat er een 1 of 4, dan is register 47005 terugleesbaar en kan de waakhond in de fork op de modus zelf kijken. Het journal meldt dit bij de start.
- `*_ms`: leestijden. Blijven P1 en Indevolts onder ±200 ms en de Alfen onder ±100 ms, dan is een regelinterval van 10 s ruim haalbaar (open vraag in het ontwerp).
- Het teken van `garage_p_w`: 's avonds bij netafname moet het **negatief** zijn (ontladen). Positief bij afname = teken omgekeerd op de PV-variant.

Bestanden: ±17 MB per dag onbewerkt, de vorige dag wordt automatisch gegzipt (±2 MB). Een maand kost dus ±60 MB.

De logger leest de Alfen standaard via evcc (`/api/state`, instelling `"alfen_source": "evcc"`) en opent dus geen eigen Modbus-verbinding naar de Alfen. Reden: op 25 oktober bleek dat elke Modbus-aanvraag aan de Alfen minstens 125 ms duurt en dat ±15% faalt, waardoor 8 aanvragen per seconde nooit binnen 0,9 s klaar zijn (elke rij werd `alfen:late`). Gevolg: de Alfen-kolommen veranderen alleen als evcc ze ververst (standaard elke 10 s), en `alfen_applied_a`, `alfen_valid_s` en `alfen_safe_a` blijven leeg. Terug naar eigen Modbus-lezing kan met `"alfen_source": "modbus"` (dan gelden de oude opmerkingen: een beperkt aantal gelijktijdige verbindingen, `"interval_s": 2.0` of `"alfen_extended": false`). Heeft evcc een wachtwoord, dan geeft `/api/state` 401 en verschijnt `alfen:HTTPError` in de kolom `errors`.

## 2. Businesscase (één avond)

Invoer:

1. **Fluvius-kwartieren**: Mijn Fluvius → Verbruik → kwartierwaarden → CSV, voor de laatste 12 à 13 volle maanden. Het script herkent de kolommen `Van Datum`, `Van Tijdstip`, `Register`, `Volume` en telt `Afname Dag` + `Afname Nacht` (bruto afname, zoals het capaciteitstarief rekent).
2. **Laadpaalenergie**, één van beide:
   - de evcc-database: `cp /var/lib/evcc/evcc.db /tmp/evcc.db` en dan `--ev-sessions /tmp/evcc.db` (laadsessies, energie gelijk gespreid over de sessie; 's nachts is dat exact, overdag bij zonneladen een benadering);
   - of een CSV uit HA met een tijdkolom en een waarde (vermogen in W, of een kWh-teller): `--ev alfen.csv --ev-kind power_w|energy_kwh`. Uurwaarden uit de HA-energie-export volstaan voor een businesscase.

```bash
python3 f0_businesscase.py --fluvius kwartieren.csv --ev-sessions /tmp/evcc.db --out ./businesscase
```

Uitvoer in `businesscase/businesscase.md`:

- maandpieken per scenario: **A** nu, **B** EV begrensd (huis alleen), **C** B + 2 kW batterij, **D** B + alleen de zolder (1,2 kW), met het tijdstip van elke piek en het EV-aandeel in de huidige piek;
- jaarkost en besparing per scenario (gemiddelde van 12 maandpieken × €60,53);
- de 15 grootste resterende kwartieren van het huis alleen — daar kan geen EMS iets aan doen, alleen de batterij of uitstel van de vaatwasser;
- per uur hoe vaak het huis alleen boven 2,5 kW zit, weekdag en weekend apart: dat is de controle op het kookvenster (17–19 u, weekend 10:30–12 u) uit het ontwerp;
- de grootste dagenergie boven de vloer: moet onder de winterreserve van 3,5 kWh blijven.

C en D zijn bovengrenzen: de batterij begint elke dag met de seizoensreserve (3,5 / 2 / 1,5 kWh, instelbaar met `--reserve`) en reageert zonder vertraging. De review schat dat de werkelijkheid enkele euro's lager ligt (H1).

## 3. Indevolt-testscript (één middag, ±2 uur)

Voorwaarden, in deze volgorde:

1. Zonnig, bij voorkeur 12–15 u, batterijen niet vol (anders kan charge niet getest worden) en niet leeg (minstens 25 %).
2. Auto niet aan het laden.
3. In Home Assistant de batterij-automations van capaciteitstarief v8 **uit** (anders stuurt HA er modi doorheen).
4. `systemctl stop evcc` (het script weigert anders te starten).
5. Het logscript mag blijven draaien.

```bash
python3 f0_indevolt_test.py -c /etc/f0/logger.json --minsoc 10 --maxsoc 100 --idle-min 60
```

`--minsoc`/`--maxsoc`: dezelfde waarden als in evcc (batterijinstellingen). Per unit: 3 min referentie, 15 min hold, 15 min charge, 2 × 3 min ontladen-kandidaat, 60 min stil, 3 min terug — ±1 u 40 per unit; `--idle-min 20` maakt het 1 uur. Daarna de staptest met beide units (5 min): het script piept en vraagt je een last van ±2 kW aan te zetten (waterkoker, oven) en 90 s later weer uit.

Veiligheid: bij Ctrl-C, een fout of het einde zet het script beide units terug op normal (drie pogingen; lukt het niet, dan zegt het luid wat je met de hand moet doen). Tijdens charge stopt het als het net 30 s boven 3,5 kW zit. Het ontladen-op-vast-vermogen (status 2 en 3 in 47015) is een gok: Indevolt documenteert het niet. Het script stopt die test zodra het bewijs er is (ontladen of injectie gezien) of na 3 minuten.

Uitvoer: `/var/log/f0/RAPPORT.md` met per unit de antwoorden op V1–V9 (zie de kop van het script), plus de ruwe CSV en een events-bestand met elke SetData-call, het antwoord en de HTTP-latentie.

Daarna: `systemctl start evcc`, v8-automations weer aan, en de bevindingen in het ontwerpdocument onder "Beslist op …".

## Wat F0 nog niet meet

- Laadrendement van de Enyaq aan 6, 9 en 16 A (review M4): dat vraagt de Škoda-API (SoC) en een paar nachten; kan met de logger-data (Alfen-energie) zodra de Škoda-koppeling er is.
- Gedrag van de Enyaq bij pauzeren en hervatten aan 6 A: komt in F1, via evcc zelf.
- Of 47005/47015 in flash staan: niet meetbaar; vragen aan Indevolt.
